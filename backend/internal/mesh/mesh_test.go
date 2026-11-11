package mesh

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/log"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"golang.org/x/net/http2"

	"github.com/wins/jaz/backend/internal/mcpsession"
)

var td = spiffeid.RequireTrustDomainFromString("mesh.test")

type testCA struct {
	cert *x509.Certificate
	key  crypto.Signer
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mesh test"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
		URIs: []*url.URL{td.ID().URL()},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key}
}

func (ca *testCA) svid(t *testing.T, path string) *x509svid.SVID {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	id := spiffeid.RequireFromPath(td, path)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:        []*url.URL{id.URL()},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, key.Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &x509svid.SVID{ID: id, Certificates: []*x509.Certificate{cert}, PrivateKey: key}
}

// startMesh runs Jaz (upstream), a gateway and a session's capture listener;
// it returns the address an agent's captured connections arrive at.
func startMesh(t *testing.T, client *x509svid.SVID, upstream http.Handler) string {
	t.Helper()
	ca := newTestCA(t)
	bundle := x509bundle.FromX509Authorities(td, []*x509.Certificate{ca.cert})
	jaz := httptest.NewServer(upstream)
	t.Cleanup(jaz.Close)
	target, _ := url.Parse(jaz.URL)

	gatewaySVID := ca.svid(t, "/mesh-gateway")
	g := newGateway(target, td, log.Default())
	server := &http.Server{Handler: g, TLSConfig: tlsconfig.MTLSServerConfig(gatewaySVID, bundle, tlsconfig.AdaptMatcher(g.authorize))}
	gln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.ServeTLS(gln, "", "") }()
	t.Cleanup(func() { server.Close() })

	if client == nil {
		client = ca.svid(t, "/session/s1")
	} else {
		// Re-sign the requested identity with this mesh's CA.
		client = ca.svid(t, client.ID.Path())
	}
	ctx, cancel := context.WithCancel(context.Background())
	tun := &tunnel{
		session: "s1", gateway: gln.Addr().String(), svid: client, ctx: ctx, cancel: cancel, log: log.Default(),
		destination: func(net.Conn) (netip.AddrPort, error) { return netip.MustParseAddrPort(target.Host), nil },
	}
	transport := &http2.Transport{
		TLSClientConfig: tlsconfig.MTLSClientConfig(client, bundle, tlsconfig.AuthorizeID(gatewaySVID.ID)),
		DialTLSContext: func(ctx context.Context, _, addr string, cfg *tls.Config) (net.Conn, error) {
			return (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", addr)
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tun.ln, tun.transport = ln, transport
	go tun.accept(ln)
	t.Cleanup(tun.close)
	return ln.Addr().String()
}

func TestTunnelCarriesTheVerifiedSession(t *testing.T) {
	addr := startMesh(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Forwarded-For") != "" {
			t.Error("gateway added X-Forwarded-For; Jaz refuses forwarded agent requests")
		}
		body, _ := io.ReadAll(r.Body)
		_, _ = io.WriteString(w, r.Header.Get(mcpsession.HeaderName)+" "+r.URL.Path+" "+string(body))
	}))
	client := &http.Client{Timeout: 10 * time.Second}
	for i, claimed := range []string{"forged", ""} {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp/x", strings.NewReader("ping"))
		if claimed != "" {
			req.Header.Set(mcpsession.HeaderName, claimed)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(got) != "s1 /mcp/x ping" {
			t.Fatalf("request %d: Jaz saw %q", i, got)
		}
	}
}

func TestTunnelStreams(t *testing.T) {
	release := make(chan struct{})
	addr := startMesh(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "second\n")
	}))
	resp, err := http.Get("http://" + addr + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 6)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "first\n" {
		t.Fatalf("first event %q before the stream ended: %v", buf, err)
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "second\n" {
		t.Fatalf("rest %q", rest)
	}
}

func TestGatewayRefusesNonSessions(t *testing.T) {
	ca := newTestCA(t)
	addr := startMesh(t, ca.svid(t, "/jaz"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a non-session identity reached Jaz")
	}))
	client := &http.Client{Timeout: 10 * time.Second}
	if resp, err := client.Get("http://" + addr + "/mcp/x"); err == nil {
		resp.Body.Close()
		t.Fatalf("got %s, want the connection dropped", resp.Status)
	}
}

// TestTunnelEndsAfterConnectionClose: after a response net/http stops its
// background read with a past read deadline and waits for it, so the tunnel
// must honour read deadlines or a "Connection: close" exchange never ends.
func TestTunnelEndsAfterConnectionClose(t *testing.T) {
	addr := startMesh(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "GET /x HTTP/1.1\r\nHost: jaz\r\nConnection: close\r\n\r\n")
	got, err := io.ReadAll(conn)
	if err != nil || !strings.HasSuffix(string(got), "ok") {
		t.Fatalf("response %q, %v: the tunnel did not end", got, err)
	}
}
