//go:build conformance

package conformance

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "sigs.k8s.io/gateway-api/conformance/echo-basic/grpcechoserver"
	"sigs.k8s.io/gateway-api/conformance/utils/config"
	grpcconf "sigs.k8s.io/gateway-api/conformance/utils/grpc"
)

// errNoEchoRequest is returned when an ExpectedResponse specifies none of the
// Echo request variants — mirrors the conformance DefaultClient's behaviour
// with a sentinel error (err113 forbids the inline fmt.Errorf the upstream
// vendored client uses).
var errNoEchoRequest = errors.New("no request specified")

// TunnelGRPCClient is the gRPC counterpart of TunnelRoundTripper: it satisfies
// the conformance suite's grpc.Client interface but routes every RPC through
// the Cloudflare edge instead of dialing the Gateway status address directly.
//
// The Gateway address the suite hands to SendRPC is a tunnel CNAME
// (*.cfargotunnel.com → Cloudflare ULA fd10::/8) that is not routable from a
// test runner, so the default client cannot reach it. We dial the real tunnel
// hostname over TLS (ALPN h2) instead, keep the wire :authority equal to the
// edge hostname so Cloudflare accepts and routes the stream to our tunnel, and
// carry the test's intended :authority to the in-cluster proxy via
// x-original-host — exactly the split TunnelRoundTripper uses for HTTP.
//
// Concurrency: the conformance suite injects ONE shared client (opts.GRPCClient)
// and a single ConformanceTest fans its sub-cases out with t.Parallel(), so
// SendRPC is called concurrently. A grpc.ClientConn is safe for concurrent RPCs
// and manages its own transport reconnection, so all callers share one lazily
// dialed connection guarded by mu. Close() is therefore a no-op (see its doc).
//
// gRPC only survives the tunnel on the http2 transport: cloudflared's QUIC
// adapter drops HTTP trailers, where grpc-status lives, so the conformance run
// must pin proxy.tunnel.protocol=http2.
type TunnelGRPCClient struct {
	Debug         bool
	TimeoutConfig config.TimeoutConfig

	mu   sync.Mutex
	conn *grpc.ClientConn
}

// buildOutgoingMetadata builds the gRPC metadata for a request routed through
// the edge. The test's own metadata is always forwarded (the header-matching
// test relies on it); when the suite pins a specific :authority (the
// hostname-scoped tests) it is carried to the proxy via x-original-host, since
// the wire :authority must stay the edge hostname for Cloudflare to route.
func buildOutgoingMetadata(reqMeta *grpcconf.RequestMetadata) metadata.MD {
	outgoing := metadata.MD{}
	if reqMeta == nil {
		return outgoing
	}

	for key, value := range reqMeta.Metadata {
		outgoing.Append(key, value)
	}

	if reqMeta.Authority != "" {
		outgoing.Set(originalHostHeader, reqMeta.Authority)
	}

	return outgoing
}

// SendRPC implements grpc.Client. The address argument (the unroutable Gateway
// status address) is intentionally ignored: every RPC is dialed through the
// edge in connection().
//
//nolint:gocritic // expected is passed by value to match the grpc.Client interface signature exactly.
func (c *TunnelGRPCClient) SendRPC(
	t *testing.T,
	address string,
	expected grpcconf.ExpectedResponse,
	timeout time.Duration,
) (*grpcconf.Response, error) {
	t.Helper()

	if c.Debug {
		t.Logf("TunnelGRPCClient: dialing edge %s (suite gateway address %q ignored — not routable)",
			tunnelHostname(), address)
	}

	conn, err := c.connection()
	if err != nil {
		return &grpcconf.Response{}, err
	}

	resp := &grpcconf.Response{
		Headers:  &metadata.MD{},
		Trailers: &metadata.MD{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ctx = metadata.NewOutgoingContext(ctx, buildOutgoingMetadata(expected.RequestMetadata))

	stub := pb.NewGrpcEchoClient(conn)

	switch {
	case expected.EchoRequest != nil:
		resp.Response, err = stub.Echo(ctx, expected.EchoRequest, grpc.Header(resp.Headers), grpc.Trailer(resp.Trailers))
	case expected.EchoTwoRequest != nil:
		resp.Response, err = stub.EchoTwo(ctx, expected.EchoTwoRequest, grpc.Header(resp.Headers), grpc.Trailer(resp.Trailers))
	case expected.EchoThreeRequest != nil:
		resp.Response, err = stub.EchoThree(ctx, expected.EchoThreeRequest, grpc.Header(resp.Headers), grpc.Trailer(resp.Trailers))
	default:
		return resp, errNoEchoRequest
	}

	if err != nil {
		resp.Code = status.Code(err)
		t.Logf("TunnelGRPCClient: RPC finished with error: %v", err)

		return resp, nil
	}

	resp.Code = codes.OK

	return resp, nil
}

// Close is a no-op. The conformance suite calls Close() after every request
// (vendor grpc.go: defer c.Close()), but this single client is shared across
// all of a test's parallel sub-cases and reused for the whole suite run, so
// tearing the connection down per-request would break in-flight RPCs from
// sibling sub-cases with "the client connection is closing". The shared
// grpc.ClientConn manages its own transport reconnection and is reclaimed when
// the test process exits.
func (c *TunnelGRPCClient) Close() {}

// connection lazily dials the tunnel edge once and returns the shared, mutex-
// guarded connection. Concurrent callers (parallel sub-cases) get the same
// *grpc.ClientConn, which is safe for concurrent RPCs.
func (c *TunnelGRPCClient) connection() (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		return c.conn, nil
	}

	edgeHost := tunnelHostname()

	conn, err := grpc.NewClient(
		net.JoinHostPort(edgeHost, "443"),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: edgeHost,
		})),
		// :authority must be a hostname Cloudflare recognises, otherwise the
		// edge rejects the stream before it reaches our tunnel. The intended
		// host travels in x-original-host metadata instead.
		grpc.WithAuthority(edgeHost),
	)
	if err != nil {
		return nil, fmt.Errorf("dialing tunnel edge: %w", err)
	}

	c.conn = conn

	return c.conn, nil
}
