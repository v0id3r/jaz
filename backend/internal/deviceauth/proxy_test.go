package deviceauth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testIssuer, testAudience = "proxy.example", "https://jaz.example"

// newTestProxy serves a JWKS with an ES256 key "ec" and an RS256 key "rsa",
// and signs proxy JWTs with them.
func newTestProxy(t *testing.T) (ProxyConfig, func(kid string, claims map[string]any) string, *int) {
	t.Helper()
	ec := newTestKey(t, "ES256")
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{
		{"kid": "ec", "kty": "EC", "crv": "P-256", "x": ec.jwk["x"], "y": ec.jwk["y"]},
		{"kid": "rsa", "kty": "RSA", "n": b64(rsaKey.N.Bytes()), "e": b64(big.NewInt(int64(rsaKey.E)).Bytes())},
	}})
	fetches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		_, _ = w.Write(jwks)
	}))
	t.Cleanup(server.Close)
	sign := func(kid string, claims map[string]any) string {
		alg, _ := claims["alg"].(string)
		delete(claims, "alg")
		h, _ := json.Marshal(map[string]any{"alg": alg, "kid": kid})
		c, _ := json.Marshal(claims)
		signed := b64(h) + "." + b64(c)
		sig := ec.sign([]byte(signed))
		if kid == "rsa" {
			digest := sha256.Sum256([]byte(signed))
			sig, _ = rsa.SignPKCS1v15(nil, rsaKey, crypto.SHA256, digest[:])
		}
		return signed + "." + b64(sig)
	}
	return ProxyConfig{Header: "Proxy-Jwt", JWKS: server.URL, Issuer: testIssuer, Audience: testAudience}, sign, &fetches
}

func TestProxyJWTsAreVerifiedAgainstTheIssuerKeys(t *testing.T) {
	proxy, sign, fetches := newTestProxy(t)
	service, now := newProofService(t, proxy)
	claims := func(alg string, change func(map[string]any)) map[string]any {
		c := map[string]any{"alg": alg, "iss": testIssuer, "sub": "alice", "aud": testAudience, "exp": now().Add(time.Hour).Unix()}
		if change != nil {
			change(c)
		}
		return c
	}
	for _, test := range []struct {
		name  string
		token string
		valid bool
	}{
		{"ES256", sign("ec", claims("ES256", nil)), true},
		{"RS256", sign("rsa", claims("RS256", nil)), true},
		{"audience list", sign("ec", claims("ES256", func(c map[string]any) { c["aud"] = []string{"other", testAudience} })), true},
		{"another issuer", sign("ec", claims("ES256", func(c map[string]any) { c["iss"] = "evil.example" })), false},
		{"another audience", sign("ec", claims("ES256", func(c map[string]any) { c["aud"] = "https://other.example" })), false},
		{"expired", sign("ec", claims("ES256", func(c map[string]any) { c["exp"] = now().Unix() })), false},
		{"without exp", sign("ec", claims("ES256", func(c map[string]any) { delete(c, "exp") })), false},
		{"not yet valid", sign("ec", claims("ES256", func(c map[string]any) { c["nbf"] = now().Add(time.Minute).Unix() })), false},
		{"without subject", sign("ec", claims("ES256", func(c map[string]any) { delete(c, "sub") })), false},
		{"RSA key named as ES256", sign("rsa", claims("ES256", nil)), false},
		{"unsigned", sign("ec", claims("none", nil)), false},
		{"unknown key", sign("gone", claims("ES256", nil)), false},
		{"missing", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			subject, _, err := service.users.verify(test.token, now())
			if test.valid != (err == nil && subject == "alice") || err != nil && !errors.Is(err, ErrUserInvalid) {
				t.Fatalf("verify() = %q, %v; want valid %v", subject, err, test.valid)
			}
		})
	}
	if *fetches != 1 {
		t.Fatalf("JWKS fetched %d times, want once a minute even for unknown keys", *fetches)
	}
	unreachable := &jwtIssuer{jwks: "http://127.0.0.1:1", client: http.DefaultClient}
	if _, _, err := unreachable.verify(sign("ec", claims("ES256", nil)), now()); err == nil || errors.Is(err, ErrUserInvalid) {
		t.Fatalf("verify() with an unreachable JWKS = %v, want a server error", err)
	}
}

func TestProxyUserGetsTheDeviceTokensAndKeepsTheDevice(t *testing.T) {
	proxy, sign, _ := newTestProxy(t)
	service, now := newProofService(t, proxy)
	key := newTestKey(t, "EdDSA")
	if _, err := service.RegisterApproved(key.registration("Laptop")); err != nil {
		t.Fatal(err)
	}
	issue := func(user string, expiresIn time.Duration) (AccessToken, error) {
		proxyJWT := ""
		if user != "" {
			proxyJWT = sign("ec", map[string]any{"alg": "ES256", "iss": testIssuer, "sub": user, "aud": testAudience, "exp": now().Add(expiresIn).Unix()})
		}
		assertion := signJWS(key, map[string]any{"alg": key.alg}, map[string]any{
			"iss": key.id, "sub": key.id, "aud": testOrigin, "jti": rand.Text(), "iat": now().Unix(), "exp": now().Add(30 * time.Second).Unix(),
		})
		return service.IssueAccessToken(assertion, Proof{JWT: tokenProof(key, now()), Method: "POST", Path: "/v1/devices/token"}, proxyJWT)
	}
	if _, err := issue("", 0); !errors.Is(err, ErrUserInvalid) {
		t.Fatalf("token without a proxy identity = %v", err)
	}
	if token, err := issue("alice", time.Hour); err != nil || token.ExpiresIn != int(accessTokenTTL/time.Second) {
		t.Fatalf("alice's first token = %+v, %v", token, err)
	}
	if device, _ := service.store.LoadDevice(key.id); device.User != "alice@"+testIssuer {
		t.Fatalf("device user = %q, want alice bound on first use", device.User)
	}
	if token, err := issue("alice", 2*time.Minute); err != nil || token.ExpiresIn != 120 {
		t.Fatalf("token for a proxy identity expiring sooner = %+v, %v", token, err)
	}
	if _, err := issue("bob", time.Hour); !errors.Is(err, ErrUserInvalid) {
		t.Fatalf("token for another user = %v", err)
	}
	if _, err := service.RegisterApproved(key.registration("Laptop")); err != nil {
		t.Fatal(err)
	}
	if _, err := issue("bob", time.Hour); err != nil {
		t.Fatalf("token for another user after approving the device again = %v", err)
	}
}

func TestProofModeNeedsACompleteProxy(t *testing.T) {
	if _, err := NewProofMode(nil, []string{testOrigin}, ProxyConfig{Header: "Proxy-Jwt", JWKS: "https://proxy.example/jwks"}); err == nil {
		t.Fatal("NewProofMode() accepted a proxy without issuer and audience")
	}
}
