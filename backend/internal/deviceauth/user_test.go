package deviceauth

import (
	"errors"
	"testing"

	sqlitestore "github.com/wins/jaz/backend/internal/storage/sqlite"
)

func TestDevicesBelongToTheProxyIdentityThatPairedThem(t *testing.T) {
	store, err := sqlitestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := New(store)
	service.RequireUser = true
	legacy, err := service.RegisterApproved(testClientInfo("Laptop", "desktop", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.CreatePairing(testClientInfo("Phone", "mobile", 2)); !errors.Is(err, ErrUserRequired) {
		t.Fatalf("pairing without an identity: %v", err)
	}
	reg := testClientInfo("Phone", "mobile", 2)
	reg.User = "aleksei@teleport.example"
	pairing, secret, err := service.CreatePairing(reg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApprovePairing(pairing.ID); err != nil {
		t.Fatal(err)
	}
	paired, err := service.PollPairing(pairing.ID, secret)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, token, user string
		want              error
	}{
		{"paired device, another user", paired.Token, "mallory@teleport.example", ErrUnauthorized},
		{"paired device, its user", paired.Token, "aleksei@teleport.example", nil},
		{"paired device, no identity", paired.Token, "", ErrUserRequired},
		{"older device binds to its first user", legacy.Token, "aleksei@teleport.example", nil},
		{"older device, then another user", legacy.Token, "mallory@teleport.example", ErrUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.Authenticate(test.token, Proof{}, Identity{User: test.user}, SeenInfo{}); !errors.Is(err, test.want) {
				t.Fatalf("Authenticate() = %v, want %v", err, test.want)
			}
		})
	}
}
