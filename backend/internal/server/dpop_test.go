package server

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
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
	proxyKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	proxyPublic, _ := proxyKey.PublicKey.Bytes()
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kid": "k", "kty": "EC", "crv": "P-256",
			"x": base64.RawURLEncoding.EncodeToString(proxyPublic[1:33]), "y": base64.RawURLEncoding.EncodeToString(proxyPublic[33:])}}})
	}))
	t.Cleanup(jwks.Close)
	devices, err := deviceauth.NewProofMode(store, []string{"https://jaz.example"}, deviceauth.ProxyConfig{
		Header: "Teleport-Jwt-Assertion", JWKS: jwks.URL, Issuer: "teleport.example", Audience: "https://jaz.example", Users: []string{"alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
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
		{Pattern: "GET /v1/devices", Handler: http.HandlerFunc(devicesapi.NewHandler(devices, nil, serverconfig.Config{}).List)},
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
	proxyJWT := func(user string) string {
		h, _ := json.Marshal(map[string]any{"alg": "ES256", "kid": "k"})
		c, _ := json.Marshal(map[string]any{"iss": "teleport.example", "sub": user, "aud": "https://jaz.example", "exp": time.Now().Add(time.Hour).Unix()})
		signed := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
		digest := sha256.Sum256([]byte(signed))
		r, s, _ := ecdsa.Sign(rand.Reader, proxyKey, digest[:])
		return signed + "." + base64.RawURLEncoding.EncodeToString(append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...))
	}
	requestToken := func(grant, contentType, user string) (*httptest.ResponseRecorder, map[string]any) {
		form := url.Values{"grant_type": {grant}, "client_assertion_type": {deviceauth.ClientAssertionType}, "client_assertion": {sign(key,
			map[string]any{"typ": "client-authentication+jwt", "alg": "EdDSA"},
			map[string]any{"iss": deviceID, "sub": deviceID, "aud": "https://jaz.example", "jti": rand.Text(), "iat": time.Now().Unix(), "exp": time.Now().Add(30 * time.Second).Unix()},
		)}}
		req := httptest.NewRequest(http.MethodPost, "/v1/devices/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("DPoP", proof(key, "POST", "https://jaz.example/v1/devices/token", ""))
		if user != "" {
			req.Header.Set("Teleport-Jwt-Assertion", proxyJWT(user))
		}
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
	if res, body := requestToken("authorization_code", "application/x-www-form-urlencoded", "alice"); res.Code != http.StatusBadRequest || body["error"] != "unsupported_grant_type" {
		t.Fatalf("authorization_code grant = %d %v", res.Code, body)
	}
	if res, body := requestToken("client_credentials", "application/json", "alice"); res.Code != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("JSON token request = %d %v", res.Code, body)
	}
	if res, body := requestToken("client_credentials", "application/x-www-form-urlencoded", ""); res.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("token request around the auth proxy = %d %v", res.Code, body)
	}
	res, body := requestToken("client_credentials", "application/x-www-form-urlencoded; charset=utf-8", "alice")
	token, _ := body["access_token"].(string)
	if res.Code != http.StatusOK || token == "" || body["token_type"] != "DPoP" || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("token response = %d %v %v", res.Code, body, res.Header())
	}
	list := httptest.NewRequest(http.MethodGet, "/v1/devices", nil)
	list.Header.Set("Authorization", "DPoP "+token)
	list.Header.Set("DPoP", proof(key, "GET", "https://jaz.example/v1/devices", token))
	listed := httptest.NewRecorder()
	srv.ServeHTTP(listed, list)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"can_manage":true`) {
		t.Fatalf("devices as the proxy's owner = %d %s, want can_manage", listed.Code, listed.Body)
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
		{"root key under the DPoP scheme is just an unknown token", map[string]string{"Authorization": "DPoP root"}, "invalid_token", `error="invalid_token"`},
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

func TestTokenModeTakesDeviceTokensOnlyAsBearer(t *testing.T) {
	store, err := sqlite.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	devices := deviceauth.New(store)
	public, _, _ := ed25519.GenerateKey(rand.Reader)
	sum := sha256.Sum256(public)
	registered, err := devices.RegisterApproved(deviceauth.Registration{
		Identity: deviceauth.DeviceIdentity{DeviceID: hex.EncodeToString(sum[:]), PublicKey: base64.RawURLEncoding.EncodeToString(public)},
		Profile:  deviceauth.DeviceProfile{Name: "Laptop", Kind: "browser"},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := (&Server{Store: store, Devices: devices, AuthKey: "root", Routes: Routes{
		{Pattern: "GET /v1/sessions", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})},
	}}).Handler()
	for scheme, want := range map[string]int{"Bearer": http.StatusOK, "DPoP": http.StatusUnauthorized} {
		req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
		req.Header.Set("Authorization", scheme+" "+registered.Token)
		res := httptest.NewRecorder()
		srv.ServeHTTP(res, req)
		if res.Code != want {
			t.Errorf("%s device token = %d, want %d", scheme, res.Code, want)
		}
	}
}
