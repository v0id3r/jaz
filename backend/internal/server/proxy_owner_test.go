package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wins/jaz/backend/internal/deviceauth"
	devicesapi "github.com/wins/jaz/backend/internal/httpapi/devices"
	"github.com/wins/jaz/backend/internal/serverconfig"
	sqlite "github.com/wins/jaz/backend/internal/storage/sqlite"
)

func TestTrustedProxyOwnerPairsAndManagesWithoutHostCredential(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "EC", "crv": "P-256", "kid": "cluster",
			"x": base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))),
			"y": base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32))),
		}}})
	}))
	t.Cleanup(jwks.Close)
	store, err := sqlite.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	devices := deviceauth.New(store)
	existing, err := devices.RegisterApproved(testDeviceInfo("Desktop", "desktop", 1))
	if err != nil {
		t.Fatal(err)
	}
	handler := devicesapi.NewHandler(devices, store, serverconfig.Config{PublicURL: "https://jaz.example"})
	srv := httptest.NewServer((&Server{Store: store, Devices: devices, AuthKey: "root",
		DeviceProxy: deviceauth.NewProxy(deviceauth.ProxyConfig{Header: "Teleport-Jwt-Assertion", JWKS: jwks.URL, Issuer: "teleport.example", Audience: "http://jaz-web:8080", Users: []string{"aleksei"}}),
		Routes: Routes{
			{Pattern: "GET /v1/devices", Handler: http.HandlerFunc(handler.List)},
			{Pattern: "POST /v1/devices/pairing-requests", Handler: http.HandlerFunc(handler.CreatePairing)},
			{Pattern: "/v1/devices/pairing-requests/", Handler: http.HandlerFunc(handler.Pairing)},
		}}).Handler())
	t.Cleanup(srv.Close)
	assertion := ownerAssertion(t, key, "aleksei")
	stranger := ownerAssertion(t, key, "bot-jaz-agent")
	do := func(method, path, body string, headers map[string]string) map[string]any {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "jaz.example"
		req.Header.Set("X-Forwarded-Proto", "https")
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	pairAndPoll := func(seed byte, assertion string) map[string]any {
		identity := testDeviceInfo("Browser", "browser", seed).Identity
		created := do(http.MethodPost, "/v1/devices/pairing-requests", `{"device_id":"`+identity.DeviceID+`","public_key":"`+identity.PublicKey+`"}`,
			map[string]string{"Teleport-Jwt-Assertion": assertion})
		pairing, _ := created["pairing"].(map[string]any)
		secret, _ := created["pairing_secret"].(string)
		return do(http.MethodGet, "/v1/devices/pairing-requests/"+pairing["id"].(string), "", map[string]string{"X-Jaz-Pairing-Secret": secret})
	}
	if poll := pairAndPoll(2, assertion); poll["token"] == nil {
		t.Fatalf("an owner's pairing was not approved at once: %v", poll)
	}
	if poll := pairAndPoll(3, stranger); poll["token"] != nil {
		t.Fatalf("a proxy identity outside the allow list was approved: %v", poll)
	}
	bearer := map[string]string{"Authorization": "Bearer " + existing.Token}
	if list := do(http.MethodGet, "/v1/devices", "", bearer); list["can_manage"] != false || list["current_device_id"] != existing.Device.ID {
		t.Fatalf("a device without the owner assertion may manage devices: %v", list)
	}
	bearer["Teleport-Jwt-Assertion"] = assertion
	if list := do(http.MethodGet, "/v1/devices", "", bearer); list["can_manage"] != true || list["current_device_id"] != existing.Device.ID {
		t.Fatalf("the owner's device lost management or its device identity: %v", list)
	}
	devices.RequireUser = true
	bearer["Teleport-Jwt-Assertion"] = stranger
	if list := do(http.MethodGet, "/v1/devices", "", bearer); list["code"] != "unauthorized" {
		t.Fatalf("another proxy identity used the owner's device: %v", list)
	}
	delete(bearer, "Teleport-Jwt-Assertion")
	if list := do(http.MethodGet, "/v1/devices", "", bearer); list["code"] != "user_required" {
		t.Fatalf("a request that bypassed the proxy was served: %v", list)
	}
}

func ownerAssertion(t *testing.T, key *ecdsa.PrivateKey, user string) string {
	t.Helper()
	now := time.Now().Unix()
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": "cluster"})
	payload, _ := json.Marshal(map[string]any{"iss": "teleport.example", "aud": "http://jaz-web:8080", "sub": user, "exp": now + 60, "nbf": now - 60})
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signed))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...))
}
