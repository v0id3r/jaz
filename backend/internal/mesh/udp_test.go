package mesh

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/log"
)

// TestMain reruns the tests with HTTP/2 extended CONNECT on, for the test
// gateway: Go reads the switch from GODEBUG once, at start.
func TestMain(m *testing.M) {
	if !strings.Contains(os.Getenv("GODEBUG"), "http2xconnect=1") {
		cmd := exec.Command(os.Args[0], os.Args[1:]...)
		cmd.Env = append(os.Environ(), "GODEBUG="+strings.TrimPrefix(os.Getenv("GODEBUG")+",http2xconnect=1", ","))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				os.Exit(ee.ExitCode())
			}
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// udpGateway is a connect-udp gateway like the broker's egress: it relays a
// stream's datagrams to the target in its path. It counts the streams it
// served and ended.
func udpGateway(t *testing.T) (open func(context.Context, netip.AddrPort) (io.ReadWriteCloser, error), opened, ended *atomic.Int32) {
	opened, ended = &atomic.Int32{}, &atomic.Int32{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Header.Get(":protocol") != connectUDP || r.Header.Get("Capsule-Protocol") != "?1" {
			http.Error(w, "not connect-udp", http.StatusBadRequest)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/.well-known/masque/udp/"), "/")
		up, err := net.Dial("udp", strings.ReplaceAll(parts[0], "%3A", ":")+":"+parts[1])
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer up.Close()
		opened.Add(1)
		defer ended.Add(1)
		w.Header().Set("Capsule-Protocol", "?1")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		rc.Flush()
		go func() {
			br := bufio.NewReader(r.Body)
			for {
				p, err := readDatagram(br)
				if err != nil {
					up.Close()
					return
				}
				up.Write(p)
			}
		}()
		buf := make([]byte, maxCapsule)
		for {
			n, err := up.Read(buf)
			if err != nil {
				return
			}
			if writeDatagram(w, buf[:n]) != nil || rc.Flush() != nil {
				return
			}
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	addr := strings.TrimPrefix(srv.URL, "https://")
	return func(ctx context.Context, dst netip.AddrPort) (io.ReadWriteCloser, error) {
		conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: roots, NextProtos: []string{"h2"}})
		if err != nil {
			return nil, err
		}
		return openUDP(ctx, conn, addr, dst)
	}, opened, ended
}

func udpEcho(t *testing.T) netip.AddrPort {
	echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			echo.WriteToUDP(buf[:n], from)
		}
	}()
	return echo.LocalAddr().(*net.UDPAddr).AddrPort()
}

// TestUDPRelay carries a session's datagrams over connect-udp and its
// replies back to where it sent from, one stream per flow, until the flow
// goes idle.
func TestUDPRelay(t *testing.T) {
	open, opened, ended := udpGateway(t)
	echo := udpEcho(t)
	// The session's socket; replies reach it from replyAs, here a plain socket.
	session, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	src := session.LocalAddr().(*net.UDPAddr).AddrPort()
	replyAs := func(netip.AddrPort) (net.PacketConn, error) { return net.ListenPacket("udp4", "127.0.0.1:0") }
	r := newUDPRelay("s1", log.New(io.Discard), 300*time.Millisecond, udpRetry, open, replyAs)
	defer r.close()

	read := func() string {
		session.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 2048)
		n, err := session.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		return string(buf[:n])
	}
	for _, msg := range []string{"one", "two", "three"} {
		r.packet(src, echo, []byte(msg))
		if got := read(); got != msg {
			t.Fatalf("echo: %q, want %q", got, msg)
		}
	}
	if opened.Load() != 1 {
		t.Fatalf("one flow opened %d streams", opened.Load())
	}
	// A second source is a second flow.
	other, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer other.Close()
	r.packet(other.LocalAddr().(*net.UDPAddr).AddrPort(), echo, []byte("x"))
	other.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, _ := other.Read(make([]byte, 16)); n != 1 || opened.Load() != 2 {
		t.Fatalf("second flow: %d bytes, %d streams", n, opened.Load())
	}
	// Idle flows end, and their streams with them.
	deadline := time.Now().Add(5 * time.Second)
	for ended.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if ended.Load() != 2 {
		t.Fatalf("idle flows left %d of 2 streams open", 2-ended.Load())
	}
	r.packet(src, echo, []byte("again"))
	if got := read(); got != "again" || opened.Load() != 3 {
		t.Fatalf("after idle: %q, %d streams", got, opened.Load())
	}
}

// TestUDPRelayRetries drops a flow's datagrams while its stream cannot open
// and tries again after udpRetry, not on every datagram.
func TestUDPRelayRetries(t *testing.T) {
	var tries atomic.Int32
	open := func(context.Context, netip.AddrPort) (io.ReadWriteCloser, error) {
		tries.Add(1)
		return nil, errors.New("refused")
	}
	r := newUDPRelay("s1", log.New(io.Discard), time.Minute, 200*time.Millisecond, open, nil)
	defer r.close()
	src, dst := netip.MustParseAddrPort("10.0.0.2:5000"), netip.MustParseAddrPort("192.0.2.1:443")
	for range 20 {
		r.packet(src, dst, []byte("x"))
		time.Sleep(5 * time.Millisecond)
	}
	if n := tries.Load(); n != 1 {
		t.Fatalf("%d tries within the retry delay", n)
	}
	time.Sleep(400 * time.Millisecond)
	r.packet(src, dst, []byte("x"))
	time.Sleep(50 * time.Millisecond)
	if n := tries.Load(); n != 2 {
		t.Fatalf("%d tries after the retry delay", n)
	}
}
