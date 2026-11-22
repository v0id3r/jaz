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
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// ProofHeader carries a DPoP proof (RFC 9449) signed by the device key.
	ProofHeader = "DPoP"
	// ProofAlgs lists the proof algorithms a DPoP challenge advertises.
	ProofAlgs = "EdDSA ES256"
)

const (
	proofMaxAge    = time.Minute
	proofMaxJTILen = 64
)

// ErrProofInvalid rejects a request whose proof is missing, stale, replayed or
// made for another request; the device key itself may still be fine.
var ErrProofInvalid = errors.New("device proof is missing or invalid")

// Proof is the DPoP proof a request presents with the method and path it was
// received on. LocalOrigin is the origin of a request that reached loopback
// without passing a proxy, the only request whose own origin is trusted.
type Proof struct {
	JWT         string
	Method      string
	Path        string
	LocalOrigin string
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
	if !s.requireProof {
		return nil
	}
	var header struct {
		Typ, Alg string
		JWK      struct{ Kty, Crv, X, Y, D string }
	}
	var claims struct {
		JTI string `json:"jti"`
		HTM string `json:"htm"`
		HTU string `json:"htu"`
		IAT int64  `json:"iat"`
		ATH string `json:"ath"`
	}
	jws, ok := parseJWS(proof.JWT, &header, &claims)
	raw := jwkPublicKey(header.JWK.Kty, header.JWK.Crv, header.JWK.X, header.JWK.Y)
	verify := keyVerifier(header.Alg, raw)
	if !ok || header.Typ != "dpop+jwt" || header.JWK.D != "" || verify == nil {
		return ErrProofInvalid
	}
	if base64.RawURLEncoding.EncodeToString(raw) != publicKey || !verify(jws.signed, jws.sig) {
		return ErrUnauthorized
	}
	target, err := url.Parse(claims.HTU)
	if err != nil || !s.ownOrigin(claims.HTU, proof) || target.Path != proof.Path || claims.HTM != proof.Method || claims.ATH != tokenHash ||
		claims.JTI == "" || len(claims.JTI) > proofMaxJTILen || s.now().Sub(time.Unix(claims.IAT, 0)).Abs() > proofMaxAge {
		return ErrProofInvalid
	}
	return s.spendProof(claims.JTI)
}

// ownOrigin reports whether a URL a client signed (a proof's htu or a client
// assertion's aud) names this server: one of its configured origins, or the
// origin a local request reached it on.
func (s *Service) ownOrigin(value string, proof Proof) bool {
	origin := normalizeOrigin(value)
	return origin != "" && (slices.Contains(s.origins, origin) || origin == normalizeOrigin(proof.LocalOrigin))
}

// jws is a compact JWS whose header and claims were decoded.
type jws struct{ signed, sig []byte }

func parseJWS(token string, header, claims any) (jws, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || decodeSegment(parts[0], header) != nil || decodeSegment(parts[1], claims) != nil {
		return jws{}, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	return jws{signed: []byte(parts[0] + "." + parts[1]), sig: sig}, err == nil
}

// normalizeOrigin reduces a URL to its scheme://host[:port] origin, folding
// WebSocket schemes and default ports into their HTTP form, or returns "".
func normalizeOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := map[string]string{"http": "http", "ws": "http", "https": "https", "wss": "https"}[strings.ToLower(u.Scheme)]
	if scheme == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" && port != map[string]string{"http": "80", "https": "443"}[scheme] {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host
}

// jwkPublicKey returns a JWK's key in the raw form device identities store:
// 32 bytes for Ed25519, the 65-byte uncompressed point for P-256.
func jwkPublicKey(kty, crv, x, y string) []byte {
	rawX, errX := base64.RawURLEncoding.DecodeString(x)
	rawY, errY := base64.RawURLEncoding.DecodeString(y)
	switch {
	case errX != nil:
	case kty == "OKP" && crv == "Ed25519":
		return rawX
	case kty == "EC" && crv == "P-256" && errY == nil:
		return append(append([]byte{4}, rawX...), rawY...)
	}
	return nil
}

// keyVerifier checks JWS signatures of alg by a raw device key, or is nil when
// alg and key do not form a supported pair.
func keyVerifier(alg string, raw []byte) func(signed, sig []byte) bool {
	switch {
	case alg == "EdDSA" && len(raw) == ed25519.PublicKeySize:
		return func(signed, sig []byte) bool { return ed25519.Verify(raw, signed, sig) }
	case alg == "ES256":
		key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
		if err != nil {
			return nil
		}
		return func(signed, sig []byte) bool {
			digest := sha256.Sum256(signed)
			return len(sig) == 64 && ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
		}
	}
	return nil
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
