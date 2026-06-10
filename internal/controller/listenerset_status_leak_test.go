package controller

// Pins the spec recommendation that ListenerSet status must not leak
// information about other resources: a child's conditions may report its own
// verdicts and operational errors, but never the parent Gateway's or a
// sibling ListenerSet's Secret contents (TLS keys, cert payloads).

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestListenerSetStatus_NeverLeaksSecretContents(t *testing.T) {
	t.Parallel()

	const (
		parentSecretMarker  = "PARENT-TLS-KEY-MARKER-3f9c"
		siblingSecretMarker = "SIBLING-TLS-KEY-MARKER-a71e"
	)

	from := gatewayv1.NamespacesFromSame

	gc := managedGatewayClass()
	parentSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-cert", Namespace: "infra"},
		Data: map[string][]byte{
			"tls.crt": []byte(parentSecretMarker),
			"tls.key": []byte(parentSecretMarker),
		},
	}
	siblingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sibling-cert", Namespace: "infra"},
		Data: map[string][]byte{
			"tls.crt": []byte(siblingSecretMarker),
			"tls.key": []byte(siblingSecretMarker),
		},
	}
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &from},
			},
			Listeners: []gatewayv1.Listener{
				{
					Name: "gw-https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
					TLS: &gatewayv1.ListenerTLSConfig{
						CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "parent-cert"}},
					},
				},
			},
		},
	}
	// The sibling conflicts with the reconciled child on (port, protocol,
	// hostname) so its existence shows up in the child's conflict verdicts --
	// the most leak-prone path.
	sibling := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ls-older", Namespace: "infra",
			CreationTimestamp: metav1.Time{Time: metav1.Now().Add(-3600e9)},
		},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: "dup", Port: 8443, Protocol: gatewayv1.HTTPSProtocolType,
					TLS: &gatewayv1.ListenerTLSConfig{
						CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "sibling-cert"}},
					},
				},
			},
		},
	}
	child := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls-younger", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: "dup", Port: 8443, Protocol: gatewayv1.HTTPSProtocolType,
					TLS: &gatewayv1.ListenerTLSConfig{
						CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "sibling-cert"}},
					},
				},
			},
		},
	}

	scheme := newListenerSetScheme(t)
	require.NoError(t, corev1.AddToScheme(scheme))
	r, cli := newListenerSetReconcilerWithObjects(t, scheme, gc, gw, sibling, child, parentSecret, siblingSecret)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: child.Name, Namespace: child.Namespace},
	})
	require.NoError(t, err)

	updated := getListenerSet(t, cli, child.Name, child.Namespace)

	statusJSON, err := json.Marshal(updated.Status)
	require.NoError(t, err)

	assert.NotContains(t, string(statusJSON), parentSecretMarker,
		"ListenerSet status must not echo the parent Gateway's Secret contents")
	assert.NotContains(t, string(statusJSON), siblingSecretMarker,
		"ListenerSet status must not echo a sibling's Secret contents")
}

// TestListenerSetReconciler_GRPCRouteCountedOncePerEntry pins two
// AttachedRoutes MUSTs for the ListenerSet path: GRPCRoutes attached to a
// ListenerSet entry are counted (parity with HTTPRoute), and a route listing
// the same entry through multiple parentRefs (degenerate but legal) counts
// at most once.
func TestListenerSetReconciler_GRPCRouteCountedOncePerEntry(t *testing.T) {
	t.Parallel()

	from := gatewayv1.NamespacesFromSame
	section := gatewayv1.SectionName("grpc")
	lsKind := gatewayv1.Kind("ListenerSet")

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &from},
			},
			Listeners: []gatewayv1.Listener{
				{Name: "gw-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: section, Port: 8080, Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}
	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "grpc-r", Namespace: "infra"},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				// Two parentRefs naming the same ListenerSet section: legal,
				// degenerate, must count once.
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: gatewayv1.ObjectName(ls.Name), SectionName: &section},
					{Kind: &lsKind, Name: gatewayv1.ObjectName(ls.Name), SectionName: &section},
				},
			},
		},
	}

	scheme := newListenerSetScheme(t)
	r, cli := newListenerSetReconcilerWithObjects(t, scheme, gc, gw, ls, route)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ls.Name, Namespace: ls.Namespace},
	})
	require.NoError(t, err)

	updated := getListenerSet(t, cli, ls.Name, ls.Namespace)
	require.Len(t, updated.Status.Listeners, 1)
	assert.Equal(t, int32(1), updated.Status.Listeners[0].AttachedRoutes,
		"a GRPCRoute with duplicate parentRefs to one entry must count exactly once")
}
