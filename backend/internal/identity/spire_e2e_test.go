package identity

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/log"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// TestSPIREEndToEnd runs against a live SPIRE agent and a `jaz runner` with
// identities configured: JAZ_SPIRE_E2E="<delegate socket> <runner socket>
// <expected SPIFFE ID prefix>", e.g. "spiffe://example.org/session/".
func TestSPIREEndToEnd(t *testing.T) {
	spec := strings.Fields(os.Getenv("JAZ_SPIRE_E2E"))
	if len(spec) != 3 {
		t.Skip("JAZ_SPIRE_E2E not set")
	}
	p, err := New(Config{Delegate: spec[0], Runner: spec[1], Dir: t.TempDir()}, log.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	session := "e2e-" + randomHex(4)
	env, release, err := p.acquire(session)
	if err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(env[0], "SPIFFE_ENDPOINT_SOCKET=")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := workloadapi.New(ctx, workloadapi.WithAddr(addr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	want := spec[2] + session

	// A new entry reaches the agent with its next sync, a few seconds later.
	var got string
	for got == "" {
		svid, err := client.FetchX509SVID(ctx)
		if err == nil {
			got = svid.ID.String()
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("no X.509-SVID: %v", err)
		}
		time.Sleep(time.Second)
	}
	if got != want {
		t.Fatalf("X.509-SVID %s, want %s", got, want)
	}
	bundles, err := client.FetchX509Bundles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("X.509 bundles: %d trust domains", bundles.Len())

	jwt, err := client.FetchJWTSVID(ctx, jwtsvid.Params{Audience: "jaz-mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if jwt.ID.String() != want {
		t.Fatalf("JWT-SVID %s, want %s", jwt.ID, want)
	}
	validated, err := client.ValidateJWTSVID(ctx, jwt.Marshal(), "jaz-mcp")
	if err != nil {
		t.Fatal(err)
	}
	if validated.ID.String() != want {
		t.Fatalf("validated %s, want %s", validated.ID, want)
	}
	if _, err := client.ValidateJWTSVID(ctx, jwt.Marshal(), "other"); err == nil {
		t.Fatal("a token for jaz-mcp validated for another audience")
	}

	// Releasing the lease removes the entry; the delegate then gets nothing.
	release()
	again, err := New(Config{Delegate: spec[0], Runner: spec[1], Dir: t.TempDir()}, log.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	again.hold = func(string) (func(), error) { return func() {}, nil }
	env, stale, err := again.acquire(session)
	if err != nil {
		t.Fatal(err)
	}
	defer stale()
	addr = strings.TrimPrefix(env[0], "SPIFFE_ENDPOINT_SOCKET=")
	for start := time.Now(); ; time.Sleep(time.Second) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := workloadapi.FetchX509SVID(ctx, workloadapi.WithAddr(addr))
		cancel()
		if err != nil {
			t.Logf("%s after release: %v", time.Since(start).Round(time.Second), err)
			return
		}
		if time.Since(start) > 30*time.Second {
			t.Fatal("entry still issued 30s after release")
		}
	}
}
