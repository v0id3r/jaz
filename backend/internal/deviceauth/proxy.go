package deviceauth

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/wins/jaz/backend/internal/storage"
)

// ProxyConfig trusts the user a trusted auth proxy (Teleport app access,
// Cloudflare Access, Pomerium, ...) vouches for with a JWT in Header, signed by
// a key of the JWKS it publishes. In proof mode a device then gets access
// tokens only for such a user, and only for the first one it got them for.
type ProxyConfig struct {
	Header   string
	JWKS     string
	Issuer   string
	Audience string
}

// ErrUserInvalid refuses an access token to a request without a valid auth
// proxy identity, or with one the device does not belong to.
var ErrUserInvalid = errors.New("auth proxy identity is missing, invalid or not the device's user")

// The issuer's keys are refetched when older than jwksMaxAge, so removed keys
// stop working, and once older than jwksMinAge when none verifies a token, so
// rotated keys start working; a failed fetch also waits jwksMinAge.
const jwksMinAge, jwksMaxAge = time.Minute, 10 * time.Minute

// jwtIssuer verifies JWTs an external issuer signs for audience with the keys
// it publishes at jwks.
type jwtIssuer struct {
	jwks, issuer, audience string
	client                 *http.Client
	mu                     sync.Mutex
	keys                   []jwk
	fetchErr               error
	fetched                time.Time
}

type jwk struct{ Kid, Kty, Crv, X, Y, N, E string }

// verify returns the user (sub@iss) of a valid token and when it expires. The
// issuer's clock may run up to proofMaxAge ahead of ours.
func (i *jwtIssuer) verify(token string, now time.Time) (string, time.Time, error) {
	var header struct{ Alg, Kid string }
	var claims struct {
		Iss string   `json:"iss"`
		Sub string   `json:"sub"`
		Aud audience `json:"aud"`
		Exp int64    `json:"exp"`
		Nbf int64    `json:"nbf"`
	}
	jws, ok := parseJWS(token, &header, &claims)
	if !ok {
		return "", time.Time{}, ErrUserInvalid
	}
	signed := func(keys []jwk) bool {
		return slices.ContainsFunc(keys, func(key jwk) bool {
			verify := key.verifier(header.Alg)
			return key.Kid == header.Kid && verify != nil && verify(jws.signed, jws.sig)
		})
	}
	keys, err := i.keySet(now, false)
	if err == nil && !signed(keys) {
		keys, err = i.keySet(now, true)
	}
	if err != nil {
		return "", time.Time{}, err
	}
	exp := time.Unix(claims.Exp, 0)
	if !signed(keys) || claims.Iss != i.issuer || !slices.Contains(claims.Aud, i.audience) || claims.Sub == "" ||
		!now.Before(exp) || now.Add(proofMaxAge).Before(time.Unix(claims.Nbf, 0)) {
		return "", time.Time{}, ErrUserInvalid
	}
	return claims.Sub + "@" + claims.Iss, exp, nil
}

// audience is an aud claim, a single string or a list (RFC 7519 §4.1.3).
type audience []string

func (a *audience) UnmarshalJSON(raw []byte) error {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		*a = audience{one}
		return nil
	}
	return json.Unmarshal(raw, (*[]string)(a))
}

// keySet returns the issuer's keys, refetching them as the jwks ages allow;
// unverified asks for a refetch since none of them verified a token.
func (i *jwtIssuer) keySet(now time.Time, unverified bool) ([]jwk, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if age := now.Sub(i.fetched); age < jwksMinAge || age < jwksMaxAge && !unverified && i.fetchErr == nil {
		return i.keys, i.fetchErr
	}
	i.fetched = now
	i.keys, i.fetchErr = i.fetchKeys()
	return i.keys, i.fetchErr
}

func (i *jwtIssuer) fetchKeys() ([]jwk, error) {
	res, err := i.client.Get(i.jwks)
	if err != nil {
		return nil, fmt.Errorf("auth proxy JWKS: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth proxy JWKS returned %s", res.Status)
	}
	var set struct{ Keys []jwk }
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("auth proxy JWKS: %w", err)
	}
	return set.Keys, nil
}

// verifier checks JWS signatures of alg by the key, or is nil when alg and key
// do not form a supported pair.
func (k jwk) verifier(alg string) func(signed, sig []byte) bool {
	if k.Kty != "RSA" {
		return keyVerifier(alg, jwkPublicKey(k.Kty, k.Crv, k.X, k.Y))
	}
	n, errN := base64.RawURLEncoding.DecodeString(k.N)
	e, errE := base64.RawURLEncoding.DecodeString(k.E)
	if alg != "RS256" || errN != nil || errE != nil || len(e) == 0 || len(e) > 4 {
		return nil
	}
	key := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	return func(signed, sig []byte) bool {
		digest := sha256.Sum256(signed)
		return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) == nil
	}
}

// authenticateUser binds a device to the user the auth proxy vouches for, if
// it belongs to nobody yet, and returns when that user's identity expires.
func (s *Service) authenticateUser(device storage.Device, token string) (time.Time, error) {
	user, expiresAt, err := s.users.verify(token, s.now())
	if err != nil {
		return time.Time{}, err
	}
	if device.User != user {
		bound, err := s.store.BindDeviceUser(device.ID, user)
		if err != nil {
			return time.Time{}, err
		}
		if !bound {
			return time.Time{}, ErrUserInvalid
		}
	}
	return expiresAt, nil
}
