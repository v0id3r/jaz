package mesh

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/charmbracelet/log"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// Inbound terminates session tunnels in Jaz itself, on Jaz's own listener:
// a connection that opens with a TLS handshake is a tunnel, anything else is
// the plain HTTP the listener always served.
type Inbound struct {
	svids   x509svid.Source
	bundles x509bundle.Source
	close   func() error
	log     *log.Logger
}

// NewInbound fetches Jaz's own SVID from the Workload API at addr.
func NewInbound(ctx context.Context, addr string, logger *log.Logger) (*Inbound, error) {
	source, err := workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(addr)))
	if err != nil {
		return nil, err
	}
	return &Inbound{svids: source, bundles: source, close: source.Close, log: logger.WithPrefix("mesh")}, nil
}

func (in *Inbound) Close() error { return in.close() }

// Listen splits ln: tunnels are served here with handler, as their sessions,
// and the returned listener yields the other connections.
func (in *Inbound) Listen(ln net.Listener, handler http.Handler) (net.Listener, error) {
	own, err := in.svids.GetX509SVID()
	if err != nil {
		return nil, err
	}
	t := &tunnels{
		td: own.ID.TrustDomain(), log: in.log,
		allow:   func(string) bool { return true },
		handler: func(string) http.Handler { return handler },
	}
	split := &splitListener{Listener: ln, plain: newChanListener(ln.Addr()), tls: newChanListener(ln.Addr())}
	split.plain.inner = ln
	server := &http.Server{
		Handler:           t,
		TLSConfig:         tlsconfig.MTLSServerConfig(in.svids, in.bundles, tlsconfig.AdaptMatcher(t.authorize)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go split.run()
	go func() {
		if err := server.ServeTLS(split.tls, "", ""); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
			in.log.Error("tunnel listener", "error", err)
		}
	}()
	in.log.Info("terminating session tunnels", "id", own.ID, "listen", ln.Addr())
	return split.plain, nil
}

// splitListener sorts accepted connections by their first byte: 0x16 opens
// a TLS handshake record.
type splitListener struct {
	net.Listener
	plain, tls *chanListener
}

const sniffTimeout = 30 * time.Second

func (s *splitListener) run() {
	defer s.plain.close()
	defer s.tls.close()
	for {
		conn, err := s.Accept()
		if err != nil {
			var temporary interface{ Temporary() bool }
			if errors.As(err, &temporary) && temporary.Temporary() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return
		}
		go s.sort(conn)
	}
}

func (s *splitListener) sort(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(sniffTimeout))
	r := bufio.NewReader(conn)
	first, err := r.Peek(1)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return
	}
	peeked := &peekedConn{Conn: conn, r: r}
	if first[0] == 0x16 {
		s.tls.deliver(peeked)
	} else {
		s.plain.deliver(peeked)
	}
}

// peekedConn replays the bytes read while sorting.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// chanListener hands out connections sorted to it; closing the shared
// underlying listener closes it too.
type chanListener struct {
	addr  net.Addr
	conns chan net.Conn
	once  sync.Once
	done  chan struct{}
	inner net.Listener
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{addr: addr, conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *chanListener) deliver(conn net.Conn) {
	select {
	case l.conns <- conn:
	case <-l.done:
		conn.Close()
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) close() { l.once.Do(func() { close(l.done) }) }

func (l *chanListener) Addr() net.Addr { return l.addr }

// Close is how the HTTP server stops; it stops the shared listener as well.
func (l *chanListener) Close() error {
	l.close()
	if l.inner != nil {
		return l.inner.Close()
	}
	return nil
}
