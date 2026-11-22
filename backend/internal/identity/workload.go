package identity

import (
	"bytes"
	"context"

	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	delegatedidentityv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/agent/delegatedidentity/v1"
	"github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// workloadAPI is a SPIFFE Workload API for one session, answered from the
// SPIRE agent's Delegated Identity API with the session's selectors.
type workloadAPI struct {
	workload.UnimplementedSpiffeWorkloadAPIServer
	client    delegatedidentityv1.DelegatedIdentityClient
	selectors []*types.Selector
}

// checkHeader enforces the header the Workload API spec requires against
// server-side request forgery.
func checkHeader(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if values := md.Get("workload.spiffe.io"); len(values) != 1 || values[0] != "true" {
		return status.Error(codes.InvalidArgument, "security header missing from request")
	}
	return nil
}

var errNoIdentity = status.Error(codes.PermissionDenied, "no identity issued")

func (w *workloadAPI) FetchX509SVID(_ *workload.X509SVIDRequest, stream grpc.ServerStreamingServer[workload.X509SVIDResponse]) error {
	ctx := stream.Context()
	if err := checkHeader(ctx); err != nil {
		return err
	}
	svids, err := w.client.SubscribeToX509SVIDs(ctx, &delegatedidentityv1.SubscribeToX509SVIDsRequest{Selectors: w.selectors})
	if err != nil {
		return err
	}
	bundles, err := w.client.SubscribeToX509Bundles(ctx, &delegatedidentityv1.SubscribeToX509BundlesRequest{})
	if err != nil {
		return err
	}
	svidUpdates := relay(ctx, svids.Recv)
	bundleUpdates := relay(ctx, bundles.Recv)
	var current *delegatedidentityv1.SubscribeToX509SVIDsResponse
	var roots map[string][]byte
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case update := <-svidUpdates:
			if update.err != nil {
				return update.err
			}
			if len(update.value.X509Svids) == 0 {
				return errNoIdentity
			}
			current = update.value
		case update := <-bundleUpdates:
			if update.err != nil {
				return update.err
			}
			roots = update.value.CaCertificates
		}
		if current == nil || roots == nil {
			continue
		}
		if err := stream.Send(x509Response(current, roots)); err != nil {
			return err
		}
	}
}

func x509Response(svids *delegatedidentityv1.SubscribeToX509SVIDsResponse, roots map[string][]byte) *workload.X509SVIDResponse {
	resp := &workload.X509SVIDResponse{FederatedBundles: map[string][]byte{}}
	for _, svid := range svids.X509Svids {
		id := svid.X509Svid.GetId()
		resp.Svids = append(resp.Svids, &workload.X509SVID{
			SpiffeId:    "spiffe://" + id.GetTrustDomain() + id.GetPath(),
			X509Svid:    bytes.Join(svid.X509Svid.GetCertChain(), nil),
			X509SvidKey: svid.X509SvidKey,
			Bundle:      roots["spiffe://"+id.GetTrustDomain()],
			Hint:        svid.X509Svid.GetHint(),
		})
	}
	for _, td := range svids.FederatesWith {
		if bundle, ok := roots[td]; ok {
			resp.FederatedBundles[td] = bundle
		}
	}
	return resp
}

func (w *workloadAPI) FetchX509Bundles(_ *workload.X509BundlesRequest, stream grpc.ServerStreamingServer[workload.X509BundlesResponse]) error {
	ctx := stream.Context()
	if err := checkHeader(ctx); err != nil {
		return err
	}
	bundles, err := w.client.SubscribeToX509Bundles(ctx, &delegatedidentityv1.SubscribeToX509BundlesRequest{})
	if err != nil {
		return err
	}
	for {
		update, err := bundles.Recv()
		if err != nil {
			return err
		}
		if err := stream.Send(&workload.X509BundlesResponse{Bundles: update.CaCertificates}); err != nil {
			return err
		}
	}
}

func (w *workloadAPI) FetchJWTSVID(ctx context.Context, req *workload.JWTSVIDRequest) (*workload.JWTSVIDResponse, error) {
	if err := checkHeader(ctx); err != nil {
		return nil, err
	}
	if len(req.Audience) == 0 {
		return nil, status.Error(codes.InvalidArgument, "audience must be specified")
	}
	fetched, err := w.client.FetchJWTSVIDs(ctx, &delegatedidentityv1.FetchJWTSVIDsRequest{Audience: req.Audience, Selectors: w.selectors})
	if err != nil {
		return nil, err
	}
	resp := &workload.JWTSVIDResponse{}
	for _, svid := range fetched.Svids {
		id := "spiffe://" + svid.GetId().GetTrustDomain() + svid.GetId().GetPath()
		if req.SpiffeId != "" && req.SpiffeId != id {
			continue
		}
		resp.Svids = append(resp.Svids, &workload.JWTSVID{SpiffeId: id, Svid: svid.Token})
	}
	if len(resp.Svids) == 0 {
		return nil, errNoIdentity
	}
	return resp, nil
}

func (w *workloadAPI) FetchJWTBundles(_ *workload.JWTBundlesRequest, stream grpc.ServerStreamingServer[workload.JWTBundlesResponse]) error {
	ctx := stream.Context()
	if err := checkHeader(ctx); err != nil {
		return err
	}
	bundles, err := w.client.SubscribeToJWTBundles(ctx, &delegatedidentityv1.SubscribeToJWTBundlesRequest{})
	if err != nil {
		return err
	}
	for {
		update, err := bundles.Recv()
		if err != nil {
			return err
		}
		if err := stream.Send(&workload.JWTBundlesResponse{Bundles: update.Bundles}); err != nil {
			return err
		}
	}
}

func (w *workloadAPI) ValidateJWTSVID(ctx context.Context, req *workload.ValidateJWTSVIDRequest) (*workload.ValidateJWTSVIDResponse, error) {
	if err := checkHeader(ctx); err != nil {
		return nil, err
	}
	if req.Audience == "" || req.Svid == "" {
		return nil, status.Error(codes.InvalidArgument, "audience and svid must be specified")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := w.client.SubscribeToJWTBundles(ctx, &delegatedidentityv1.SubscribeToJWTBundlesRequest{})
	if err != nil {
		return nil, err
	}
	update, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	set := jwtbundle.NewSet()
	for id, jwks := range update.Bundles {
		td, err := spiffeid.TrustDomainFromString(id)
		if err != nil {
			continue
		}
		bundle, err := jwtbundle.Parse(td, jwks)
		if err != nil {
			continue
		}
		set.Add(bundle)
	}
	svid, err := jwtsvid.ParseAndValidate(req.Svid, set, []string{req.Audience})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	claims, err := structpb.NewStruct(svid.Claims)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &workload.ValidateJWTSVIDResponse{SpiffeId: svid.ID.String(), Claims: claims}, nil
}

type update[T any] struct {
	value T
	err   error
}

// relay turns a blocking receive into a channel that closes with the context.
func relay[T any](ctx context.Context, recv func() (T, error)) <-chan update[T] {
	ch := make(chan update[T])
	go func() {
		for {
			value, err := recv()
			select {
			case ch <- update[T]{value: value, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}
