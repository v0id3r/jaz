// Package mesh carries session traffic over mutual TLS with the session's
// SPIFFE identity, transparently to the agent: the runner captures a
// session's connections inside its network namespace and tunnels them to a
// gateway, which knows the session from its client certificate alone.
// Tunnels use HBONE, HTTP/2 CONNECT over mTLS, as Istio ambient does.
package mesh

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	bundlev1 "github.com/spiffe/spire-api-sdk/proto/spire/api/server/bundle/v1"
	svidv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/server/svid/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// issuer mints X.509-SVIDs for sessions from a SPIRE server's admin API; the
// runner already vouches for which session a container belongs to.
type issuer struct {
	conn   *grpc.ClientConn
	svids  svidv1.SVIDClient
	bundle bundlev1.BundleClient
	td     spiffeid.TrustDomain
	ttl    time.Duration
}

func newIssuer(server string, td spiffeid.TrustDomain, ttl time.Duration) (*issuer, error) {
	conn, err := grpc.NewClient("unix://"+server, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &issuer{conn: conn, svids: svidv1.NewSVIDClient(conn), bundle: bundlev1.NewBundleClient(conn), td: td, ttl: ttl}, nil
}

func (i *issuer) mint(ctx context.Context, id spiffeid.ID) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{id.URL()}}, key)
	if err != nil {
		return nil, err
	}
	resp, err := i.svids.MintX509SVID(ctx, &svidv1.MintX509SVIDRequest{Csr: csr, Ttl: int32(i.ttl.Seconds())})
	if err != nil {
		return nil, fmt.Errorf("mint %s: %w", id, err)
	}
	chain := resp.GetSvid().GetCertChain()
	if len(chain) == 0 {
		return nil, fmt.Errorf("mint %s: empty certificate chain", id)
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: chain, PrivateKey: key, Leaf: leaf}, nil
}

func (i *issuer) authorities(ctx context.Context) (*x509bundle.Bundle, error) {
	resp, err := i.bundle.GetBundle(ctx, &bundlev1.GetBundleRequest{})
	if err != nil {
		return nil, err
	}
	bundle := x509bundle.New(i.td)
	for _, authority := range resp.GetX509Authorities() {
		cert, err := x509.ParseCertificate(authority.GetAsn1())
		if err != nil {
			return nil, err
		}
		bundle.AddX509Authority(cert)
	}
	return bundle, nil
}
