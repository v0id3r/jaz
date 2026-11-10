package mesh

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/log"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"golang.org/x/net/http2"

	"github.com/wins/jaz/backend/internal/mcpsession"
)

// Jaz's listener keeps serving plain HTTP and serves tunnels on the same
// port, as the sessions their certificates name.
func TestInboundSplitsTunnelsFromPlainHTTP(t *testing.T) {
	ca := newTestCA(t)
	bundle := x509bundle.FromX509Authorities(td, []*x509.Certificate{ca.cert})
	jazSVID := ca.svid(t, "/jaz")
	in := &Inbound{svids: jazSVID, bundles: bundle, close: func() error { return nil }, log: log.Default()}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "session="+Session(r.Context())+" header="+r.Header.Get(mcpsession.HeaderName))
	})
	plain, err := in.Listen(ln, handler)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(plain) }()
	t.Cleanup(func() { server.Close() })
	addr := ln.Addr().String()

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + addr + "/health")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "session= header=" {
		t.Fatalf("plain request saw %q", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := ca.svid(t, "/session/s2")
	tun := &tunnel{
		session: "s2", gateway: addr, svid: session, ctx: ctx, cancel: cancel, log: log.Default(),
		destination: func(net.Conn) (netip.AddrPort, error) { return netip.MustParseAddrPort(addr), nil },
		transport: &http2.Transport{
			TLSClientConfig: tlsconfig.MTLSClientConfig(session, bundle, tlsconfig.AuthorizeID(jazSVID.ID)),
			DialTLSContext: func(ctx context.Context, _, addr string, cfg *tls.Config) (net.Conn, error) {
				return (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", addr)
			},
		},
	}
	capture, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tun.ln = capture
	go tun.accept(capture)
	t.Cleanup(tun.close)
	req, _ := http.NewRequest(http.MethodGet, "http://"+capture.Addr().String()+"/mcp/jaztools", nil)
	req.Header.Set(mcpsession.HeaderName, "forged")
	resp, err = (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(string(got), "session=s2 header=s2") {
		t.Fatalf("tunnelled request saw %q", got)
	}
}
