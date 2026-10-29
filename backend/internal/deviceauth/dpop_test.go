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

const testOrigin = "https://jaz.example"

func TestDeviceProofBindsAccessTokensToTheDeviceKey(t *testing.T) {
	for _, alg := range []string{"EdDSA", "ES256"} {
		t.Run(alg, func(t *testing.T) {
			service, now := newProofService(t)
			key, stranger := newTestKey(t, alg), newTestKey(t, alg)
			registered, err := service.RegisterApproved(key.registration("Laptop"))
			if err != nil || registered.Token != "" {
				t.Fatalf("RegisterApproved() = token %q, %v; want no long-lived token in proof mode", registered.Token, err)
			}
			token := issueToken(t, service, key)
			ath := TokenHash(token)
			claims := func(change func(map[string]any)) map[string]any {
				c := map[string]any{"jti": rand.Text(), "htm": "GET", "htu": testOrigin + "/v1/sessions", "iat": now().Unix(), "ath": ath}
				if change != nil {
					change(c)
				}
				return c
			}
			replayed := signProof(key, key, claims(nil))
			for _, test := range []struct {
				name  string
				token string
				proof string
				want  error
			}{
				{"valid", token, replayed, nil},
				{"replayed", token, replayed, ErrProofInvalid},
				{"default port and query", token, signProof(key, key, claims(func(c map[string]any) { c["htu"] = "HTTPS://Jaz.Example:443/v1/sessions?x=1" })), nil},
				{"origin of another server", token, signProof(key, key, claims(func(c map[string]any) { c["htu"] = "http://10.0.0.1:8080/v1/sessions" })), ErrProofInvalid},
				{"other method", token, signProof(key, key, claims(func(c map[string]any) { c["htm"] = "POST" })), ErrProofInvalid},
				{"other path", token, signProof(key, key, claims(func(c map[string]any) { c["htu"] = testOrigin + "/v1/devices" })), ErrProofInvalid},
				{"other token", token, signProof(key, key, claims(func(c map[string]any) { c["ath"] = TokenHash("jaz_other") })), ErrProofInvalid},
				{"stale", token, signProof(key, key, claims(func(c map[string]any) { c["iat"] = now().Add(-2 * time.Minute).Unix() })), ErrProofInvalid},
				{"from the future", token, signProof(key, key, claims(func(c map[string]any) { c["iat"] = now().Add(2 * time.Minute).Unix() })), ErrProofInvalid},
				{"oversized jti", token, signProof(key, key, claims(func(c map[string]any) { c["jti"] = strings.Repeat("j", 65) })), ErrProofInvalid},
				{"missing", token, "", ErrProofInvalid},
				{"signed by another key", token, signProof(stranger, key, claims(nil)), ErrUnauthorized},
				{"another device key", token, signProof(stranger, stranger, claims(nil)), ErrUnauthorized},
				{"unknown token", "jaz_unknown", signProof(key, key, claims(func(c map[string]any) { c["ath"] = TokenHash("jaz_unknown") })), ErrTokenInvalid},
				{"private key in jwk", token, signJWS(key, map[string]any{"typ": "dpop+jwt", "alg": alg, "jwk": withPrivate(key.jwk)}, claims(nil)), ErrProofInvalid},
			} {
				t.Run(test.name, func(t *testing.T) {
					_, err := service.Authenticate(test.token, Proof{JWT: test.proof, Method: "GET", Path: "/v1/sessions"}, SeenInfo{})
					if !errors.Is(err, test.want) || (test.want == nil && err != nil) {
						t.Fatalf("Authenticate() error = %v, want %v", err, test.want)
					}
				})
			}
			service.now = func() time.Time { return now().Add(accessTokenTTL) }
			if _, err := service.Authenticate(token, Proof{JWT: signProof(key, key, claims(nil)), Method: "GET", Path: "/v1/sessions"}, SeenInfo{}); !errors.Is(err, ErrTokenInvalid) {
				t.Fatalf("expired access token = %v, want ErrTokenInvalid", err)
			}
			service.now = now
			token = issueToken(t, service, key)
			if _, err := service.RevokeDevice(key.id); err != nil {
				t.Fatal(err)
			}
			ath = TokenHash(token)
			if _, err := service.Authenticate(token, Proof{JWT: signProof(key, key, claims(nil)), Method: "GET", Path: "/v1/sessions"}, SeenInfo{}); !errors.Is(err, ErrTokenInvalid) {
				t.Fatalf("access token of a revoked device = %v, want ErrTokenInvalid", err)
			}
		})
	}
}

func withPrivate(jwk map[string]string) map[string]string {
	return map[string]string{"kty": jwk["kty"], "crv": jwk["crv"], "x": jwk["x"], "y": jwk["y"], "d": "AAAA"}
}

func TestSpentProofsOutliveTheirAcceptanceWindow(t *testing.T) {
	service, now := newProofService(t)
	key := newTestKey(t, "EdDSA")
	if _, err := service.RegisterApproved(key.registration("Laptop")); err != nil {
		t.Fatal(err)
	}
	token := issueToken(t, service, key)
	current := now().Add(90 * time.Second)
	service.now = func() time.Time { return current }
	early := signProof(key, key, map[string]any{"jti": rand.Text(), "htm": "GET", "htu": testOrigin + "/v1/sessions", "iat": current.Add(proofMaxAge).Unix(), "ath": TokenHash(token)})
	if _, err := service.Authenticate(token, Proof{JWT: early, Method: "GET", Path: "/v1/sessions"}, SeenInfo{}); err != nil {
		t.Fatal(err)
	}
	current = current.Add(proofMaxAge)
	if _, err := service.Authenticate(token, Proof{JWT: early, Method: "GET", Path: "/v1/sessions"}, SeenInfo{}); !errors.Is(err, ErrProofInvalid) {
		t.Fatalf("a still-fresh proof was replayable after the spent set rotated: %v", err)
	}
}

func TestRemoteRegistrationProvesTheDeviceKey(t *testing.T) {
	service, now := newProofService(t)
	if _, err := service.RegisterApproved(newTestKey(t, "EdDSA").registration("Laptop")); err != nil {
		t.Fatal(err)
	}
	key := newTestKey(t, "ES256")
	newcomer := key.registration("Phone")
	if _, _, err := service.CreatePairing(newcomer); !errors.Is(err, ErrProofInvalid) {
		t.Fatalf("pairing without a key proof = %v", err)
	}
	newcomer.Proof = Proof{Method: "POST", Path: "/v1/devices/pairing-requests", JWT: signProof(key, key, map[string]any{
		"jti": rand.Text(), "htm": "POST", "htu": testOrigin + "/v1/devices/pairing-requests", "iat": now().Unix(),
	})}
	if _, _, err := service.CreatePairing(newcomer); err != nil {
		t.Fatalf("pairing with a key proof = %v", err)
	}
}

func TestAccessTokensRequireTheDeviceClientAssertion(t *testing.T) {
	service, now := newProofService(t)
	key, stranger, unpaired, revoked := newTestKey(t, "EdDSA"), newTestKey(t, "ES256"), newTestKey(t, "EdDSA"), newTestKey(t, "ES256")
	for _, k := range []testKey{key, revoked} {
		if _, err := service.RegisterApproved(k.registration("Laptop")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.RevokeDevice(revoked.id); err != nil {
		t.Fatal(err)
	}
	claims := func(k testKey, change func(map[string]any)) map[string]any {
		c := map[string]any{"iss": k.id, "sub": k.id, "aud": testOrigin, "jti": rand.Text(), "iat": now().Unix(), "exp": now().Add(30 * time.Second).Unix()}
		if change != nil {
			change(c)
		}
		return c
	}
	assertion := func(signer testKey, c map[string]any) string {
		return signJWS(signer, map[string]any{"typ": "client-authentication+jwt", "alg": signer.alg}, c)
	}
	replayed := assertion(key, claims(key, nil))
	for _, test := range []struct {
		name      string
		key       testKey
		assertion string
		proof     string
		want      error
	}{
		{"valid", key, replayed, "", nil},
		{"replayed assertion", key, replayed, "", ErrInvalidClient},
		{"untyped", key, signJWS(key, map[string]any{"alg": "EdDSA"}, claims(key, nil)), "", nil},
		{"dpop proof as assertion", key, signJWS(key, map[string]any{"typ": "dpop+jwt", "alg": "EdDSA"}, claims(key, nil)), "", ErrInvalidClient},
		{"token endpoint audience", key, assertion(key, claims(key, func(c map[string]any) { c["aud"] = testOrigin + "/v1/devices/token" })), "", ErrInvalidClient},
		{"audience of another server", key, assertion(key, claims(key, func(c map[string]any) { c["aud"] = "https://other.example" })), "", ErrInvalidClient},
		{"audience list", key, assertion(key, claims(key, func(c map[string]any) { c["aud"] = []string{testOrigin} })), "", ErrInvalidClient},
		{"issuer is not the client", key, assertion(key, claims(key, func(c map[string]any) { c["iss"] = "someone" })), "", ErrInvalidClient},
		{"expired", key, assertion(key, claims(key, func(c map[string]any) { c["exp"] = now().Unix() })), "", ErrInvalidClient},
		{"long-lived", key, assertion(key, claims(key, func(c map[string]any) { c["exp"] = now().Add(time.Hour).Unix() })), "", ErrInvalidClient},
		{"client clock behind", key, assertion(key, claims(key, func(c map[string]any) {
			c["iat"], c["exp"] = now().Add(-50*time.Second).Unix(), now().Add(-20*time.Second).Unix()
		})), "", ErrInvalidClient},
		{"client clock ahead within skew", key, assertion(key, claims(key, func(c map[string]any) {
			c["iat"], c["exp"] = now().Add(50*time.Second).Unix(), now().Add(80*time.Second).Unix()
		})), "", nil},
		{"client clock ahead beyond skew", key, assertion(key, claims(key, func(c map[string]any) {
			c["iat"], c["exp"] = now().Add(2*time.Minute).Unix(), now().Add(150*time.Second).Unix()
		})), "", ErrInvalidClient},
		{"without iat", key, assertion(key, claims(key, func(c map[string]any) { delete(c, "iat") })), "", ErrInvalidClient},
		{"signed by another key", key, assertion(stranger, claims(key, nil)), "", ErrInvalidClient},
		{"revoked device", revoked, assertion(revoked, claims(revoked, nil)), "", ErrInvalidClient},
		{"unpaired device", unpaired, assertion(unpaired, claims(unpaired, nil)), "", ErrInvalidClient},
		{"proof by another key", key, assertion(key, claims(key, nil)), tokenProof(stranger, now()), ErrProofInvalid},
		{"missing proof", key, assertion(key, claims(key, nil)), "-", ErrProofInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			proof := test.proof
			if proof == "" {
				proof = tokenProof(test.key, now())
			}
			_, err := service.IssueAccessToken(test.assertion, Proof{JWT: proof, Method: "POST", Path: "/v1/devices/token"})
			if !errors.Is(err, test.want) || (test.want == nil && err != nil) {
				t.Fatalf("IssueAccessToken() error = %v, want %v", err, test.want)
			}
		})
	}
	service.RequireProof = false
	if _, err := service.IssueAccessToken(assertion(key, claims(key, nil)), Proof{}); !errors.Is(err, ErrUnsupportedGrant) {
		t.Fatalf("IssueAccessToken() without proof mode = %v", err)
	}
}

func TestProofOriginsAreConfiguredOrTheLocalRequestOrigin(t *testing.T) {
	service := &Service{Origins: []string{"https://jaz.example"}}
	local := Proof{LocalOrigin: "http://127.0.0.1:5299"}
	for _, test := range []struct {
		value string
		proof Proof
		want  bool
	}{
		{"https://jaz.example", Proof{}, true},
		{"https://JAZ.example:443/v1/sessions", Proof{}, true},
		{"https://evil.example", Proof{}, false},
		{"http://127.0.0.1:5299/v1/sessions", local, true},
		{"ws://127.0.0.1:5299/v1/sessions", local, true},
		{"http://127.0.0.1:5299", Proof{}, false},
		{"", Proof{}, false},
	} {
		if got := service.ownOrigin(test.value, test.proof); got != test.want {
			t.Errorf("ownOrigin(%q, %+v) = %v, want %v", test.value, test.proof, got, test.want)
		}
	}
}

func newProofService(t *testing.T) (*Service, func() time.Time) {
	t.Helper()
	store, err := sqlite.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := New(store)
	service.RequireProof = true
	service.Origins = []string{testOrigin}
	start := time.Now().UTC()
	now := func() time.Time { return start }
	service.now = now
	return service, now
}

// testKey is a device key of either supported algorithm.
type testKey struct {
	alg  string
	id   string
	raw  []byte
	jwk  map[string]string
	sign func([]byte) []byte
}

func newTestKey(t *testing.T, alg string) testKey {
	t.Helper()
	k := testKey{alg: alg}
	if alg == "EdDSA" {
		public, private, _ := ed25519.GenerateKey(rand.Reader)
		k.raw = public
		k.jwk = map[string]string{"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(public)}
		k.sign = func(input []byte) []byte { return ed25519.Sign(private, input) }
	} else {
		private, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		k.raw, _ = private.PublicKey.Bytes()
		k.jwk = map[string]string{"kty": "EC", "crv": "P-256", "x": base64.RawURLEncoding.EncodeToString(k.raw[1:33]), "y": base64.RawURLEncoding.EncodeToString(k.raw[33:])}
		k.sign = func(input []byte) []byte {
			digest := sha256.Sum256(input)
			r, s, _ := ecdsa.Sign(rand.Reader, private, digest[:])
			return append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
		}
	}
	sum := sha256.Sum256(k.raw)
	k.id = hex.EncodeToString(sum[:])
	return k
}

func (k testKey) registration(name string) Registration {
	return Registration{
		Identity: DeviceIdentity{DeviceID: k.id, PublicKey: base64.RawURLEncoding.EncodeToString(k.raw)},
		Profile:  DeviceProfile{Name: name, Kind: "browser"},
	}
}

func signJWS(signer testKey, header, claims map[string]any) string {
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	signed := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	return signed + "." + base64.RawURLEncoding.EncodeToString(signer.sign([]byte(signed)))
}

// signProof signs a DPoP proof with signer for the key claimed in its header.
func signProof(signer, claimed testKey, claims map[string]any) string {
	return signJWS(signer, map[string]any{"typ": "dpop+jwt", "alg": claimed.alg, "jwk": claimed.jwk}, claims)
}

func tokenProof(k testKey, now time.Time) string {
	return signProof(k, k, map[string]any{"jti": rand.Text(), "htm": "POST", "htu": testOrigin + "/v1/devices/token", "iat": now.Unix()})
}

func issueToken(t *testing.T, service *Service, k testKey) string {
	t.Helper()
	assertion := signJWS(k, map[string]any{"typ": "client-authentication+jwt", "alg": k.alg}, map[string]any{
		"iss": k.id, "sub": k.id, "aud": testOrigin, "jti": rand.Text(), "iat": service.now().Unix(), "exp": service.now().Add(30 * time.Second).Unix(),
	})
	token, err := service.IssueAccessToken(assertion, Proof{JWT: tokenProof(k, service.now()), Method: "POST", Path: "/v1/devices/token"})
	if err != nil || token.Token == "" || token.ExpiresIn != int(accessTokenTTL/time.Second) {
		t.Fatalf("IssueAccessToken() = %+v, %v", token, err)
	}
	return token.Token
}
