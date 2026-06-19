package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestEmitDiagnosticEvents_DeduplicatesIdenticalDiagnostics pins the
// per-sync Event dedup: a route living in several data-plane partitions
// converts once per partition and can therefore carry the SAME diagnostic
// twice — the operator must see one Event per distinct message, not one per
// partition.
func TestEmitDiagnosticEvents_DeduplicatesIdenticalDiagnostics(t *testing.T) {
	t.Parallel()

	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"}}

	duplicate := proxy.RouteDiagnostic{
		Namespace: "default", Name: "r",
		Target:  proxy.DiagnosticShadowed,
		Message: "rule 0 is shadowed by HTTPRoute team-a/app rule 0",
	}
	distinct := proxy.RouteDiagnostic{
		Namespace: "default", Name: "r",
		Target:    proxy.DiagnosticEvent,
		EventType: "Normal",
		Message:   "appProtocol hint superseded by BackendTLSPolicy",
	}

	rec := events.NewFakeRecorder(10)
	emitDiagnosticEvents(rec, route, []proxy.RouteDiagnostic{duplicate, duplicate, distinct, duplicate})

	emitted := drainEvents(rec)
	assert.Len(t, emitted, 2, "one Event per distinct (target, message), duplicates collapsed")
}
