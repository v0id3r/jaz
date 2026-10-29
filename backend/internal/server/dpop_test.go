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
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wins/jaz/backend/internal/deviceauth"
	devicesapi "github.com/wins/jaz/backend/internal/httpapi/devices"
	"github.com/wins/jaz/backend/internal/serverconfig"
	sqlite "github.com/wins/jaz/backend/internal/storage/sqlite"
)

func TestProofModeIssuesAccessTokensAndRequiresTheirProof(t *testing.T) {
	store, err := sqlite.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	devices := deviceauth.New(store)
	devices.RequireProof = true
	devices.Origins = []string{"https://jaz.example"}
	public, key, _ := ed25519.GenerateKey(rand.Reader)
	sum := sha256.Sum256(public)
	deviceID := hex.EncodeToString(sum[:])
	if _, err := devices.RegisterApproved(deviceauth.Registration{
		Identity: deviceauth.DeviceIdentity{DeviceID: deviceID, PublicKey: base64.RawURLEncoding.EncodeToString(public)},
		Profile:  deviceauth.DeviceProfile{Name: "Laptop", Kind: "browser"},
	}); err != nil {
		t.Fatal(err)
	}
	srv := (&Server{Store: store, Devices: devices, AuthKey: "root", Routes: Routes{
		{Pattern: "POST /v1/devices/token", Handler: http.HandlerFunc(devicesapi.NewHandler(devices, nil, serverconfig.Config{}).Token)},
		{Pattern: "GET /v1/sessions/s1/terminal", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})},
	}}).Handler()
	sign := func(signer ed25519.PrivateKey, header, claims map[string]any) string {
		h, _ := json.Marshal(header)
		c, _ := json.Marshal(claims)
		signed := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
		return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
	}
	proof := func(signer ed25519.PrivateKey, method, target, token string) string {
		claims := map[string]any{"jti": rand.Text(), "htm": method, "htu": target, "iat": time.Now().Unix()}
		if token != "" {
			claims["ath"] = deviceauth.TokenHash(token)
		}
		return sign(signer, map[string]any{"typ": "dpop+jwt", "alg": "EdDSA", "jwk": map[string]string{"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(public)}}, claims)
	}
	requestToken := func(grant, contentType string) (*httptest.ResponseRecorder, map[string]any) {
		form := url.Values{"grant_type": {grant}, "client_assertion_type": {deviceauth.ClientAssertionType}, "client_assertion": {sign(key,
			map[string]any{"typ": "client-authentication+jwt", "alg": "EdDSA"},
			map[string]any{"iss": deviceID, "sub": deviceID, "aud": "https://jaz.example", "jti": rand.Text(), "iat": time.Now().Unix(), "exp": time.Now().Add(30 * time.Second).Unix()},
		)}}
		req := httptest.NewRequest(http.MethodPost, "/v1/devices/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("DPoP", proof(key, "POST", "https://jaz.example/v1/devices/token", ""))
		res := httptest.NewRecorder()
		srv.ServeHTTP(res, req)
		var body map[string]any
		_ = json.NewDecoder(res.Body).Decode(&body)
		return res, body
	}
	health := httptest.NewRecorder()
	srv.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if !strings.Contains(health.Body.String(), `"device_proof":true`) {
		t.Fatalf("health = %s, want device_proof", health.Body)
	}
	if res, body := requestToken("authorization_code", "application/x-www-form-urlencoded"); res.Code != http.StatusBadRequest || body["error"] != "unsupported_grant_type" {
		t.Fatalf("authorization_code grant = %d %v", res.Code, body)
	}
	if res, body := requestToken("client_credentials", "application/json"); res.Code != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("JSON token request = %d %v", res.Code, body)
	}
	res, body := requestToken("client_credentials", "application/x-www-form-urlencoded; charset=utf-8")
	token, _ := body["access_token"].(string)
	if res.Code != http.StatusOK || token == "" || body["token_type"] != "DPoP" || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("token response = %d %v %v", res.Code, body, res.Header())
	}

	target := "wss://jaz.example/v1/sessions/s1/terminal"
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	for _, test := range []struct {
		name      string
		headers   map[string]string
		code      string
		challenge string
	}{
		{"header", map[string]string{"Authorization": "DPoP " + token, "DPoP": proof(key, "GET", target, token)}, "", ""},
		{"websocket protocol", map[string]string{"Upgrade": "websocket", "Sec-WebSocket-Protocol": "jaz, jaz-auth." + token + ", jaz-dpop." + proof(key, "GET", target, token)}, "", ""},
		{"token alone keeps the token", map[string]string{"Authorization": "DPoP " + token}, "invalid_dpop_proof", `error="invalid_dpop_proof"`},
		{"bound token as bearer", map[string]string{"Authorization": "Bearer " + token, "DPoP": proof(key, "GET", target, token)}, "unauthorized", `error="invalid_token"`},
		{"another key drops the token", map[string]string{"Authorization": "DPoP " + token, "DPoP": proof(otherKey, "GET", target, token)}, "unauthorized", `error="invalid_token"`},
		{"unknown token asks for a new one", map[string]string{"Authorization": "DPoP jaz_gone", "DPoP": proof(key, "GET", target, "jaz_gone")}, "invalid_token", `error="invalid_token"`},
		{"no credentials", nil, "unauthorized", `DPoP algs="EdDSA ES256"`},
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
			if challenge := res.Header().Get("WWW-Authenticate"); !strings.HasSuffix(challenge, test.challenge) || test.challenge != "" && !strings.HasPrefix(challenge, `DPoP algs="EdDSA ES256"`) {
				t.Fatalf("WWW-Authenticate = %q, want %q", challenge, test.challenge)
			}
		})
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/s1/terminal", nil)
	req.Header.Set("Authorization", "DPoP "+token)
	req.Header.Add("DPoP", proof(key, "GET", target, token))
	req.Header.Add("DPoP", proof(key, "GET", target, token))
	res = httptest.NewRecorder()
	srv.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized || !strings.Contains(res.Body.String(), "invalid_dpop_proof") {
		t.Fatalf("two DPoP headers = %d %s, want invalid_dpop_proof (RFC 9449 §4.3)", res.Code, res.Body)
	}
}
