package deviceauth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProxyIdentityRequiresASignatureFromTheProxyKeys(t *testing.T) {
	signer, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaSigner, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "EC", "crv": "P-256", "kid": "cluster",
			"x": base64.RawURLEncoding.EncodeToString(signer.X.FillBytes(make([]byte, 32))),
			"y": base64.RawURLEncoding.EncodeToString(signer.Y.FillBytes(make([]byte, 32))),
		}, {
			"kty": "RSA", "kid": "rsa",
			"n": base64.RawURLEncoding.EncodeToString(rsaSigner.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rsaSigner.E)).Bytes()),
		}}})
	}))
	t.Cleanup(jwks.Close)
	proxy := NewProxy(ProxyConfig{Header: "Teleport-Jwt-Assertion", JWKS: jwks.URL, Issuer: "teleport.example", Audience: "http://jaz-web:8080",
		RoleClaim: "roles", Users: []string{"aleksei"}, Roles: []string{"jaz-owner"}})
	now := time.Now().Unix()
	claims := func(change func(map[string]any)) map[string]any {
		c := map[string]any{"iss": "teleport.example", "aud": "http://jaz-web:8080", "sub": "aleksei", "exp": now + 60, "nbf": now - 60, "roles": []string{"access"}}
		if change != nil {
			change(c)
		}
		return c
	}
	owner := Identity{User: "aleksei@teleport.example", Owner: true}
	for _, test := range []struct {
		name  string
		token string
		want  Identity
	}{
		{"allowed user", signProxyToken(t, signer, "cluster", claims(nil)), owner},
		{"RS256", signProxyToken(t, rsaSigner, "rsa", claims(nil)), owner},
		{"allowed role", signProxyToken(t, signer, "cluster", claims(func(c map[string]any) { c["sub"] = "bot-jaz"; c["roles"] = []string{"jaz-owner"} })), Identity{User: "bot-jaz@teleport.example", Owner: true}},
		{"audience list", signProxyToken(t, signer, "cluster", claims(func(c map[string]any) { c["aud"] = []string{"other", "http://jaz-web:8080"} })), owner},
		{"other user", signProxyToken(t, signer, "cluster", claims(func(c map[string]any) { c["sub"] = "bot-jaz-agent" })), Identity{User: "bot-jaz-agent@teleport.example"}},
		{"other app", signProxyToken(t, signer, "cluster", claims(func(c map[string]any) { c["aud"] = "http://other:8080" })), Identity{}},
		{"other cluster", signProxyToken(t, signer, "cluster", claims(func(c map[string]any) { c["iss"] = "evil.example" })), Identity{}},
		{"expired", signProxyToken(t, signer, "cluster", claims(func(c map[string]any) { c["exp"] = now - 1 })), Identity{}},
		{"not yet valid", signProxyToken(t, signer, "cluster", claims(func(c map[string]any) { c["nbf"] = now + 60 })), Identity{}},
		{"forged signature", signProxyToken(t, stranger, "cluster", claims(nil)), Identity{}},
		{"key of another algorithm", signProxyToken(t, signer, "rsa", claims(nil)), Identity{}},
		{"unknown key", signProxyToken(t, signer, "rotated", claims(nil)), Identity{}},
		{"missing", "", Identity{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if test.token != "" {
				req.Header.Set("Teleport-Jwt-Assertion", test.token)
			}
			if got := proxy.Identify(req); got != test.want {
				t.Fatalf("Identify() = %+v, want %+v", got, test.want)
			}
		})
	}
	disabled := NewProxy(ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Teleport-Jwt-Assertion", signProxyToken(t, signer, "cluster", claims(nil)))
	if disabled != nil || disabled.Identify(req) != (Identity{}) {
		t.Fatal("a proxy without JWKS must vouch for nobody")
	}
}

func signProxyToken(t *testing.T, key crypto.Signer, kid string, claims map[string]any) string {
	t.Helper()
	alg := "ES256"
	if _, ok := key.(*rsa.PrivateKey); ok {
		alg = "RS256"
	}
	header, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT", "kid": kid})
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signed))
	var sig []byte
	switch key := key.(type) {
	case *ecdsa.PrivateKey:
		r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		sig = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	case *rsa.PrivateKey:
		if sig, err = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:]); err != nil {
			t.Fatal(err)
		}
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}
