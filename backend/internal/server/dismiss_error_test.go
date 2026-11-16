package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wins/jaz/backend/internal/sessionevents"
	"github.com/wins/jaz/backend/internal/storage"
	sqlitestore "github.com/wins/jaz/backend/internal/storage/sqlite"
)

func TestDismissSessionErrorClearsOnlyTerminalFailure(t *testing.T) {
	store, err := sqlitestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	session, err := store.CreateSession(storage.CreateSession{Slug: "dismiss-me"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionEvents(session.ID, sessionevents.Event{Type: "acp_error", Content: "provider failed"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSessionStatus(session.ID, storage.StatusError, "provider failed", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetThreadUnread(session.ID, true); err != nil {
		t.Fatal(err)
	}
	before, err := store.LoadSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}

	handler := (&Server{Store: store}).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+session.ID+"/dismiss-error", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("dismiss status = %d, body = %s", res.Code, res.Body.String())
	}

	after, err := store.LoadSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != storage.StatusIdle || after.Error != "" || after.Turn != nil {
		t.Fatalf("dismissed session = %#v", after)
	}
	if !after.Unread || !after.LastAttentionAt.Equal(before.LastAttentionAt) {
		t.Fatalf("dismiss changed unread or focus recency: before %#v after %#v", before, after)
	}
	events, err := store.LoadSessionEvents(session.ID)
	if err != nil || len(events) != 1 || events[0].Content != "provider failed" {
		t.Fatalf("dismiss changed transcript events: %#v, %v", events, err)
	}

	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/v1/sessions/"+session.ID+"/dismiss-error", nil))
	if res.Code != http.StatusConflict {
		t.Fatalf("repeat dismiss status = %d, want conflict", res.Code)
	}
}
