package jsonstore

import (
	"testing"

	"github.com/wins/jaz/backend/internal/storage"
)

func TestDismissSessionErrorRequiresIdleTerminalFailure(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	terminal, err := store.CreateSession(storage.CreateSession{Slug: "terminal"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSessionStatus(terminal.ID, storage.StatusError, "failed", terminal.LastAttentionAt); err != nil {
		t.Fatal(err)
	}
	dismissed, err := store.DismissSessionError(terminal.ID)
	if err != nil || !dismissed {
		t.Fatalf("dismissed = %v, err = %v", dismissed, err)
	}
	terminal, err = store.LoadSession(terminal.ID)
	if err != nil || terminal.Status != storage.StatusIdle || terminal.Error != "" {
		t.Fatalf("terminal = %#v, err = %v", terminal, err)
	}

	dismissed, err = store.DismissSessionError(terminal.ID)
	if err != nil || dismissed {
		t.Fatalf("repeat dismiss = %v, err = %v", dismissed, err)
	}
}
