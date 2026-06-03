package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
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

// TestEmitDiagnosticEvents_MessageWithPercentIsLiteral pins that a diagnostic
// message containing a literal % is emitted verbatim. Eventf treats its note as
// a format string, so passing diag.Message as the format (instead of a "%s"
// argument) would render "50%!(NOVERB)" garbage for any future message that
// embeds user-controlled data with a percent sign.
func TestEmitDiagnosticEvents_MessageWithPercentIsLiteral(t *testing.T) {
	t.Parallel()

	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}}
	diagnostics := []proxy.RouteDiagnostic{
		{Target: proxy.DiagnosticEvent, EventType: proxy.EventTypeNormal, Message: "appProtocol foo%bar overridden; 50% applied"},
	}

	rec := events.NewFakeRecorder(5)
	emitDiagnosticEvents(rec, route, diagnostics)

	got := drainEvents(rec)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "foo%bar", "a literal %% in the message must survive verbatim")
	assert.Contains(t, got[0], "50% applied")
	assert.NotContains(t, got[0], "NOVERB", "the message must not be mis-parsed as a format string")
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

// eventDiagSchemeAndClient builds the scheme and a fake client builder with the
// API types the reconciler event-emission tests need.
func eventDiagSchemeAndClient(t *testing.T) (*runtime.Scheme, *fake.ClientBuilder) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	return scheme, fake.NewClientBuilder().WithScheme(scheme)
}

// TestHTTPRouteReconciler_updateRouteStatus_EmitsEvent pins that the HTTPRoute
// reconciler routes an Event-target diagnostic to its Recorder during status
// update. This guards against the recorder being left unwired (the bug where
// the reconciler is constructed without a Recorder would silently swallow every
// benign-override Event in production).
func TestHTTPRouteReconciler_updateRouteStatus_EmitsEvent(t *testing.T) {
	t.Parallel()

	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}}
	scheme, builder := eventDiagSchemeAndClient(t)
	cli := builder.WithObjects(route).WithStatusSubresource(route).Build()

	rec := events.NewFakeRecorder(5)
	r := &HTTPRouteReconciler{
		Client:         cli,
		Scheme:         scheme,
		ControllerName: "test-controller",
		Recorder:       rec,
	}

	diagnostics := []proxy.RouteDiagnostic{
		{Namespace: "default", Name: "web", Target: proxy.DiagnosticEvent, EventType: proxy.EventTypeNormal, Message: "h2c suppressed by BackendTLSPolicy"},
	}

	require.NoError(t, r.updateRouteStatus(context.Background(), route, routeBindingInfo{}, nil, diagnostics, nil))

	got := drainEvents(rec)
	require.Len(t, got, 1, "the HTTPRoute reconciler must emit the Event-target diagnostic")
	assert.Contains(t, got[0], "h2c suppressed")
}

// TestGRPCRouteReconciler_updateRouteStatus_EmitsEvent is the same pin for the
// GRPCRoute reconciler — it caught a real bug where the reconciler was
// constructed in manager.go without a Recorder, so its Events were dropped.
func TestGRPCRouteReconciler_updateRouteStatus_EmitsEvent(t *testing.T) {
	t.Parallel()

	route := &gatewayv1.GRPCRoute{ObjectMeta: metav1.ObjectMeta{Name: "grpc", Namespace: "default"}}
	scheme, builder := eventDiagSchemeAndClient(t)
	cli := builder.WithObjects(route).WithStatusSubresource(route).Build()

	rec := events.NewFakeRecorder(5)
	r := &GRPCRouteReconciler{
		Client:         cli,
		Scheme:         scheme,
		ControllerName: "test-controller",
		Recorder:       rec,
	}

	diagnostics := []proxy.RouteDiagnostic{
		{Namespace: "default", Name: "grpc", Target: proxy.DiagnosticEvent, EventType: proxy.EventTypeNormal, Message: "h2c suppressed by BackendTLSPolicy"},
	}

	require.NoError(t, r.updateRouteStatus(context.Background(), route, routeBindingInfo{}, nil, diagnostics, nil))

	// updateRouteStatus also emits the unconditional gRPC edge-toggle breadcrumb
	// on this gRPC-capable transport, so filter to the diagnostic Event under test
	// rather than asserting a single total.
	var diagEvents []string

	for _, e := range drainEvents(rec) {
		if strings.Contains(e, "h2c suppressed") {
			diagEvents = append(diagEvents, e)
		}
	}

	require.Len(t, diagEvents, 1, "the GRPCRoute reconciler must emit the Event-target diagnostic")
	assert.Contains(t, diagEvents[0], "h2c suppressed")
}
