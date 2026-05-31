//go:build conformance

package conformance

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"google.golang.org/grpc"

	grpcconf "sigs.k8s.io/gateway-api/conformance/utils/grpc"
)

// TestBuildOutgoingMetadata pins the host-conveyance decision for the tunnel
// gRPC client: the test's own metadata is always forwarded, and the intended
// :authority (set only by the hostname-scoped GRPCRoute tests) is carried to
// the in-cluster proxy via x-original-host — the gRPC analogue of the header
// TunnelRoundTripper sets for HTTP. The wire :authority stays the edge
// hostname, so this header is the only signal the proxy can match a
// non-edge hostname on.
func TestBuildOutgoingMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		meta               *grpcconf.RequestMetadata
		wantOriginalHost   []string
		wantForwardedPairs map[string][]string
	}{
		{
			name:             "nil metadata carries nothing",
			meta:             nil,
			wantOriginalHost: nil,
		},
		{
			name:             "authority becomes x-original-host",
			meta:             &grpcconf.RequestMetadata{Authority: "example.com"},
			wantOriginalHost: []string{"example.com"},
		},
		{
			name:               "test metadata is forwarded without an authority",
			meta:               &grpcconf.RequestMetadata{Metadata: map[string]string{"magic": "foo"}},
			wantOriginalHost:   nil,
			wantForwardedPairs: map[string][]string{"magic": {"foo"}},
		},
		{
			name: "authority and metadata are both carried",
			meta: &grpcconf.RequestMetadata{
				Authority: "echo.example.com",
				Metadata:  map[string]string{"magic": "foo"},
			},
			wantOriginalHost:   []string{"echo.example.com"},
			wantForwardedPairs: map[string][]string{"magic": {"foo"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			md := buildOutgoingMetadata(tt.meta)

			assert.Equal(t, tt.wantOriginalHost, md.Get(originalHostHeader),
				"x-original-host must mirror RequestMetadata.Authority")

			for key, want := range tt.wantForwardedPairs {
				assert.Equal(t, want, md.Get(key), "test metadata %q must be forwarded", key)
			}
		})
	}
}

// TestTunnelGRPCClientCloseIsNoOp pins the shared-client lifecycle. The
// conformance suite reuses a single injected grpc.Client across a test's
// parallel sub-cases and calls Close() after every request (vendor grpc.go:
// defer c.Close()). Close() must NOT tear down the connection, or an in-flight
// RPC from a sibling sub-case fails with "the client connection is closing".
// So Close() is a no-op: the shared connection survives and the client stays
// usable across repeated Close calls.
func TestTunnelGRPCClientCloseIsNoOp(t *testing.T) {
	t.Parallel()

	client := &TunnelGRPCClient{}

	conn, err := client.connection()
	require.NoError(t, err)
	require.NotNil(t, conn, "connection must establish a ClientConn")

	client.Close()

	// After Close the same connection must still be handed out (no re-dial,
	// no teardown), so concurrent sibling sub-cases keep working.
	again, err := client.connection()
	require.NoError(t, err, "connection must remain usable after Close")
	assert.Same(t, conn, again, "Close must not drop the shared connection")

	client.Close()
}

// TestTunnelGRPCClientConnectionIsShared pins that concurrent callers (the
// parallel sub-cases) get one shared ClientConn rather than racing to dial.
func TestTunnelGRPCClientConnectionIsShared(t *testing.T) {
	t.Parallel()

	client := &TunnelGRPCClient{}

	const callers = 8

	conns := make([]*grpc.ClientConn, callers)

	var wg sync.WaitGroup
	for i := range conns {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()

			conn, err := client.connection()
			assert.NoError(t, err)
			conns[idx] = conn
		}(i)
	}

	wg.Wait()

	for i := 1; i < callers; i++ {
		assert.Same(t, conns[0], conns[i], "all concurrent callers must share one ClientConn")
	}
}
