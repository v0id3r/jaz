package deviceauth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sqlite "github.com/wins/jaz/backend/internal/storage/sqlite"
)

func TestDeviceProofBindsRequestsAndRegistrationsToTheDeviceKey(t *testing.T) {
	store, err := sqlite.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := New(store)
	service.RequireProof = true
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	registered, err := service.RegisterApproved(proofRegistration(key, "Laptop"))
	if err != nil {
		t.Fatal(err)
	}
	ath := TokenHash(registered.Token)
	claims := func(change func(map[string]any)) map[string]any {
		c := map[string]any{"jti": rand.Text(), "htm": "GET", "htu": "https://jaz.example/v1/sessions", "iat": now.Unix(), "ath": ath}
		if change != nil {
			change(c)
		}
		return c
	}
	replayed := signDeviceProof(t, key, key, claims(nil))
	for _, test := range []struct {
		name  string
		proof string
		want  error
	}{
		{"valid", replayed, nil},
		{"replayed", replayed, ErrProofInvalid},
		{"origin rewritten by the proxy", signDeviceProof(t, key, key, claims(func(c map[string]any) { c["htu"] = "http://10.0.0.1:8080/v1/sessions" })), nil},
		{"other method", signDeviceProof(t, key, key, claims(func(c map[string]any) { c["htm"] = "POST" })), ErrProofInvalid},
		{"other path", signDeviceProof(t, key, key, claims(func(c map[string]any) { c["htu"] = "https://jaz.example/v1/devices" })), ErrProofInvalid},
		{"other token", signDeviceProof(t, key, key, claims(func(c map[string]any) { c["ath"] = TokenHash("jaz_other") })), ErrProofInvalid},
		{"stale", signDeviceProof(t, key, key, claims(func(c map[string]any) { c["iat"] = now.Add(-2 * time.Minute).Unix() })), ErrProofInvalid},
		{"from the future", signDeviceProof(t, key, key, claims(func(c map[string]any) { c["iat"] = now.Add(2 * time.Minute).Unix() })), ErrProofInvalid},
		{"oversized jti", signDeviceProof(t, key, key, claims(func(c map[string]any) { c["jti"] = strings.Repeat("j", 65) })), ErrProofInvalid},
		{"missing", "", ErrProofInvalid},
		{"signed by another key", signDeviceProof(t, stranger, key, claims(nil)), ErrUnauthorized},
		{"another device key", signDeviceProof(t, stranger, stranger, claims(nil)), ErrUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.Authenticate(registered.Token, Proof{JWT: test.proof, Method: "GET", Path: "/v1/sessions"}, SeenInfo{})
			if !errors.Is(err, test.want) || (test.want == nil && err != nil) {
				t.Fatalf("Authenticate() error = %v, want %v", err, test.want)
			}
		})
	}
	now = now.Add(90 * time.Second)
	early := signDeviceProof(t, key, key, claims(func(c map[string]any) { c["iat"] = now.Add(proofMaxAge).Unix() }))
	if _, err := service.Authenticate(registered.Token, Proof{JWT: early, Method: "GET", Path: "/v1/sessions"}, SeenInfo{}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(proofMaxAge)
	if _, err := service.Authenticate(registered.Token, Proof{JWT: early, Method: "GET", Path: "/v1/sessions"}, SeenInfo{}); !errors.Is(err, ErrProofInvalid) {
		t.Fatalf("a still-fresh proof was replayable after the spent set rotated: %v", err)
	}
	newcomer := proofRegistration(stranger, "Phone")
	if _, _, err := service.CreatePairing(newcomer); !errors.Is(err, ErrProofInvalid) {
		t.Fatalf("pairing without a key proof = %v", err)
	}
	newcomer.Proof = Proof{Method: "POST", Path: "/v1/devices/pairing-requests", JWT: signDeviceProof(t, stranger, stranger, map[string]any{
		"jti": rand.Text(), "htm": "POST", "htu": "https://jaz.example/v1/devices/pairing-requests", "iat": now.Unix(),
	})}
	if _, _, err := service.CreatePairing(newcomer); err != nil {
		t.Fatalf("pairing with a key proof = %v", err)
	}
}

func proofRegistration(key ed25519.PrivateKey, name string) Registration {
	public := key.Public().(ed25519.PublicKey)
	sum := sha256.Sum256(public)
	return Registration{
		Identity: DeviceIdentity{DeviceID: hex.EncodeToString(sum[:]), PublicKey: base64.RawURLEncoding.EncodeToString(public)},
		Profile:  DeviceProfile{Name: name, Kind: "browser"},
	}
}

func signDeviceProof(t *testing.T, signer, claimed ed25519.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]any{"typ": "dpop+jwt", "alg": "EdDSA", "jwk": map[string]string{
		"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(claimed.Public().(ed25519.PublicKey)),
	}})
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
}

func TestDeviceProofAcceptsP256Keys(t *testing.T) {
	store, err := sqlite.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := New(store)
	service.RequireProof = true
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	stranger, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	public, _ := key.PublicKey.Bytes()
	sum := sha256.Sum256(public)
	registered, err := service.RegisterApproved(Registration{
		Identity: DeviceIdentity{DeviceID: hex.EncodeToString(sum[:]), PublicKey: base64.RawURLEncoding.EncodeToString(public)},
		Profile:  DeviceProfile{Name: "Safari", Kind: "browser"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sign := func(signer, claimed *ecdsa.PrivateKey, alg string) string {
		point, _ := claimed.PublicKey.Bytes()
		header, _ := json.Marshal(map[string]any{"typ": "dpop+jwt", "alg": alg, "jwk": map[string]string{
			"kty": "EC", "crv": "P-256", "x": base64.RawURLEncoding.EncodeToString(point[1:33]), "y": base64.RawURLEncoding.EncodeToString(point[33:]),
		}})
		payload, _ := json.Marshal(map[string]any{"jti": rand.Text(), "htm": "GET", "htu": "https://jaz.example/v1/sessions", "iat": time.Now().Unix(), "ath": TokenHash(registered.Token)})
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
		digest := sha256.Sum256([]byte(signed))
		r, s, _ := ecdsa.Sign(rand.Reader, signer, digest[:])
		return signed + "." + base64.RawURLEncoding.EncodeToString(append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...))
	}
	for _, test := range []struct {
		name  string
		proof string
		want  error
	}{
		{"valid", sign(key, key, "ES256"), nil},
		{"alg not matching the key", sign(key, key, "EdDSA"), ErrProofInvalid},
		{"signed by another key", sign(stranger, key, "ES256"), ErrUnauthorized},
		{"another device key", sign(stranger, stranger, "ES256"), ErrUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.Authenticate(registered.Token, Proof{JWT: test.proof, Method: "GET", Path: "/v1/sessions"}, SeenInfo{})
			if !errors.Is(err, test.want) || (test.want == nil && err != nil) {
				t.Fatalf("Authenticate() error = %v, want %v", err, test.want)
			}
		})
	}
}
