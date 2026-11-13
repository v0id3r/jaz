package mesh

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	"github.com/wins/jaz/backend/internal/mcpsession"
)

// ServeGateway is `jaz mesh-gateway`: it terminates session tunnels and
// forwards the HTTP inside them to Jaz as the session the client certificate
// names, replacing whatever session header the agent sent.
func ServeGateway(args []string) int {
	fs := flag.NewFlagSet("mesh-gateway", flag.ContinueOnError)
	listen := fs.String("listen", ":15008", "tunnel listener address")
	workloadAPI := fs.String("workload-api", "unix:///run/spire/agent/api.sock", "SPIFFE Workload API address for the gateway's own SVID")
	upstream := fs.String("upstream", "", "Jaz base URL, e.g. http://10.0.0.1:5299; tunnels may only target its host:port")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger := log.Default().WithPrefix("mesh-gateway")
	target, err := url.Parse(*upstream)
	if err != nil || target.Host == "" || target.Scheme != "http" {
		logger.Error("upstream must be an http:// URL", "upstream", *upstream)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	start, cancel := context.WithTimeout(ctx, 30*time.Second)
	source, err := workloadapi.NewX509Source(start, workloadapi.WithClientOptions(workloadapi.WithAddr(*workloadAPI)))
	cancel()
	if err != nil {
		logger.Error("own SVID", "error", err)
		return 1
	}
	defer source.Close()
	own, err := source.GetX509SVID()
	if err != nil {
		logger.Error("own SVID", "error", err)
		return 1
	}
	g := newGateway(target, own.ID.TrustDomain(), logger)
	server := &http.Server{
		Handler:           g,
		TLSConfig:         tlsconfig.MTLSServerConfig(source, source, tlsconfig.AdaptMatcher(g.authorize)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		logger.Error("listen", "error", err)
		return 1
	}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	logger.Info("serving", "listen", *listen, "id", own.ID, "upstream", target)
	if err := server.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("serve", "error", err)
		return 1
	}
	return 0
}

// tunnels terminates HBONE tunnels from the sessions of a trust domain and
// serves the HTTP inside each tunnel with the handler for its session. The
// session comes from the client certificate alone.
type tunnels struct {
	td      spiffeid.TrustDomain
	allow   func(destination string) bool
	handler func(session string) http.Handler
	log     *log.Logger
}

// authorize admits session identities of the trust domain only.
func (t *tunnels) authorize(id spiffeid.ID) error {
	if _, ok := sessionOf(id, t.td); !ok {
		return fmt.Errorf("%s is not a session of %s", id, t.td)
	}
	return nil
}

func sessionOf(id spiffeid.ID, td spiffeid.TrustDomain) (string, bool) {
	session, ok := strings.CutPrefix(id.Path(), "/session/")
	return session, ok && id.TrustDomain() == td && session != "" && !strings.Contains(session, "/")
}

func (t *tunnels) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect || r.ProtoMajor != 2 || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "HBONE tunnels only", http.StatusMethodNotAllowed)
		return
	}
	id, err := x509svid.IDFromCert(r.TLS.PeerCertificates[0])
	session, ok := sessionOf(id, t.td)
	if err != nil || !ok {
		http.Error(w, "not a session", http.StatusForbidden)
		return
	}
	if !t.allow(r.Host) {
		t.log.Warn("tunnel refused", "session", session, "destination", r.Host)
		http.Error(w, "destination not served", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return
	}
	conn := newStreamConn(r.Body, w, rc, r.RemoteAddr)
	t.log.Info("tunnel", "session", session, "destination", r.Host)
	server := &http.Server{Handler: t.withSession(session), ReadHeaderTimeout: 30 * time.Second}
	_ = server.Serve(&oneConnListener{conn: conn})
	<-conn.closed
}

// withSession names the tunnel's session in every request, replacing
// whatever session header the agent sent.
func (t *tunnels) withSession(session string) http.Handler {
	next := t.handler(session)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if claimed := r.Header.Get(mcpsession.HeaderName); claimed != "" && claimed != session {
			t.log.Warn("session header replaced", "session", session, "claimed", claimed)
		}
		r.Header.Set(mcpsession.HeaderName, session)
		next.ServeHTTP(w, r.WithContext(WithSession(r.Context(), session)))
	})
}

type sessionKey struct{}

// WithSession marks ctx as a request that came through a tunnel of session.
func WithSession(ctx context.Context, session string) context.Context {
	return context.WithValue(ctx, sessionKey{}, session)
}

// Session is the session a request arrived as through a mesh tunnel; empty
// for any other request.
func Session(ctx context.Context) string {
	session, _ := ctx.Value(sessionKey{}).(string)
	return session
}

// newGateway serves tunnels to target only and forwards their requests there.
func newGateway(target *url.URL, td spiffeid.TrustDomain, logger *log.Logger) *tunnels {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) { pr.SetURL(target) },
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: dialTimeout}).DialContext,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		},
		FlushInterval: -1,
	}
	return &tunnels{
		td: td, log: logger,
		allow:   func(destination string) bool { return destination == target.Host },
		handler: func(string) http.Handler { return proxy },
	}
}

// streamConn is the inside of an HBONE stream as a net.Conn: reads come from
// the request body, writes go out as response data, flushed at once. The body
// is read by a goroutine of its own, so a read deadline can end a Read without
// losing data: after each response net/http stops its background read with a
// past deadline and waits for it, and a no-op deadline would hang the tunnel.
type streamConn struct {
	body   io.ReadCloser
	w      io.Writer
	rc     *http.ResponseController
	remote string
	chunks chan streamChunk

	once   sync.Once
	closed chan struct{}

	rest []byte // of the last chunk, for the next Read
	err  error  // that ended the body, once rest is read

	mu       sync.Mutex
	deadline chan struct{} // closed once the read deadline passes
	timer    *time.Timer
	gen      int // of the deadline the timer is for
}

type streamChunk struct {
	b   []byte
	err error
}

func newStreamConn(body io.ReadCloser, w io.Writer, rc *http.ResponseController, remote string) *streamConn {
	c := &streamConn{
		body: body, w: w, rc: rc, remote: remote, chunks: make(chan streamChunk),
		closed: make(chan struct{}), deadline: make(chan struct{}),
	}
	go c.pump()
	return c
}

func (c *streamConn) pump() {
	for {
		b := make([]byte, 32<<10)
		n, err := c.body.Read(b)
		select {
		case c.chunks <- streamChunk{b[:n], err}:
		case <-c.closed:
			return
		}
		if err != nil {
			return
		}
	}
}

func (c *streamConn) Read(p []byte) (int, error) {
	if len(c.rest) == 0 {
		if c.err != nil {
			return 0, c.err
		}
		c.mu.Lock()
		deadline := c.deadline
		c.mu.Unlock()
		select {
		case ch := <-c.chunks:
			c.rest, c.err = ch.b, ch.err
			if len(c.rest) == 0 {
				return 0, c.err
			}
		case <-deadline:
			return 0, os.ErrDeadlineExceeded
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
	n := copy(p, c.rest)
	c.rest = c.rest[n:]
	return n, nil
}

func (c *streamConn) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if err == nil {
		err = c.rc.Flush()
	}
	return n, err
}

func (c *streamConn) Close() error {
	c.once.Do(func() {
		close(c.closed)
		_ = c.body.Close()
	})
	return nil
}

// SetReadDeadline ends a pending Read when the deadline passes.
func (c *streamConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		c.timer.Stop()
	}
	c.gen++
	select {
	case <-c.deadline:
		c.deadline = make(chan struct{})
	default:
	}
	switch {
	case t.IsZero():
	case time.Until(t) <= 0:
		close(c.deadline)
	default:
		gen := c.gen
		c.timer = time.AfterFunc(time.Until(t), func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.gen == gen {
				close(c.deadline)
			}
		})
	}
	return nil
}

func (c *streamConn) LocalAddr() net.Addr              { return streamAddr("hbone") }
func (c *streamConn) RemoteAddr() net.Addr             { return streamAddr(c.remote) }
func (c *streamConn) SetDeadline(t time.Time) error    { return c.SetReadDeadline(t) }
func (c *streamConn) SetWriteDeadline(time.Time) error { return nil }

type streamAddr string

func (a streamAddr) Network() string { return "hbone" }
func (a streamAddr) String() string  { return string(a) }

// oneConnListener hands out a single connection, then reports itself closed.
type oneConnListener struct {
	mu   sync.Mutex
	conn net.Conn
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return nil, net.ErrClosed
	}
	conn := l.conn
	l.conn = nil
	return conn, nil
}

func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return streamAddr("hbone") }
