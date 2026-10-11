package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/listenermerge"
)

func httpsListener(name, hostname string) gatewayv1.Listener {
	return gatewayv1.Listener{
		Name:     gatewayv1.SectionName(name),
		Port:     443,
		Protocol: gatewayv1.HTTPSProtocolType,
		Hostname: new(gatewayv1.Hostname(hostname)),
	}
}

// TestGatewayReconciler_OverlappingTLSConfig pins the spec's example for
// ListenerConditionOverlappingTLSConfig: listeners foo.example.com and
// *.example.com overlap and both carry the condition, foo.example.org carries
// none, and the condition gates neither Accepted nor Programmed.
func TestGatewayReconciler_OverlappingTLSConfig(t *testing.T) {
	t.Parallel()

	statuses := reconcileGatewayListeners(t, []gatewayv1.Listener{
		httpsListener("a", "foo.example.com"),
		httpsListener("b", "foo.example.org"),
		httpsListener("c", "*.example.com"),
	})
	require.Len(t, statuses, 3)

	for _, status := range statuses {
		cond := findCondition(status.Conditions, string(gatewayv1.ListenerConditionOverlappingTLSConfig))

		if status.Name == "b" {
			assert.Nil(t, cond, "the condition is never written False")

			continue
		}

		require.NotNil(t, cond, "listener %s overlaps", status.Name)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, string(gatewayv1.ListenerReasonOverlappingHostnames), cond.Reason)

		accepted := findCondition(status.Conditions, string(gatewayv1.ListenerConditionAccepted))
		require.NotNil(t, accepted)
		assert.Equal(t, metav1.ConditionTrue, accepted.Status)
	}
}

// TestBuildListenerSetEntryStatuses_OverlappingTLSConfig pins that a
// ListenerSet entry overlapping a listener of its parent Gateway carries the
// condition on its own entry status.
func TestBuildListenerSetEntryStatuses_OverlappingTLSConfig(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec:       gatewayv1.GatewaySpec{Listeners: []gatewayv1.Listener{httpsListener("wild", "*.example.com")}},
	}
	listenerSet := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "infra", CreationTimestamp: metav1.NewTime(time.Unix(1000, 0))},
		Spec: gatewayv1.ListenerSetSpec{Listeners: []gatewayv1.ListenerEntry{
			{Name: "app", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, Hostname: new(gatewayv1.Hostname("app.example.com"))},
			{Name: "other", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, Hostname: new(gatewayv1.Hostname("app.example.org"))},
		}},
	}

	statuses := buildListenerSetEntryStatuses(listenerSet, listenerSetAcceptanceResult{
		MergeResult: listenermerge.Merge(gateway, []*gatewayv1.ListenerSet{listenerSet}),
	}, 1, metav1.Now())
	require.Len(t, statuses, 2)

	cond := findCondition(statuses[0].Conditions, string(gatewayv1.ListenerConditionOverlappingTLSConfig))
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonOverlappingHostnames), cond.Reason)

	assert.Nil(t, findCondition(statuses[1].Conditions, string(gatewayv1.ListenerConditionOverlappingTLSConfig)))
}

// TestPreserveConditionTransitions_DropsClearedOverlappingTLSConfig pins that
// the condition clears once the overlap is gone: it is never written False,
// so leaving the stored True in place would report an overlap that no longer
// exists.
func TestPreserveConditionTransitions_DropsClearedOverlappingTLSConfig(t *testing.T) {
	t.Parallel()

	prior := []metav1.Condition{{
		Type:   string(gatewayv1.ListenerConditionOverlappingTLSConfig),
		Status: metav1.ConditionTrue,
		Reason: string(gatewayv1.ListenerReasonOverlappingHostnames),
	}}
	desired := []metav1.Condition{{
		Type:   string(gatewayv1.ListenerConditionAccepted),
		Status: metav1.ConditionTrue,
		Reason: string(gatewayv1.ListenerReasonAccepted),
	}}

	merged := preserveConditionTransitions(prior, desired)

	assert.Nil(t, findCondition(merged, string(gatewayv1.ListenerConditionOverlappingTLSConfig)))
}
