package deviceauth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// ProxyConfig trusts the signed identity an auth proxy (Teleport app access,
// Cloudflare Access, Pomerium, ...) attaches to each request as a JWT in
// Header. Listed users and roles act as the host owner; with Require, device
// requests must carry an identity and devices are bound to it. Disabled while
// JWKS is empty.
type ProxyConfig struct {
	Header    string
	JWKS      string
	Issuer    string
	Audience  string
	RoleClaim string
	Users     []string
	Roles     []string
	Require   bool
}

func (c ProxyConfig) Validate() error {
	if c.JWKS == "" && !c.Require {
		return nil
	}
	if c.Header == "" || c.JWKS == "" || c.Issuer == "" || c.Audience == "" {
		return errors.New("jaz.devices.proxy needs header, jwks, issuer and audience")
	}
	return nil
}

// Identity is what the auth proxy vouches for: User is "sub@iss", empty when
// the request carries no valid identity.
type Identity struct {
	User  string
	Owner bool
}

type Proxy struct {
	cfg     ProxyConfig
	client  *http.Client
	mu      sync.Mutex
	keys    map[string]crypto.PublicKey
	fetched time.Time
}

const jwksRefreshInterval = time.Minute

func NewProxy(cfg ProxyConfig) *Proxy {
	if cfg.JWKS == "" {
		return nil
	}
	return &Proxy{cfg: cfg, client: &http.Client{Timeout: 10 * time.Second}}
}

// Identify reads the request's proxy identity. A nil Proxy vouches for nobody.
func (p *Proxy) Identify(r *http.Request) Identity {
	if p == nil {
		return Identity{}
	}
	token := r.Header.Get(p.cfg.Header)
	if token == "" {
		return Identity{}
	}
	claims, roles, err := p.verify(token)
	if err != nil || claims.Subject == "" {
		return Identity{}
	}
	return Identity{
		User: claims.Subject + "@" + claims.Issuer,
		Owner: slices.Contains(p.cfg.Users, claims.Subject) || slices.ContainsFunc(roles, func(role string) bool {
			return slices.Contains(p.cfg.Roles, role)
		}),
	}
}

type proxyClaims struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"`
	Audience  audience `json:"aud"`
	Expiry    int64    `json:"exp"`
	NotBefore int64    `json:"nbf"`
}

type audience []string

func (a *audience) UnmarshalJSON(raw []byte) error {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		*a = audience{one}
		return nil
	}
	return json.Unmarshal(raw, (*[]string)(a))
}

func (p *Proxy) verify(token string) (proxyClaims, []string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return proxyClaims{}, nil, errors.New("malformed proxy token")
	}
	var header struct{ Alg, Kid string }
	if err := decodeSegment(parts[0], &header); err != nil {
		return proxyClaims{}, nil, err
	}
	key, err := p.key(header.Kid)
	if err != nil {
		return proxyClaims{}, nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return proxyClaims{}, nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !verifySignature(header.Alg, key, digest[:], sig) {
		return proxyClaims{}, nil, errors.New("invalid proxy token signature")
	}
	var claims proxyClaims
	if err := decodeSegment(parts[1], &claims); err != nil {
		return proxyClaims{}, nil, err
	}
	now := time.Now().Unix()
	if claims.Issuer != p.cfg.Issuer || !slices.Contains(claims.Audience, p.cfg.Audience) || now >= claims.Expiry || now < claims.NotBefore {
		return proxyClaims{}, nil, errors.New("proxy token is not valid here")
	}
	var roles []string
	if p.cfg.RoleClaim != "" {
		var all map[string]json.RawMessage
		if err := decodeSegment(parts[1], &all); err == nil {
			_ = json.Unmarshal(all[p.cfg.RoleClaim], &roles)
		}
	}
	return claims, roles, nil
}

func verifySignature(alg string, key crypto.PublicKey, digest, sig []byte) bool {
	switch key := key.(type) {
	case *ecdsa.PublicKey:
		return alg == "ES256" && len(sig) == 64 &&
			ecdsa.Verify(key, digest, new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
	case *rsa.PublicKey:
		return alg == "RS256" && rsa.VerifyPKCS1v15(key, crypto.SHA256, digest, sig) == nil
	}
	return false
}

func decodeSegment(segment string, out any) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (p *Proxy) key(kid string) (crypto.PublicKey, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if key, ok := p.keys[kid]; ok {
		return key, nil
	}
	if time.Since(p.fetched) < jwksRefreshInterval {
		return nil, errors.New("unknown proxy signing key")
	}
	p.fetched = time.Now()
	keys, err := p.fetchKeys()
	if err != nil {
		return nil, err
	}
	p.keys = keys
	if key, ok := keys[kid]; ok {
		return key, nil
	}
	return nil, errors.New("unknown proxy signing key")
}

func (p *Proxy) fetchKeys() (map[string]crypto.PublicKey, error) {
	res, err := p.client.Get(p.cfg.JWKS)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy JWKS returned %s", res.Status)
	}
	var set struct {
		Keys []struct{ Kty, Crv, Kid, X, Y, N, E string } `json:"keys"`
	}
	if err := json.NewDecoder(res.Body).Decode(&set); err != nil {
		return nil, err
	}
	keys := make(map[string]crypto.PublicKey)
	for _, k := range set.Keys {
		if key := parseJWK(k.Kty, k.Crv, k.X, k.Y, k.N, k.E); key != nil {
			keys[k.Kid] = key
		}
	}
	return keys, nil
}

func parseJWK(kty, crv, x, y, n, e string) crypto.PublicKey {
	b64 := base64.RawURLEncoding.DecodeString
	switch kty {
	case "EC":
		xb, errX := b64(x)
		yb, errY := b64(y)
		if crv != "P-256" || errX != nil || errY != nil {
			return nil
		}
		if key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, xb...), yb...)); err == nil {
			return key
		}
	case "RSA":
		nb, errN := b64(n)
		eb, errE := b64(e)
		if errN != nil || errE != nil || len(eb) == 0 || len(eb) > 4 {
			return nil
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
	}
	return nil
}
