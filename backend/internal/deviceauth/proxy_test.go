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

// testProxy serves a JWKS of the keys in published, from ES256 keys "ec" and
// "ec2" and an RS256 key "rsa", or 503 while down, and signs proxy JWTs.
type testProxy struct {
	config    ProxyConfig
	published []string
	down      bool
	fetches   int
	jwks      map[string]map[string]string
	signers   map[string]func([]byte) []byte
}

func newTestProxy(t *testing.T) *testProxy {
	t.Helper()
	b64 := base64.RawURLEncoding.EncodeToString
	p := &testProxy{published: []string{"ec", "rsa"}, jwks: map[string]map[string]string{}, signers: map[string]func([]byte) []byte{}}
	for _, kid := range []string{"ec", "ec2"} {
		key := newTestKey(t, "ES256")
		p.jwks[kid] = map[string]string{"kid": kid, "kty": "EC", "crv": "P-256", "x": key.jwk["x"], "y": key.jwk["y"]}
		p.signers[kid] = key.sign
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.jwks["rsa"] = map[string]string{"kid": "rsa", "kty": "RSA", "n": b64(rsaKey.N.Bytes()), "e": b64(big.NewInt(int64(rsaKey.E)).Bytes())}
	p.signers["rsa"] = func(signed []byte) []byte {
		digest := sha256.Sum256(signed)
		sig, _ := rsa.SignPKCS1v15(nil, rsaKey, crypto.SHA256, digest[:])
		return sig
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.fetches++
		if p.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		keys := []map[string]string{}
		for _, kid := range p.published {
			keys = append(keys, p.jwks[kid])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	t.Cleanup(server.Close)
	p.config = ProxyConfig{Header: "Proxy-Jwt", JWKS: server.URL, Issuer: testIssuer, Audience: testAudience}
	return p
}

// sign signs claims, whose "alg" goes to the header, with the key kid, or the
// "ec" key for a kid the proxy does not have.
func (p *testProxy) sign(kid string, claims map[string]any) string {
	b64 := base64.RawURLEncoding.EncodeToString
	alg, _ := claims["alg"].(string)
	delete(claims, "alg")
	h, _ := json.Marshal(map[string]any{"alg": alg, "kid": kid})
	c, _ := json.Marshal(claims)
	signed := b64(h) + "." + b64(c)
	signer, ok := p.signers[kid]
	if !ok {
		signer = p.signers["ec"]
	}
	return signed + "." + b64(signer([]byte(signed)))
}

func userClaims(alg string, now time.Time, change func(map[string]any)) map[string]any {
	c := map[string]any{"alg": alg, "iss": testIssuer, "sub": "alice", "aud": testAudience, "exp": now.Add(time.Hour).Unix()}
	if change != nil {
		change(c)
	}
	return c
}

func TestProxyJWTsAreVerifiedAgainstTheIssuerKeys(t *testing.T) {
	proxy := newTestProxy(t)
	service, now := newProofService(t, proxy.config)
	sign := proxy.sign
	claims := func(alg string, change func(map[string]any)) map[string]any { return userClaims(alg, now(), change) }
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
		{"issuer clock ahead within skew", sign("ec", claims("ES256", func(c map[string]any) { c["nbf"] = now().Add(30 * time.Second).Unix() })), true},
		{"not yet valid", sign("ec", claims("ES256", func(c map[string]any) { c["nbf"] = now().Add(2 * time.Minute).Unix() })), false},
		{"without subject", sign("ec", claims("ES256", func(c map[string]any) { delete(c, "sub") })), false},
		{"RSA key named as ES256", sign("rsa", claims("ES256", nil)), false},
		{"unsigned", sign("ec", claims("none", nil)), false},
		{"unknown key", sign("gone", claims("ES256", nil)), false},
		{"missing", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			user, _, err := service.users.verify(test.token, now())
			if test.valid != (err == nil && user == "alice@"+testIssuer) || err != nil && !errors.Is(err, ErrUserInvalid) {
				t.Fatalf("verify() = %q, %v; want valid %v", user, err, test.valid)
			}
		})
	}
	if proxy.fetches != 1 {
		t.Fatalf("JWKS fetched %d times, want once a minute even for unknown keys", proxy.fetches)
	}
}

func TestProxyKeysFollowTheIssuer(t *testing.T) {
	proxy := newTestProxy(t)
	service, now := newProofService(t, proxy.config)
	verify := func(kid string, at time.Duration) error {
		_, _, err := service.users.verify(proxy.sign(kid, userClaims("ES256", now().Add(at), nil)), now().Add(at))
		return err
	}
	proxy.published = []string{"ec"}
	if err := verify("ec", 0); err != nil {
		t.Fatal(err)
	}
	proxy.published = []string{"ec", "ec2"}
	if err := verify("ec2", 30*time.Second); !errors.Is(err, ErrUserInvalid) {
		t.Fatalf("new key within a minute of the last fetch = %v", err)
	}
	if err := verify("ec2", jwksMinAge); err != nil {
		t.Fatalf("new key a minute later = %v, want a refetch", err)
	}
	proxy.published = []string{"ec2"}
	if err := verify("ec", jwksMinAge+jwksMaxAge); !errors.Is(err, ErrUserInvalid) {
		t.Fatalf("removed key once the keys aged = %v", err)
	}
	proxy.down = true
	fetches := proxy.fetches
	for _, at := range []time.Duration{3 * jwksMaxAge, 3*jwksMaxAge + 30*time.Second} {
		if err := verify("ec2", at); err == nil || errors.Is(err, ErrUserInvalid) {
			t.Fatalf("verify() while the JWKS is down = %v, want a server error", err)
		}
	}
	if proxy.fetches != fetches+1 {
		t.Fatalf("JWKS fetched %d times while down, want once a minute", proxy.fetches-fetches)
	}
	proxy.down = false
	if err := verify("ec2", 3*jwksMaxAge+jwksMinAge); err != nil {
		t.Fatalf("verify() a minute after the JWKS is back = %v", err)
	}
}

func TestProxyUserGetsTheDeviceTokensAndKeepsTheDevice(t *testing.T) {
	proxy := newTestProxy(t)
	service, now := newProofService(t, proxy.config)
	key := newTestKey(t, "EdDSA")
	if _, err := service.RegisterApproved(key.registration("Laptop")); err != nil {
		t.Fatal(err)
	}
	issue := func(user string, expiresIn time.Duration) (AccessToken, error) {
		proxyJWT := ""
		if user != "" {
			proxyJWT = proxy.sign("ec", map[string]any{"alg": "ES256", "iss": testIssuer, "sub": user, "aud": testAudience, "exp": now().Add(expiresIn).Unix()})
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
	alices, err := issue("alice", 2*time.Minute)
	if err != nil || alices.ExpiresIn != 120 {
		t.Fatalf("token for a proxy identity expiring sooner = %+v, %v", alices, err)
	}
	if _, err := issue("bob", time.Hour); !errors.Is(err, ErrUserInvalid) {
		t.Fatalf("token for another user = %v", err)
	}
	if _, err := service.RegisterApproved(key.registration("Laptop")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.loadCredential(TokenHash(alices.Token)); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("alice's token after approving the device again = %v", err)
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
