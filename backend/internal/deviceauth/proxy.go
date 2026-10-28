package deviceauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
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

// ProxyConfig trusts the signed identity Teleport app access attaches to each
// request: its listed users and roles act as the host owner. Disabled while
// JWKS is empty.
type ProxyConfig struct {
	JWKS     string
	Issuer   string
	Audience string
	Users    []string
	Roles    []string
}

type Proxy struct {
	cfg     ProxyConfig
	client  *http.Client
	mu      sync.Mutex
	keys    map[string]*ecdsa.PublicKey
	fetched time.Time
}

const (
	proxyAssertionHeader = "Teleport-Jwt-Assertion"
	jwksRefreshInterval  = time.Minute
)

func NewProxy(cfg ProxyConfig) *Proxy {
	if cfg.JWKS == "" {
		return nil
	}
	return &Proxy{cfg: cfg, client: &http.Client{Timeout: 10 * time.Second}}
}

// Owner reports whether the request carries a valid proxy identity of an
// allowed user or role. A nil Proxy trusts nobody.
func (p *Proxy) Owner(r *http.Request) bool {
	if p == nil {
		return false
	}
	token := r.Header.Get(proxyAssertionHeader)
	if token == "" {
		return false
	}
	claims, err := p.verify(token)
	if err != nil {
		return false
	}
	return slices.Contains(p.cfg.Users, claims.Subject) || slices.ContainsFunc(claims.Roles, func(role string) bool {
		return slices.Contains(p.cfg.Roles, role)
	})
}

type proxyClaims struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"`
	Audience  audience `json:"aud"`
	Expiry    int64    `json:"exp"`
	NotBefore int64    `json:"nbf"`
	Roles     []string `json:"roles"`
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

func (p *Proxy) verify(token string) (proxyClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return proxyClaims{}, errors.New("malformed proxy token")
	}
	var header struct{ Alg, Kid string }
	if err := decodeSegment(parts[0], &header); err != nil || header.Alg != "ES256" {
		return proxyClaims{}, errors.New("unsupported proxy token")
	}
	key, err := p.key(header.Kid)
	if err != nil {
		return proxyClaims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return proxyClaims{}, errors.New("malformed proxy token signature")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return proxyClaims{}, errors.New("invalid proxy token signature")
	}
	var claims proxyClaims
	if err := decodeSegment(parts[1], &claims); err != nil {
		return proxyClaims{}, err
	}
	now := time.Now().Unix()
	if claims.Issuer != p.cfg.Issuer || !slices.Contains(claims.Audience, p.cfg.Audience) || now >= claims.Expiry || now < claims.NotBefore {
		return proxyClaims{}, errors.New("proxy token is not valid here")
	}
	return claims, nil
}

func decodeSegment(segment string, out any) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (p *Proxy) key(kid string) (*ecdsa.PublicKey, error) {
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

func (p *Proxy) fetchKeys() (map[string]*ecdsa.PublicKey, error) {
	res, err := p.client.Get(p.cfg.JWKS)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy JWKS returned %s", res.Status)
	}
	var set struct {
		Keys []struct{ Kty, Crv, Kid, X, Y string } `json:"keys"`
	}
	if err := json.NewDecoder(res.Body).Decode(&set); err != nil {
		return nil, err
	}
	keys := make(map[string]*ecdsa.PublicKey)
	for _, k := range set.Keys {
		x, errX := base64.RawURLEncoding.DecodeString(k.X)
		y, errY := base64.RawURLEncoding.DecodeString(k.Y)
		if k.Kty != "EC" || k.Crv != "P-256" || errX != nil || errY != nil {
			continue
		}
		if key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...)); err == nil {
			keys[k.Kid] = key
		}
	}
	return keys, nil
}
