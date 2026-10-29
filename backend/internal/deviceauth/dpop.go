package deviceauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ProofHeader carries a DPoP proof (RFC 9449) signed by the device key.
const ProofHeader = "DPoP"

const (
	proofMaxAge    = time.Minute
	proofMaxJTILen = 64
)

// ErrProofInvalid rejects a request whose proof is missing, stale, replayed or
// made for another request; the device key itself may still be fine.
var ErrProofInvalid = errors.New("device proof is missing or invalid")

// Proof is the DPoP proof a request presents with the method and path it was
// received on; the proof's htu is matched by path because proxies rewrite the
// origin the client signed.
type Proof struct {
	JWT    string
	Method string
	Path   string
}

// spentProofs remembers each jti for at least the window in which its iat is
// accepted, in two generations so expiry never scans.
type spentProofs struct {
	mu                sync.Mutex
	current, previous map[string]struct{}
	rotated           time.Time
}

// verifyProof returns ErrUnauthorized when the proof is not signed by
// publicKey (the credential is bound to a key this client no longer holds)
// and ErrProofInvalid for every other defect.
func (s *Service) verifyProof(proof Proof, publicKey, tokenHash string) error {
	if !s.RequireProof {
		return nil
	}
	parts := strings.Split(proof.JWT, ".")
	if len(parts) != 3 {
		return ErrProofInvalid
	}
	var header struct {
		Typ, Alg string
		JWK      struct{ Kty, Crv, X string }
	}
	if decodeSegment(parts[0], &header) != nil || header.Typ != "dpop+jwt" || header.Alg != "EdDSA" ||
		header.JWK.Kty != "OKP" || header.JWK.Crv != "Ed25519" {
		return ErrProofInvalid
	}
	key, errKey := base64.RawURLEncoding.DecodeString(publicKey)
	sig, errSig := base64.RawURLEncoding.DecodeString(parts[2])
	if header.JWK.X != publicKey || errKey != nil || errSig != nil || len(key) != ed25519.PublicKeySize ||
		!ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return ErrUnauthorized
	}
	var claims struct {
		JTI string `json:"jti"`
		HTM string `json:"htm"`
		HTU string `json:"htu"`
		IAT int64  `json:"iat"`
		ATH string `json:"ath"`
	}
	if decodeSegment(parts[1], &claims) != nil {
		return ErrProofInvalid
	}
	target, err := url.Parse(claims.HTU)
	if err != nil || target.Path != proof.Path || claims.HTM != proof.Method || claims.ATH != tokenHash ||
		claims.JTI == "" || len(claims.JTI) > proofMaxJTILen || s.now().Sub(time.Unix(claims.IAT, 0)).Abs() > proofMaxAge {
		return ErrProofInvalid
	}
	return s.spendProof(claims.JTI)
}

func (s *Service) spendProof(jti string) error {
	p := &s.proofs
	p.mu.Lock()
	defer p.mu.Unlock()
	if now := s.now(); p.current == nil || now.Sub(p.rotated) >= 2*proofMaxAge {
		p.previous, p.current, p.rotated = p.current, make(map[string]struct{}), now
	}
	_, inCurrent := p.current[jti]
	_, inPrevious := p.previous[jti]
	if inCurrent || inPrevious {
		return ErrProofInvalid
	}
	p.current[jti] = struct{}{}
	return nil
}
