package proxy

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsTLSAppProtocol pins the TLS-vs-cleartext classification shared by the
// HTTP switch and the gRPC transport path: only https / HTTPS / kubernetes.io/wss
// select TLS (and so require a BackendTLSPolicy). Every other value — including
// the cleartext KEP-3726 hints and unrecognised strings — is non-TLS.
func TestIsTLSAppProtocol(t *testing.T) {
	t.Parallel()

	cases := []struct {
		appProto string
		want     bool
	}{
		{"https", true},
		{"HTTPS", true},
		{"kubernetes.io/wss", true},
		{"", false},
		{"http", false},
		{"HTTP", false},
		{"kubernetes.io/h2c", false},
		{"kubernetes.io/ws", false},
		{"my-custom-proto", false},
	}

	for _, tc := range cases {
		t.Run(tc.appProto, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isTLSAppProtocol(tc.appProto))
		})
	}
}

// TestLookupAppProtocol confirms the nil-resolver guard returns "" and a wired
// resolver's value is passed through verbatim.
func TestLookupAppProtocol(t *testing.T) {
	t.Parallel()

	assert.Empty(t, lookupAppProtocol(context.Background(), nil, "ns", "svc", 443),
		"a nil resolver must resolve to the empty appProtocol")

	resolver := func(_ context.Context, namespace, serviceName string, port int32) string {
		assert.Equal(t, "ns", namespace)
		assert.Equal(t, "svc", serviceName)
		assert.Equal(t, int32(8443), port)

		return "https"
	}
	assert.Equal(t, "https", lookupAppProtocol(context.Background(), resolver, "ns", "svc", 8443))
}

// TestMarkInvalidBackend pins the spec MUST that an invalid backendRef stays
// in the weighted pool with UnavailableStatus=500 for its traffic fraction,
// and the complementary rule that a zero-weight invalid backend is dropped
// outright (zero weight means no traffic fraction to poison).
func TestMarkInvalidBackend(t *testing.T) {
	t.Parallel()

	t.Run("positive weight keeps the backend with a 500 marker", func(t *testing.T) {
		t.Parallel()

		ref, kept := markInvalidBackend(3, "svc", "ns", 80, "cluster.local", "invalid kind")

		require.True(t, kept)
		assert.Equal(t, int32(3), ref.Weight, "the invalid backend must keep its weight so the 500 fraction matches the spec")
		assert.Equal(t, http.StatusInternalServerError, ref.UnavailableStatus)
	})

	t.Run("zero weight drops the backend", func(t *testing.T) {
		t.Parallel()

		_, kept := markInvalidBackend(0, "svc", "ns", 80, "cluster.local", "invalid kind")
		assert.False(t, kept, "zero-weight invalid backends carry no traffic fraction and must be dropped")
	})

	t.Run("out-of-range port falls back to the default service port in the placeholder URL", func(t *testing.T) {
		t.Parallel()

		ref, kept := markInvalidBackend(1, "svc", "ns", 70000, "cluster.local", "invalid port")

		require.True(t, kept)
		assert.Contains(t, ref.URL, ":80", "an unbuildable port must fall back to the default placeholder port")
	})
}
