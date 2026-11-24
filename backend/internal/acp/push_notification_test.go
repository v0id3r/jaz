package acp_test

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wins/jaz/backend/internal/acp"
	"github.com/wins/jaz/backend/internal/mobilepush"
	"github.com/wins/jaz/backend/internal/storage"
)

// recordingWebPush is the web push service's sender; it keeps what would have
// gone to the browser.
type recordingWebPush struct {
	mu      sync.Mutex
	bodies  []string
	session []string
}

func (r *recordingWebPush) Enabled() bool     { return true }
func (r *recordingWebPush) PublicKey() string { return "key" }
func (r *recordingWebPush) PublicURL() string { return "https://jaz.example" }

func (r *recordingWebPush) Send(_ context.Context, _ mobilepush.WebPushSubscription, payload any) error {
	body, _ := payload.(map[string]any)
	r.mu.Lock()
	defer r.mu.Unlock()
	text, _ := body["body"].(string)
	session, _ := body["session_id"].(string)
	r.bodies = append(r.bodies, text)
	r.session = append(r.session, session)
	return nil
}

func (r *recordingWebPush) told(session, text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.bodies {
		if r.session[i] == session && strings.Contains(r.bodies[i], text) {
			return true
		}
	}
	return false
}

// The turn the agent begins and ends by itself once its background work
// finished tells the person by web push, by the same rules as any turn's end.
func TestAPushTellsOfTheRunTheAgentBeginsAndEndsItself(t *testing.T) {
	store, root := newTestStore(t)
	jaz := startJaz(t, testContext(t), store, root, workState())

	apple, err := mobilepush.NewApple(mobilepush.Config{})
	if err != nil {
		t.Fatal(err)
	}
	push := &recordingWebPush{}
	service := mobilepush.NewService(store, apple, push, jaz.manager.AwaitsAnswer)
	if _, err := store.SaveApprovedDevice(storage.SaveApprovedDevice{ID: "phone", Name: "Phone", ApprovedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	var subscription mobilepush.WebPushSubscription
	subscription.Endpoint = "https://push.example/subscription"
	subscription.Keys.P256DH = base64.RawURLEncoding.EncodeToString(make([]byte, 65))
	subscription.Keys.Auth = base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	if err := service.RegisterWebPush("phone", subscription); err != nil {
		t.Fatal(err)
	}
	service.Start(jaz.ctx, jaz.events, func(error) {})

	spawned, err := jaz.manager.Spawn(jaz.ctx, acp.SpawnRequest{Slug: "solo", Title: "Solo", Directory: jaz.root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jaz.manager.Send(jaz.ctx, acp.SendRequest{Session: spawned.SessionID, Message: "start background work suite", Completion: acp.CompletionInline}); err != nil {
		t.Fatal(err)
	}
	// The prompt's own turn ends with the work still running in the background.
	jaz.eventually("the prompting turn ending", func() bool {
		session, err := store.LoadSession(spawned.SessionID)
		return err == nil && session.Status == storage.StatusIdle
	})

	// The work finishes with no turn running; the agent's own run about it
	// begins and ends.
	jaz.step("suite")
	jaz.step("suite-run")
	jaz.running(spawned.SessionID)
	jaz.step("suite-done")

	jaz.eventually("the push about the run's reply", func() bool {
		return push.told(spawned.SessionID, "suite finished")
	})
}
