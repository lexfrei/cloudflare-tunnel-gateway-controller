package tunnel_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cloudflare/cloudflared/tracing"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
)

// countingPanicHandler panics with a chosen value and counts the panics the
// adapter reports back through tunnel.PanicRecorder.
type countingPanicHandler struct {
	value    any
	recorded atomic.Int32
}

func (h *countingPanicHandler) ServeHTTP(http.ResponseWriter, *http.Request) {
	panic(h.value)
}

func (h *countingPanicHandler) RecordContainedPanic() {
	h.recorded.Add(1)
}

var _ tunnel.PanicRecorder = (*countingPanicHandler)(nil)

// TestGatewayOriginProxy_CountsContainedPanics pins that every panic the
// adapter contains reaches a handler that records them, on both branches,
// while the routine client-abort sentinel does not.
func TestGatewayOriginProxy_CountsContainedPanics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     any
		upgrade   bool
		wantCount int32
	}{
		{name: "request branch", value: "handler exploded", wantCount: 1},
		{name: "upgrade branch", value: "upgrade handler exploded", upgrade: true, wantCount: 1},
		{name: "client abort is routine", value: http.ErrAbortHandler, wantCount: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handler := &countingPanicHandler{value: tt.value}
			proxy := tunnel.NewGatewayOriginProxy(handler, nil)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/x", nil)
			zlog := zerolog.Nop()

			require.NotPanics(t, func() {
				_ = proxy.ProxyHTTP(newTestResponseWriter(), tracing.NewTracedHTTPRequest(req, 0, &zlog), tt.upgrade)
			})

			assert.Equal(t, tt.wantCount, handler.recorded.Load())
		})
	}
}
