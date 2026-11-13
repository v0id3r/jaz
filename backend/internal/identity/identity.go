// Package identity gives the agent processes Jaz runs outside the sandbox the
// SPIFFE identity of their session. The runner registers the session with
// SPIRE for as long as a lease is held; Jaz, a delegate of the local SPIRE
// agent, fetches the session's SVIDs through the Delegated Identity API and
// serves them to the process on a SPIFFE Workload API socket of its own.
//
// Local processes share Jaz's uid, so one could open another session's
// socket: outside the sandbox the identity attributes, it does not isolate.
package identity

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/charmbracelet/log"
	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	delegatedidentityv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/agent/delegatedidentity/v1"
	"github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wins/jaz/backend/internal/sandbox"
)

type Config struct {
	// Delegate is the SPIRE agent's Delegated Identity API socket.
	Delegate string
	// Runner is the runner socket that registers session identities.
	Runner string
	// Dir holds the per-process Workload API sockets.
	Dir string
}

type Provider struct {
	cfg    Config
	conn   *grpc.ClientConn
	client delegatedidentityv1.DelegatedIdentityClient
	log    *log.Logger
	// hold registers the session with SPIRE until release; a seam for tests.
	hold func(session string) (func(), error)
}

func New(cfg Config, logger *log.Logger) (*Provider, error) {
	if cfg.Delegate == "" || cfg.Runner == "" || cfg.Dir == "" {
		return nil, fmt.Errorf("identity needs a delegate socket, a runner socket and a directory")
	}
	// Sockets of a previous Jaz process serve nobody.
	if err := os.RemoveAll(cfg.Dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient("unix://"+cfg.Delegate, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	p := &Provider{cfg: cfg, conn: conn, client: delegatedidentityv1.NewDelegatedIdentityClient(conn), log: logger}
	p.hold = func(session string) (func(), error) { return sandbox.HoldIdentity(cfg.Runner, session) }
	return p, nil
}

func (p *Provider) Close() error { return p.conn.Close() }

// Acquire serves the session's identity to one process; the process finds
// the socket through SPIFFE_ENDPOINT_SOCKET.
func (p *Provider) Acquire(session string) ([]string, func()) {
	env, release, err := p.acquire(session)
	if err != nil {
		p.log.Warn("session identity unavailable", "session", session, "error", err)
		return nil, func() {}
	}
	return env, release
}

func (p *Provider) acquire(session string) ([]string, func(), error) {
	unhold, err := p.hold(session)
	if err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(p.cfg.Dir, randomHex(8))
	if err := os.Mkdir(dir, 0o700); err != nil {
		unhold()
		return nil, nil, err
	}
	socket := filepath.Join(dir, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		unhold()
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	server := grpc.NewServer()
	workload.RegisterSpiffeWorkloadAPIServer(server, &workloadAPI{
		client:    p.client,
		selectors: []*types.Selector{{Type: "jaz", Value: "session:" + session}},
	})
	go func() { _ = server.Serve(listener) }()
	var once sync.Once
	release := func() {
		once.Do(func() {
			server.Stop()
			_ = os.RemoveAll(dir)
			unhold()
		})
	}
	return []string{"SPIFFE_ENDPOINT_SOCKET=unix://" + socket}, release, nil
}

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
