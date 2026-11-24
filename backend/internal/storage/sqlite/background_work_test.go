package sqlite

import (
	"testing"

	"github.com/wins/jaz/backend/internal/storage"
)

func TestBackgroundWorkShowsInLoadedAndListedSessions(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	session, err := store.CreateSession(storage.CreateSession{Slug: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	if session.BackgroundWork {
		t.Fatal("new session reports background work")
	}
	if err := store.SetBackgroundWork(session.ID, true); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.BackgroundWork {
		t.Fatal("loaded session misses its live background work")
	}
	listed, err := store.ListSessions(storage.SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !listed[0].BackgroundWork {
		t.Fatalf("listed sessions miss the background work: %+v", listed)
	}
	if err := store.SetBackgroundWork(session.ID, false); err != nil {
		t.Fatal(err)
	}
	if loaded, err = store.LoadSession(session.ID); err != nil || loaded.BackgroundWork {
		t.Fatalf("finished background work still reported: %v, %+v", err, loaded)
	}
}
