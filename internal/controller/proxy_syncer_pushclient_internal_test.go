package controller

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProxyPushClient_OwnsIsolatedTransport pins that the config-push client
// owns its transport instead of falling back to the process-global
// http.DefaultTransport. A nil Transport makes net/http use DefaultTransport,
// which every ProxySyncer in the process then shares. A CloseIdleConnections
// call on that shared transport — from unrelated code, or a sibling parallel
// test tearing down its own httptest server — can break an in-flight config
// push, surfacing as "transport connection broken: http: CloseIdleConnections
// called" and a flaky TestProxySyncer_SyncRoutes_* failure.
func TestProxyPushClient_OwnsIsolatedTransport(t *testing.T) {
	t.Parallel()

	client := proxyPushClient(false)

	require.NotNil(t, client.Transport,
		"push client must set Transport — a nil Transport makes net/http use the shared http.DefaultTransport")
	assert.NotSame(t, http.DefaultTransport, client.Transport,
		"push client must own its transport, not share the process-global http.DefaultTransport")
}

// TestProxyPushClient_TracingStillOwnsIsolatedTransport pins the same isolation
// with tracing enabled: the otelhttp wrapper must wrap the controller's own
// transport, never the shared DefaultTransport.
func TestProxyPushClient_TracingStillOwnsIsolatedTransport(t *testing.T) {
	t.Parallel()

	client := proxyPushClient(true)

	require.NotNil(t, client.Transport,
		"tracing push client must set Transport")
	assert.NotSame(t, http.DefaultTransport, client.Transport,
		"tracing push client must wrap its own transport, not the shared http.DefaultTransport")
}
