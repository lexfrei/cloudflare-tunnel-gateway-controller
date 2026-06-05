package proxy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
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
