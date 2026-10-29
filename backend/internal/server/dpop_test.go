package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wins/jaz/backend/internal/deviceauth"
	sqlite "github.com/wins/jaz/backend/internal/storage/sqlite"
)

func TestDeviceRequestsCarryTheirKeyProofInHeaderOrWebSocketProtocol(t *testing.T) {
	store, err := sqlite.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	devices := deviceauth.New(store)
	devices.RequireProof = true
	public, key, _ := ed25519.GenerateKey(rand.Reader)
	sum := sha256.Sum256(public)
	registered, err := devices.RegisterApproved(deviceauth.Registration{
		Identity: deviceauth.DeviceIdentity{DeviceID: hex.EncodeToString(sum[:]), PublicKey: base64.RawURLEncoding.EncodeToString(public)},
		Profile:  deviceauth.DeviceProfile{Name: "Laptop", Kind: "browser"},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := (&Server{Store: store, Devices: devices, AuthKey: "root", Routes: Routes{
		{Pattern: "GET /v1/sessions/s1/terminal", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})},
	}}).Handler()
	proof := func(signer ed25519.PrivateKey) string {
		header, _ := json.Marshal(map[string]any{"typ": "dpop+jwt", "alg": "EdDSA", "jwk": map[string]string{"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(public)}})
		claims, _ := json.Marshal(map[string]any{"jti": rand.Text(), "htm": "GET", "htu": "wss://jaz.example/v1/sessions/s1/terminal", "iat": time.Now().Unix(), "ath": deviceauth.TokenHash(registered.Token)})
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
		return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
	}
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	for _, test := range []struct {
		name    string
		headers map[string]string
		code    string
	}{
		{"header", map[string]string{"Authorization": "DPoP " + registered.Token, "DPoP": proof(key)}, ""},
		{"websocket protocol", map[string]string{"Upgrade": "websocket", "Sec-WebSocket-Protocol": "jaz, jaz-auth." + registered.Token + ", jaz-dpop." + proof(key)}, ""},
		{"token alone keeps the token", map[string]string{"Authorization": "Bearer " + registered.Token}, "invalid_dpop_proof"},
		{"another key drops the token", map[string]string{"Authorization": "DPoP " + registered.Token, "DPoP": proof(otherKey)}, "unauthorized"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/sessions/s1/terminal?cols=80", nil)
			for name, value := range test.headers {
				req.Header.Set(name, value)
			}
			res := httptest.NewRecorder()
			srv.ServeHTTP(res, req)
			var body struct{ Code string }
			_ = json.NewDecoder(res.Body).Decode(&body)
			if (test.code == "") != (res.Code == http.StatusOK) || body.Code != test.code {
				t.Fatalf("status = %d, code = %q, want %q", res.Code, body.Code, test.code)
			}
		})
	}
}
