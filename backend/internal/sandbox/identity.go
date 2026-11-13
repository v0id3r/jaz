package sandbox

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/charmbracelet/log"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	entryv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/server/entry/v1"
	"github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// RunnerIdentity registers session identities with a SPIRE server. A session
// owns spiffe://<trust domain>/session/<id> while one of its containers runs
// or while Jaz holds an identity lease for one of its local processes.
type RunnerIdentity struct {
	// Server is the SPIRE server's admin API socket.
	Server string
	// Parent is the SPIFFE ID of the SPIRE agent on this host.
	Parent string
	// WorkloadAPI is the host directory holding the agent's Workload API
	// socket, api.sock, mounted into every session container.
	WorkloadAPI string
	// FederatesWith lists trust domains whose bundles sessions receive.
	FederatesWith []string
}

const (
	// sessionHint marks the entries the runner owns, so a restarted runner
	// can remove the ones its predecessor left behind.
	sessionHint       = "jaz-session"
	sessionLabel      = "jaz.session"
	workloadAPIDir    = "/run/spire/agent"
	delegatedSelector = "jaz"
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validSessionID(id string) error {
	if !sessionIDPattern.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	return nil
}

func (c RunnerIdentity) enabled() bool { return c.Server != "" }

// identities keeps one SPIRE entry per session and kind for as long as any
// lease on it is held.
type identities struct {
	client    entryv1.EntryClient
	conn      *grpc.ClientConn
	parent    *types.SPIFFEID
	td        spiffeid.TrustDomain
	federates []string
	log       *log.Logger

	mu     sync.Mutex
	leases map[leaseKey]*entryLease
}

type leaseKey struct {
	session string
	// delegated entries are issued to Jaz on behalf of a local process;
	// the others to a runner container carrying the session label.
	delegated bool
}

type entryLease struct {
	ready chan struct{}
	id    string
	err   error
	refs  int
}

func newIdentities(cfg RunnerIdentity, logger *log.Logger) (*identities, error) {
	parent, err := spiffeid.FromString(cfg.Parent)
	if err != nil {
		return nil, fmt.Errorf("identity parent: %w", err)
	}
	for _, td := range cfg.FederatesWith {
		if _, err := spiffeid.TrustDomainFromString(td); err != nil {
			return nil, fmt.Errorf("identity federatesWith: %w", err)
		}
	}
	conn, err := grpc.NewClient("unix://"+cfg.Server, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &identities{
		client:    entryv1.NewEntryClient(conn),
		conn:      conn,
		parent:    &types.SPIFFEID{TrustDomain: parent.TrustDomain().Name(), Path: parent.Path()},
		td:        parent.TrustDomain(),
		federates: cfg.FederatesWith,
		log:       logger,
		leases:    map[leaseKey]*entryLease{},
	}, nil
}

// acquire registers the session's entry unless a lease already holds it; the
// returned release drops the entry with the last lease.
func (s *identities) acquire(ctx context.Context, session string, delegated bool) (func(), error) {
	if err := validSessionID(session); err != nil {
		return nil, err
	}
	key := leaseKey{session: session, delegated: delegated}
	s.mu.Lock()
	lease, ok := s.leases[key]
	if !ok {
		lease = &entryLease{ready: make(chan struct{})}
		s.leases[key] = lease
	}
	lease.refs++
	s.mu.Unlock()
	if !ok {
		lease.id, lease.err = s.create(ctx, session, delegated)
		close(lease.ready)
	}
	<-lease.ready
	if lease.err != nil {
		s.drop(key, lease)
		return nil, lease.err
	}
	var once sync.Once
	return func() { once.Do(func() { s.drop(key, lease) }) }, nil
}

func (s *identities) drop(key leaseKey, lease *entryLease) {
	s.mu.Lock()
	lease.refs--
	last := lease.refs == 0
	if last {
		delete(s.leases, key)
	}
	s.mu.Unlock()
	if last && lease.id != "" {
		s.delete(lease.id)
	}
}

func (s *identities) create(ctx context.Context, session string, delegated bool) (string, error) {
	selectors := []*types.Selector{
		{Type: "docker", Value: "label:" + runnerLabelKey + ":1"},
		{Type: "docker", Value: "label:" + sessionLabel + ":" + session},
	}
	if delegated {
		selectors = []*types.Selector{{Type: delegatedSelector, Value: "session:" + session}}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := s.client.BatchCreateEntry(ctx, &entryv1.BatchCreateEntryRequest{Entries: []*types.Entry{{
		ParentId:      s.parent,
		SpiffeId:      &types.SPIFFEID{TrustDomain: s.td.Name(), Path: "/session/" + session},
		Selectors:     selectors,
		FederatesWith: s.federates,
		Hint:          sessionHint,
	}}})
	if err != nil {
		return "", fmt.Errorf("register session identity: %w", err)
	}
	if len(resp.Results) != 1 {
		return "", fmt.Errorf("register session identity: %d results", len(resp.Results))
	}
	result := resp.Results[0]
	switch codes.Code(result.Status.GetCode()) {
	case codes.OK, codes.AlreadyExists:
		// An entry left by a crashed runner is adopted and removed with this lease.
		s.log.Info("session identity", "session", session, "delegated", delegated, "entry", result.Entry.GetId())
		return result.Entry.GetId(), nil
	default:
		return "", fmt.Errorf("register session identity: %s", result.Status.GetMessage())
	}
}

func (s *identities) delete(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.client.BatchDeleteEntry(ctx, &entryv1.BatchDeleteEntryRequest{Ids: []string{id}}); err != nil {
		s.log.Warn("remove session identity", "entry", id, "error", err)
	}
}

// removeStale deletes session entries a previous runner left; no lease
// survives a runner restart.
func (s *identities) removeStale() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var ids []string
	token := ""
	for {
		resp, err := s.client.ListEntries(ctx, &entryv1.ListEntriesRequest{
			Filter:     &entryv1.ListEntriesRequest_Filter{ByHint: wrapperspb.String(sessionHint)},
			OutputMask: &types.EntryMask{},
			PageSize:   500,
			PageToken:  token,
		})
		if err != nil {
			s.log.Warn("list stale session identities", "error", err)
			return
		}
		for _, entry := range resp.Entries {
			ids = append(ids, entry.Id)
		}
		if token = resp.NextPageToken; token == "" {
			break
		}
	}
	if len(ids) == 0 {
		return
	}
	s.log.Info("removing stale session identities", "count", len(ids))
	if _, err := s.client.BatchDeleteEntry(ctx, &entryv1.BatchDeleteEntryRequest{Ids: ids}); err != nil {
		s.log.Warn("remove stale session identities", "error", err)
	}
}
