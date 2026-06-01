package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// TestProxyPushClient_TracingTogglesTransport pins that the config-push client
// wraps its transport with otelhttp only when tracing is enabled; disabled
// leaves Transport nil so the stdlib default transport is used unchanged.
func TestProxyPushClient_TracingTogglesTransport(t *testing.T) {
	t.Parallel()

	plain := proxyPushClient(false)
	assert.Nil(t, plain.Transport,
		"tracing disabled must leave Transport nil (stdlib default, outbound path unchanged)")

	traced := proxyPushClient(true)

	_, ok := traced.Transport.(*otelhttp.Transport)
	assert.True(t, ok, "tracing enabled must wrap the transport with otelhttp")
}

// TestWithSyncerTracing_SetsFlag pins the option wiring.
func TestWithSyncerTracing_SetsFlag(t *testing.T) {
	t.Parallel()

	var settings proxySyncerSettings

	assert.False(t, settings.tracing)

	WithSyncerTracing()(&settings)
	assert.True(t, settings.tracing)
}
