package controller

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// drainEvents collects everything currently buffered on a FakeRecorder without
// blocking once the channel is empty.
func drainEvents(rec *events.FakeRecorder) []string {
	var got []string

	for {
		select {
		case e := <-rec.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

// TestEmitDiagnosticEvents_NormalAndWarning pins that Event-target diagnostics
// are emitted to the recorder with the right type (Normal / Warning) and the
// diagnostic message, and that non-Event diagnostics (Accepted / ResolvedRefs)
// produce no events — those are conditions, not events.
func TestEmitDiagnosticEvents_NormalAndWarning(t *testing.T) {
	t.Parallel()

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
	}

	diagnostics := []proxy.RouteDiagnostic{
		{Target: proxy.DiagnosticEvent, EventType: proxy.EventTypeNormal, Message: "h2c suppressed by BackendTLSPolicy"},
		{Target: proxy.DiagnosticEvent, EventType: proxy.EventTypeWarning, Message: "ResponseHeaderModifier strips Sec-WebSocket-Accept"},
		{Target: proxy.DiagnosticAccepted, Reason: string(gatewayv1.RouteReasonUnsupportedValue), Message: "a condition, not an event"},
	}

	rec := events.NewFakeRecorder(10)
	emitDiagnosticEvents(rec, route, diagnostics)

	got := drainEvents(rec)
	require.Len(t, got, 2, "only the two Event-target diagnostics must produce events")

	var normal, warning string

	for _, e := range got {
		switch {
		case strings.HasPrefix(e, corev1.EventTypeNormal+" "):
			normal = e
		case strings.HasPrefix(e, corev1.EventTypeWarning+" "):
			warning = e
		}
	}

	assert.Contains(t, normal, "h2c suppressed", "Normal event must carry the benign-override message")
	assert.Contains(t, warning, "Sec-WebSocket-Accept", "Warning event must carry the conflict message")
}

// TestEmitDiagnosticEvents_NilRecorder is a no-op-safety pin: a nil recorder
// (e.g. a reconciler constructed in a test without one) must not panic.
func TestEmitDiagnosticEvents_NilRecorder(t *testing.T) {
	t.Parallel()

	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}}
	diagnostics := []proxy.RouteDiagnostic{
		{Target: proxy.DiagnosticEvent, EventType: proxy.EventTypeNormal, Message: "no recorder"},
	}

	assert.NotPanics(t, func() { emitDiagnosticEvents(nil, route, diagnostics) })
}
