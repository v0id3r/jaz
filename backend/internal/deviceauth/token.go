package deviceauth

import (
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/wins/jaz/backend/internal/storage"
)

// ClientAssertionType is the private_key_jwt assertion type (RFC 7523 §2.2).
const ClientAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

const accessTokenTTL = 10 * time.Minute

var (
	ErrUnsupportedGrant = errors.New("device access tokens require proof mode")
	ErrInvalidClient    = errors.New("client assertion is missing or invalid")
	ErrTokenInvalid     = errors.New("access token is invalid or expired")
)

type AccessToken struct {
	Token     string
	ExpiresIn int
}

// accessTokens holds proof-mode access tokens: short-lived and in memory only,
// so a restart costs clients one token request and nothing durable is secret.
type accessTokens struct {
	mu     sync.Mutex
	issued map[string]issuedToken
}

type issuedToken struct {
	deviceID  string
	expiresAt time.Time
}

// IssueAccessToken serves the client_credentials grant of a device that
// authenticates with a private_key_jwt assertion and binds the token to the
// same key with a DPoP proof (RFC 9449 §5). Behind a trusted auth proxy,
// proxyJWT must vouch for the device's user, and the token expires with it.
func (s *Service) IssueAccessToken(assertion string, proof Proof, proxyJWT string) (AccessToken, error) {
	if !s.requireProof {
		return AccessToken{}, ErrUnsupportedGrant
	}
	device, err := s.authenticateClient(assertion, proof)
	if err != nil {
		return AccessToken{}, err
	}
	if err := s.verifyProof(proof, device.PublicKey, ""); err != nil {
		return AccessToken{}, ErrProofInvalid
	}
	now := s.now()
	expiresAt := now.Add(accessTokenTTL)
	if s.users != nil {
		userExpiresAt, err := s.authenticateUser(device, proxyJWT)
		if err != nil {
			return AccessToken{}, err
		}
		if userExpiresAt.Before(expiresAt) {
			expiresAt = userExpiresAt
		}
	}
	token, err := newToken()
	if err != nil {
		return AccessToken{}, err
	}
	t := &s.tokens
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.issued == nil {
		t.issued = make(map[string]issuedToken)
	}
	for hash, entry := range t.issued {
		if !now.Before(entry.expiresAt) {
			delete(t.issued, hash)
		}
	}
	t.issued[TokenHash(token)] = issuedToken{deviceID: device.ID, expiresAt: expiresAt}
	return AccessToken{Token: token, ExpiresIn: int(expiresAt.Sub(now) / time.Second)}, nil
}

// authenticateClient verifies a private_key_jwt client assertion (RFC 7523,
// with the sole issuer audience of draft-ietf-oauth-rfc7523bis) whose client
// is an approved device signing with its own key. Its iat gets the clock skew
// proofs get and bounds its exp, so its jti outlives it in the spent set.
func (s *Service) authenticateClient(assertion string, proof Proof) (storage.Device, error) {
	var header struct{ Typ, Alg string }
	var claims struct {
		Iss string `json:"iss"`
		Sub string `json:"sub"`
		Aud string `json:"aud"`
		JTI string `json:"jti"`
		IAT int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	jws, ok := parseJWS(assertion, &header, &claims)
	now, iat, exp := s.now(), time.Unix(claims.IAT, 0), time.Unix(claims.Exp, 0)
	if !ok || header.Typ != "" && header.Typ != "client-authentication+jwt" || claims.Sub == "" || claims.Iss != claims.Sub ||
		claims.JTI == "" || len(claims.JTI) > proofMaxJTILen || claims.Aud != normalizeOrigin(claims.Aud) || !s.ownOrigin(claims.Aud, proof) ||
		now.Sub(iat).Abs() > proofMaxAge || !now.Before(exp) || exp.Sub(iat) > proofMaxAge {
		return storage.Device{}, ErrInvalidClient
	}
	device, err := s.store.LoadDevice(claims.Sub)
	if errors.Is(err, storage.ErrDeviceNotFound) {
		return storage.Device{}, ErrInvalidClient
	}
	if err != nil {
		return storage.Device{}, err
	}
	raw, _ := base64.RawURLEncoding.DecodeString(device.PublicKey)
	verify := keyVerifier(header.Alg, raw)
	if device.Status != storage.DeviceStatusApproved || verify == nil || !verify(jws.signed, jws.sig) || s.spendProof("client:"+claims.JTI) != nil {
		return storage.Device{}, ErrInvalidClient
	}
	return device, nil
}

func (t *accessTokens) revoke(deviceID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for hash, entry := range t.issued {
		if entry.deviceID == deviceID {
			delete(t.issued, hash)
		}
	}
}

// loadCredential resolves the device behind a credential hash: an issued
// access token in proof mode, the long-lived device token otherwise.
func (s *Service) loadCredential(hash string) (storage.Device, error) {
	if !s.requireProof {
		return s.store.LoadDeviceByTokenHash(hash)
	}
	s.tokens.mu.Lock()
	entry, ok := s.tokens.issued[hash]
	s.tokens.mu.Unlock()
	if !ok || !s.now().Before(entry.expiresAt) {
		return storage.Device{}, ErrTokenInvalid
	}
	return s.store.LoadDevice(entry.deviceID)
}

// deviceCredential mints the long-lived device token. In proof mode the device
// key is the credential, so the token column only keeps a unique placeholder.
func (s *Service) deviceCredential(deviceID string) (token, hash string, err error) {
	if s.requireProof {
		return "", "key:" + deviceID, nil
	}
	token, err = newToken()
	return token, TokenHash(token), err
}
