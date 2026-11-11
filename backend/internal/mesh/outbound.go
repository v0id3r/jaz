package mesh

import (
	"bufio"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/log"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"golang.org/x/net/http2"
)

// Config sets up capture in the runner. A session container's connections to
// the Capture destinations are redirected inside its network namespace to a
// listener the runner holds there, and tunnelled to the Gateway with the
// session's X.509-SVID; the tunnel leaves from the session's own address.
type Config struct {
	// Server is the SPIRE server's admin API socket, which mints the SVIDs.
	Server string
	// TrustDomain is the trust domain of the session IDs.
	TrustDomain string
	// Gateway is the gateway's host:port as seen from a session's network.
	Gateway string
	// GatewayID is the SPIFFE ID the gateway must present.
	GatewayID string
	// Capture lists the ip:port destinations that go through the mesh.
	Capture []string
	// Egress is the host:port of a second gateway, the agent broker's, that
	// takes every other TCP connection of a session. With it the session sends
	// nothing else but DNS to DNS and connections to Direct: other UDP, IPv6
	// and ICMP are dropped.
	Egress string
	// EgressID is the SPIFFE ID the egress gateway must present.
	EgressID string
	// DNS lists the resolvers (IPv4) sessions query directly, over port 53.
	DNS []string
	// Direct lists ip:port destinations sessions reach without a tunnel.
	Direct []string
	// Port is the capture listener's port on a session's loopback.
	Port int
	// TTL is the lifetime of a session's SVIDs; they renew at half of it.
	TTL time.Duration
	// Dir holds the OCI hook config and the hook socket, for root only.
	Dir string
}

func (c Config) Enabled() bool { return c.Gateway != "" }

const (
	containerAnnotation = "jaz.mesh.container"
	sessionAnnotation   = "jaz.mesh.session"
	dialTimeout         = 5 * time.Second
	bundleRefresh       = 5 * time.Minute
)

// Outbound runs the capture side of the mesh in the runner.
type Outbound struct {
	cfg       Config
	gatewayID spiffeid.ID
	egressID  spiffeid.ID
	capture   []netip.AddrPort
	direct    []netip.AddrPort
	dns       []netip.Addr
	issuer    *issuer
	bundles   *bundleCache
	log       *log.Logger

	mu      sync.Mutex
	tunnels map[string]*tunnel
}

func NewOutbound(cfg Config, logger *log.Logger) (*Outbound, error) {
	if cfg.Port == 0 {
		cfg.Port = 15001
	}
	if cfg.TTL == 0 {
		cfg.TTL = time.Hour
	}
	if cfg.Dir == "" {
		cfg.Dir = "/run/jaz-runner-mesh"
	}
	td, err := spiffeid.TrustDomainFromString(cfg.TrustDomain)
	if err != nil {
		return nil, fmt.Errorf("mesh trustDomain: %w", err)
	}
	gatewayID, err := spiffeid.FromString(cfg.GatewayID)
	if err != nil {
		return nil, fmt.Errorf("mesh gatewayID: %w", err)
	}
	if _, err := netip.ParseAddrPort(cfg.Gateway); err != nil {
		return nil, fmt.Errorf("mesh gateway: %w", err)
	}
	capture, err := addrPorts("capture", cfg.Capture)
	if err != nil {
		return nil, err
	}
	if len(capture) == 0 {
		return nil, errors.New("mesh capture is empty")
	}
	o := &Outbound{cfg: cfg, gatewayID: gatewayID, capture: capture, log: logger.WithPrefix("mesh"), tunnels: map[string]*tunnel{}}
	if cfg.Egress != "" {
		if _, err := netip.ParseAddrPort(cfg.Egress); err != nil {
			return nil, fmt.Errorf("mesh egress: %w", err)
		}
		if o.egressID, err = spiffeid.FromString(cfg.EgressID); err != nil {
			return nil, fmt.Errorf("mesh egressID: %w", err)
		}
		if o.direct, err = addrPorts("direct", cfg.Direct); err != nil {
			return nil, err
		}
		for _, s := range cfg.DNS {
			addr, err := netip.ParseAddr(s)
			if err != nil || !addr.Is4() {
				return nil, fmt.Errorf("mesh dns %q: want an IPv4 address", s)
			}
			o.dns = append(o.dns, addr)
		}
	} else if cfg.EgressID != "" || len(cfg.DNS) > 0 || len(cfg.Direct) > 0 {
		return nil, errors.New("mesh egressID, dns and direct need egress")
	}
	if o.issuer, err = newIssuer(cfg.Server, td, cfg.TTL); err != nil {
		return nil, err
	}
	o.bundles = &bundleCache{issuer: o.issuer}
	return o, nil
}

func addrPorts(name string, list []string) ([]netip.AddrPort, error) {
	out := make([]netip.AddrPort, 0, len(list))
	for _, dst := range list {
		addr, err := netip.ParseAddrPort(dst)
		if err != nil || !addr.Addr().Is4() {
			return nil, fmt.Errorf("mesh %s %q: want an IPv4 ip:port", name, dst)
		}
		out = append(out, addr)
	}
	return out, nil
}

// PodmanArgs are the global podman arguments and the `podman run` arguments
// that make podman call the capture hook for the container.
func (o *Outbound) PodmanArgs(name, session string) (global, run []string) {
	return []string{"--hooks-dir", filepath.Join(o.cfg.Dir, "hooks")},
		[]string{"--annotation", containerAnnotation + "=" + name, "--annotation", sessionAnnotation + "=" + session}
}

// Expect announces a container about to start for a session; its capture is
// set up when the hook reports it and torn down by the returned release.
func (o *Outbound) Expect(name, session string) func() {
	ctx, cancel := context.WithCancel(context.Background())
	t := &tunnel{
		name: name, session: session, gateway: o.cfg.Gateway, egress: o.cfg.Egress, jaz: o.capture,
		ctx: ctx, cancel: cancel, log: o.log,
		svid:        &sessionSVID{issuer: o.issuer, id: spiffeid.RequireFromPath(o.issuer.td, "/session/"+session)},
		destination: originalDst,
	}
	o.mu.Lock()
	o.tunnels[name] = t
	o.mu.Unlock()
	return func() {
		o.mu.Lock()
		delete(o.tunnels, name)
		o.mu.Unlock()
		t.close()
	}
}

// Start writes the hook config and serves the hook socket. binary is the jaz
// executable podman runs as the hook.
func (o *Outbound) Start(binary string) (func(), error) {
	hooks := filepath.Join(o.cfg.Dir, "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(o.cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	socket := filepath.Join(o.cfg.Dir, "hook.sock")
	hook, err := json.MarshalIndent(map[string]any{
		"version": "1.0.0",
		"hook":    map[string]any{"path": binary, "args": []string{"jaz", "mesh-hook", socket}, "timeout": 20},
		"when":    map[string]any{"annotations": map[string]string{`^` + strings.ReplaceAll(containerAnnotation, ".", `\.`) + `$`: ".+"}},
		"stages":  []string{"createRuntime"},
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(hooks, "jaz-mesh.json"), hook, 0o600); err != nil {
		return nil, err
	}
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go o.serveHook(conn)
		}
	}()
	o.log.Info("capture", "gateway", o.cfg.Gateway, "gatewayID", o.gatewayID, "capture", o.cfg.Capture, "egress", o.cfg.Egress, "egressID", o.egressID, "dns", o.cfg.DNS, "direct", o.cfg.Direct, "port", o.cfg.Port)
	return func() { ln.Close() }, nil
}

type hookRequest struct {
	Container string `json:"container"`
	Session   string `json:"session"`
	PID       int    `json:"pid"`
}

func (o *Outbound) serveHook(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	var req hookRequest
	err := json.NewDecoder(conn).Decode(&req)
	if err == nil {
		err = o.attach(req)
	}
	if err != nil {
		o.log.Warn("capture refused", "container", req.Container, "session", req.Session, "error", err)
		fmt.Fprintf(conn, "error: %v\n", err)
		return
	}
	o.log.Info("capture", "container", req.Container, "session", req.Session, "pid", req.PID)
	fmt.Fprintln(conn, "ok")
}

func (o *Outbound) attach(req hookRequest) error {
	o.mu.Lock()
	t, ok := o.tunnels[req.Container]
	o.mu.Unlock()
	if !ok || t.session != req.Session || req.PID <= 1 {
		return fmt.Errorf("container %q of session %q was not started by this runner", req.Container, req.Session)
	}
	// The first SVID is minted before the session starts: a session that
	// cannot get one does not run with its traffic uncaptured.
	if _, err := t.svid.GetX509SVID(); err != nil {
		return err
	}
	ns, err := enterNetns(req.PID)
	if err != nil {
		return err
	}
	ln, err := ns.listen(fmt.Sprintf("127.0.0.1:%d", o.cfg.Port))
	if err == nil {
		err = ns.capture(captureRules(o.capture, o.direct, o.dns, o.cfg.Port, o.cfg.Egress != ""))
	}
	if err != nil {
		if ln != nil {
			ln.Close()
		}
		ns.close()
		return err
	}
	var egress *http2.Transport
	if o.cfg.Egress != "" {
		egress = o.transport(t, ns, o.egressID)
	}
	return t.start(ns, ln, o.transport(t, ns, o.gatewayID), egress)
}

// transport tunnels from inside ns, as t's session, to a gateway that must
// present gateway.
func (o *Outbound) transport(t *tunnel, ns *netns, gateway spiffeid.ID) *http2.Transport {
	return &http2.Transport{
		TLSClientConfig: tlsconfig.MTLSClientConfig(t.svid, o.bundles, tlsconfig.AuthorizeID(gateway)),
		DialTLSContext: func(ctx context.Context, _, addr string, cfg *tls.Config) (net.Conn, error) {
			raw, err := ns.dial(addr)
			if err != nil {
				return nil, err
			}
			conn := tls.Client(raw, cfg)
			if err := conn.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			return conn, nil
		},
		ReadIdleTimeout: 30 * time.Second,
		PingTimeout:     10 * time.Second,
	}
}

// tunnel carries one container's captured connections: those to jaz go to
// the gateway, the rest to egress when there is one.
type tunnel struct {
	name, session, gateway string
	egress                 string
	jaz                    []netip.AddrPort
	svid                   x509svid.Source
	destination            func(net.Conn) (netip.AddrPort, error)
	ctx                    context.Context
	cancel                 context.CancelFunc
	log                    *log.Logger

	mu              sync.Mutex
	ns              *netns
	ln              net.Listener
	transport       *http2.Transport
	egressTransport *http2.Transport
}

func (t *tunnel) start(ns *netns, ln net.Listener, transport, egress *http2.Transport) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ctx.Err() != nil || t.ln != nil {
		ln.Close()
		ns.close()
		return errors.New("container already gone or captured")
	}
	t.ns, t.ln, t.transport, t.egressTransport = ns, ln, transport, egress
	go t.accept(ln)
	return nil
}

func (t *tunnel) accept(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go t.relay(conn)
	}
}

func (t *tunnel) relay(conn net.Conn) {
	defer conn.Close()
	dst, err := t.destination(conn)
	if err != nil {
		t.log.Warn("captured connection without destination", "session", t.session, "error", err)
		return
	}
	gateway, transport := t.gateway, t.transport
	if t.egress != "" && !slices.Contains(t.jaz, dst) {
		gateway, transport = t.egress, t.egressTransport
	}
	body, feed := io.Pipe()
	req := (&http.Request{
		Method: http.MethodConnect, URL: &url.URL{Scheme: "https", Host: gateway},
		Host: dst.String(), Header: http.Header{}, Body: body, ContentLength: -1,
	}).WithContext(t.ctx)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		feed.Close()
		t.log.Warn("tunnel", "session", t.session, "destination", dst, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		feed.Close()
		t.log.Warn("tunnel refused", "session", t.session, "destination", dst, "status", resp.Status)
		return
	}
	go func() {
		_, err := io.Copy(feed, conn)
		feed.CloseWithError(err)
	}()
	_, _ = io.Copy(conn, resp.Body)
}

func (t *tunnel) close() {
	t.cancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ln != nil {
		t.ln.Close()
	}
	if t.ns != nil {
		if t.egress != "" {
			// What the session sent past the tunnels, such as UDP or IPv6.
			if n, err := t.ns.dropped(); err != nil {
				t.log.Warn("dropped packets unknown", "session", t.session, "error", err)
			} else if n > 0 {
				t.log.Info("dropped packets", "session", t.session, "container", t.name, "packets", n)
			}
		}
		t.ns.close()
	}
	for _, tr := range []*http2.Transport{t.transport, t.egressTransport} {
		if tr != nil {
			tr.CloseIdleConnections()
		}
	}
}

// sessionSVID renews a session's SVID once half its lifetime has passed.
type sessionSVID struct {
	issuer *issuer
	id     spiffeid.ID

	mu      sync.Mutex
	current *x509svid.SVID
}

func (s *sessionSVID) GetX509SVID() (*x509svid.SVID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil && time.Until(s.current.Certificates[0].NotAfter) > s.issuer.ttl/2 {
		return s.current, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cert, err := s.issuer.mint(ctx, s.id)
	if err == nil {
		s.current, err = svidFromCertificate(s.id, cert)
	}
	if err != nil {
		if s.current != nil && time.Now().Before(s.current.Certificates[0].NotAfter) {
			return s.current, nil
		}
		return nil, err
	}
	return s.current, nil
}

func svidFromCertificate(id spiffeid.ID, cert *tls.Certificate) (*x509svid.SVID, error) {
	chain := make([]*x509.Certificate, 0, len(cert.Certificate))
	for _, der := range cert.Certificate {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		chain = append(chain, c)
	}
	return &x509svid.SVID{ID: id, Certificates: chain, PrivateKey: cert.PrivateKey.(crypto.Signer)}, nil
}

// bundleCache serves the trust domain's X.509 authorities, refetched from
// the server every few minutes.
type bundleCache struct {
	issuer *issuer

	mu      sync.Mutex
	bundle  *x509bundle.Bundle
	fetched time.Time
}

func (b *bundleCache) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	if td != b.issuer.td {
		return nil, fmt.Errorf("no bundle for trust domain %s", td)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bundle == nil || time.Since(b.fetched) > bundleRefresh {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		bundle, err := b.issuer.authorities(ctx)
		cancel()
		if err != nil && b.bundle == nil {
			return nil, err
		}
		if err == nil {
			b.bundle, b.fetched = bundle, time.Now()
		}
	}
	return b.bundle, nil
}

// Hook is `jaz mesh-hook <socket>`, the OCI createRuntime hook: it reports
// the new container's process to the runner, which captures its network
// before the session's first instruction runs. A failure stops the start.
func Hook(args []string, stdin io.Reader, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: jaz mesh-hook <runner hook socket>  (OCI state on stdin)")
		return 2
	}
	var state struct {
		PID         int               `json:"pid"`
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.NewDecoder(stdin).Decode(&state); err != nil {
		fmt.Fprintln(stderr, "jaz mesh-hook: OCI state:", err)
		return 1
	}
	conn, err := net.DialTimeout("unix", args[0], dialTimeout)
	if err != nil {
		fmt.Fprintln(stderr, "jaz mesh-hook:", err)
		return 1
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(18 * time.Second))
	req := hookRequest{Container: state.Annotations[containerAnnotation], Session: state.Annotations[sessionAnnotation], PID: state.PID}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		fmt.Fprintln(stderr, "jaz mesh-hook:", err)
		return 1
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if reply = strings.TrimSpace(reply); reply != "ok" {
		fmt.Fprintln(stderr, "jaz mesh-hook:", reply, err)
		return 1
	}
	return 0
}
