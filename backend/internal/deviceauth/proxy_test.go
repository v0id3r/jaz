package deviceauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProxyOwnerRequiresSignedIdentityOfAllowedUserOrRole(t *testing.T) {
	signer, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "EC", "crv": "P-256", "kid": "cluster",
			"x": base64.RawURLEncoding.EncodeToString(signer.X.FillBytes(make([]byte, 32))),
			"y": base64.RawURLEncoding.EncodeToString(signer.Y.FillBytes(make([]byte, 32))),
		}}})
	}))
	t.Cleanup(jwks.Close)
	proxy := NewProxy(ProxyConfig{JWKS: jwks.URL, Issuer: "teleport.example", Audience: "http://jaz-web:8080", Users: []string{"aleksei"}, Roles: []string{"jaz-owner"}})
	now := time.Now().Unix()
	claims := func(change func(map[string]any)) map[string]any {
		c := map[string]any{"iss": "teleport.example", "aud": "http://jaz-web:8080", "sub": "aleksei", "exp": now + 60, "nbf": now - 60, "roles": []string{"access"}}
		if change != nil {
			change(c)
		}
		return c
	}
	for _, test := range []struct {
		name  string
		token string
		owner bool
	}{
		{"allowed user", signProxyToken(t, signer, "cluster", claims(nil)), true},
		{"allowed role", signProxyToken(t, signer, "cluster", claims(func(c map[string]any) { c["sub"] = "bot-jaz"; c["roles"] = []string{"jaz-owner"} })), true},
		{"audience list", signProxyToken(t, signer, "cluster", claims(func(c map[string]any) { c["aud"] = []string{"other", "http://jaz-web:8080"} })), true},
		{"other user", signProxyToken(t, signer, "cluster", claims(func(c map[string]any) { c["sub"] = "bot-jaz-agent" })), false},
		{"other app", signProxyToken(t, signer, "cluster", claims(func(c map[string]any) { c["aud"] = "http://other:8080" })), false},
		{"other cluster", signProxyToken(t, signer, "cluster", claims(func(c map[string]any) { c["iss"] = "evil.example" })), false},
		{"expired", signProxyToken(t, signer, "cluster", claims(func(c map[string]any) { c["exp"] = now - 1 })), false},
		{"not yet valid", signProxyToken(t, signer, "cluster", claims(func(c map[string]any) { c["nbf"] = now + 60 })), false},
		{"forged signature", signProxyToken(t, stranger, "cluster", claims(nil)), false},
		{"unknown key", signProxyToken(t, signer, "rotated", claims(nil)), false},
		{"missing", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if test.token != "" {
				req.Header.Set("Teleport-Jwt-Assertion", test.token)
			}
			if got := proxy.Owner(req); got != test.owner {
				t.Fatalf("Owner() = %v, want %v", got, test.owner)
			}
		})
	}
	disabled := NewProxy(ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Teleport-Jwt-Assertion", signProxyToken(t, signer, "cluster", claims(nil)))
	if disabled != nil || disabled.Owner(req) {
		t.Fatal("a proxy without JWKS must trust nobody")
	}
}

func signProxyToken(t *testing.T, key *ecdsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": kid})
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signed))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...))
}
