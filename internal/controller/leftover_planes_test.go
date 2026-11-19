package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

// planeDeployment builds a Gateway-controlled rendered-plane Deployment.
func planeDeployment(name, namespace, gateway string) *appsv1.Deployment {
	controllerTrue := true

	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: namespace,
		Labels:    map[string]string{render.GatewayLabel: gateway},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "gateway.networking.k8s.io/v1",
			Kind:       "Gateway",
			Name:       gateway,
			UID:        types.UID("uid-" + gateway),
			Controller: &controllerTrue,
		}},
	}}
}

// TestLeftoverDedicatedPlanes pins the startup detection the controller runs when
// per-Gateway data planes are disabled: a previously-rendered dedicated proxy
// Deployment (Gateway-controlled, carrying the render label, and whose
// Gateway's class THIS controller manages) cannot be torn down — the infra
// reconciler is off and the write RBAC is withdrawn — so it keeps serving its
// last-pushed routes while its Gateway reports Accepted=False. The operator
// must hear about each one; this is what finds them.
//
// The scope matters as much as the find: only Gateway-controlled Deployments
// count (the render label alone can be put on anything by anyone able to
// label a Deployment), and only Gateways whose GatewayClass carries THIS
// controller's controllerName — a second installation's healthy, actively
// managed planes must never be reported as leftovers of this one, or the
// warning would tell the operator to delete the neighbour's Gateways.
func TestLeftoverDedicatedPlanes(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))

	objs := []struct{ class, controller string }{
		{"ours", "test-controller"},
		{"theirs", "other-controller"},
	}

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		builder = builder.WithObjects(&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: o.class},
			Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewayv1.GatewayController(o.controller)},
		})
	}

	gatewayOf := func(name, namespace, class string) *gatewayv1.Gateway {
		return &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: gatewayv1.ObjectName(class)},
		}
	}

	cli := builder.WithObjects(
		// Ours: reported.
		gatewayOf("edge", "tenant-a", "ours"),
		planeDeployment("cf-proxy-edge", "tenant-a", "edge"),
		// Another installation's healthy plane: same shape, foreign
		// controllerName — never reported.
		gatewayOf("edge", "tenant-b", "theirs"),
		planeDeployment("cf-proxy-edge", "tenant-b", "edge"),
		// Gateway already deleted: ownerRef GC collects the Deployment, so
		// there is nothing for the operator to act on — not reported.
		planeDeployment("cf-proxy-gone", "tenant-c", "gone"),
		// Label without the Gateway owner: not a rendered plane.
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name:      "labelled-by-hand",
			Namespace: "tenant-d",
			Labels:    map[string]string{render.GatewayLabel: "whatever"},
		}},
	).Build()

	found, err := leftoverDedicatedPlanes(context.Background(), cli, "test-controller")
	require.NoError(t, err)
	assert.Equal(t, []string{"tenant-a/cf-proxy-edge (Gateway edge)"}, found)
}
