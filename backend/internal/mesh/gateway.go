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

type gateway struct {
	target *url.URL
	td     spiffeid.TrustDomain
	proxy  http.RoundTripper
	log    *log.Logger
}

func newGateway(target *url.URL, td spiffeid.TrustDomain, logger *log.Logger) *gateway {
	return &gateway{target: target, td: td, log: logger, proxy: &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: dialTimeout}).DialContext,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}}
}

// authorize admits session identities of the gateway's trust domain only.
func (g *gateway) authorize(id spiffeid.ID) error {
	if _, ok := sessionOf(id, g.td); !ok {
		return fmt.Errorf("%s is not a session of %s", id, g.td)
	}
	return nil
}

func sessionOf(id spiffeid.ID, td spiffeid.TrustDomain) (string, bool) {
	session, ok := strings.CutPrefix(id.Path(), "/session/")
	return session, ok && id.TrustDomain() == td && session != "" && !strings.Contains(session, "/")
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect || r.ProtoMajor != 2 || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "HBONE tunnels only", http.StatusMethodNotAllowed)
		return
	}
	id, err := x509svid.IDFromCert(r.TLS.PeerCertificates[0])
	session, ok := sessionOf(id, g.td)
	if err != nil || !ok {
		http.Error(w, "not a session", http.StatusForbidden)
		return
	}
	if r.Host != g.target.Host {
		g.log.Warn("tunnel refused", "session", session, "destination", r.Host)
		http.Error(w, "destination not served", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return
	}
	conn := newStreamConn(r.Body, w, rc, r.RemoteAddr)
	g.log.Info("tunnel", "session", session, "destination", r.Host)
	server := &http.Server{Handler: g.forward(session), ReadHeaderTimeout: 30 * time.Second}
	_ = server.Serve(&oneConnListener{conn: conn})
	<-conn.closed
}

// forward proxies one tunnel's requests to Jaz as the tunnel's session.
func (g *gateway) forward(session string) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(g.target)
			pr.Out.Header.Set(mcpsession.HeaderName, session)
		},
		Transport:     g.proxy,
		FlushInterval: -1,
		ErrorLog:      nil,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if claimed := r.Header.Get(mcpsession.HeaderName); claimed != "" && claimed != session {
			g.log.Warn("session header replaced", "session", session, "claimed", claimed)
		}
		g.log.Debug("request", "session", session, "method", r.Method, "path", r.URL.Path)
		proxy.ServeHTTP(w, r)
	})
}

// streamConn is the inside of an HBONE stream as a net.Conn: reads come from
// the request body, writes go out as response data, flushed at once.
type streamConn struct {
	body   io.ReadCloser
	w      io.Writer
	rc     *http.ResponseController
	remote string

	once   sync.Once
	closed chan struct{}
}

func newStreamConn(body io.ReadCloser, w io.Writer, rc *http.ResponseController, remote string) *streamConn {
	return &streamConn{body: body, w: w, rc: rc, remote: remote, closed: make(chan struct{})}
}

func (c *streamConn) Read(p []byte) (int, error) { return c.body.Read(p) }

func (c *streamConn) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if err == nil {
		err = c.rc.Flush()
	}
	return n, err
}

func (c *streamConn) Close() error {
	c.once.Do(func() {
		_ = c.body.Close()
		close(c.closed)
	})
	return nil
}

func (c *streamConn) LocalAddr() net.Addr              { return streamAddr("hbone") }
func (c *streamConn) RemoteAddr() net.Addr             { return streamAddr(c.remote) }
func (c *streamConn) SetDeadline(time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(time.Time) error  { return nil }
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
