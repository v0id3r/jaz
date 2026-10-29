package deviceauth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
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
		JWK      proofJWK
	}
	if decodeSegment(parts[0], &header) != nil || header.Typ != "dpop+jwt" {
		return ErrProofInvalid
	}
	raw, verify := header.JWK.verifier(header.Alg)
	if verify == nil {
		return ErrProofInvalid
	}
	sig, errSig := base64.RawURLEncoding.DecodeString(parts[2])
	if base64.RawURLEncoding.EncodeToString(raw) != publicKey || errSig != nil || !verify([]byte(parts[0]+"."+parts[1]), sig) {
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

type proofJWK struct{ Kty, Crv, X, Y string }

// verifier returns the key in the raw form device identities store (32 bytes
// for Ed25519, the 65-byte uncompressed point for P-256) and its signature
// check, or a nil check when alg and key do not form a supported pair.
func (k proofJWK) verifier(alg string) ([]byte, func(signed, sig []byte) bool) {
	x, errX := base64.RawURLEncoding.DecodeString(k.X)
	switch {
	case errX != nil:
	case alg == "EdDSA" && k.Kty == "OKP" && k.Crv == "Ed25519" && len(x) == ed25519.PublicKeySize:
		return x, func(signed, sig []byte) bool { return ed25519.Verify(x, signed, sig) }
	case alg == "ES256" && k.Kty == "EC" && k.Crv == "P-256":
		y, _ := base64.RawURLEncoding.DecodeString(k.Y)
		raw := append(append([]byte{4}, x...), y...)
		key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
		if err != nil {
			return nil, nil
		}
		return raw, func(signed, sig []byte) bool {
			digest := sha256.Sum256(signed)
			return len(sig) == 64 && ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
		}
	}
	return nil, nil
}

func validP256(raw []byte) bool {
	_, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
	return err == nil
}

func decodeSegment(segment string, out any) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
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
