package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestWithEffectiveHostnames_InheritsFromGatewayListener(t *testing.T) {
	t.Parallel()

	gatewayHost := gatewayv1.Hostname("gw-listener.example.com")
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &gatewayHost},
			},
		},
	}

	gwKind := gatewayv1.Kind(kindGateway)
	gwNS := gatewayv1.Namespace("infra")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &gwKind, Name: "gw", Namespace: &gwNS},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t, gw)

	out := withEffectiveHostnames(context.Background(), cli, []*gatewayv1.HTTPRoute{route})
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{gatewayHost}, out[0].Spec.Hostnames)
}

func TestWithEffectiveHostnames_InheritsFromListenerSetEntry(t *testing.T) {
	t.Parallel()

	entryHost := gatewayv1.Hostname("ls.example.com")
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "only", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &entryHost},
			},
		},
	}

	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: "ls", Namespace: &lsNS},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t, ls)

	out := withEffectiveHostnames(context.Background(), cli, []*gatewayv1.HTTPRoute{route})
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{entryHost}, out[0].Spec.Hostnames)
}

func TestWithEffectiveHostnames_SectionNameNarrowsListenerSetEntries(t *testing.T) {
	t.Parallel()

	a := gatewayv1.Hostname("a.example.com")
	b := gatewayv1.Hostname("b.example.com")
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "first", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &a},
				{Name: "second", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &b},
			},
		},
	}

	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")
	section := gatewayv1.SectionName("second")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: "ls", Namespace: &lsNS, SectionName: &section},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t, ls)

	out := withEffectiveHostnames(context.Background(), cli, []*gatewayv1.HTTPRoute{route})
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{b}, out[0].Spec.Hostnames, "only the sectionName-matched entry's hostname should be inherited")
}

func TestWithEffectiveHostnames_NoOpWhenRouteAlreadyDeclaresHostnames(t *testing.T) {
	t.Parallel()

	declared := gatewayv1.Hostname("override.example.com")
	listenerHost := gatewayv1.Hostname("ls.example.com")
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "only", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &listenerHost},
			},
		},
	}

	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: "ls", Namespace: &lsNS},
				},
			},
			Hostnames: []gatewayv1.Hostname{declared},
		},
	}

	cli := buildGatewayFakeClient(t, ls)

	out := withEffectiveHostnames(context.Background(), cli, []*gatewayv1.HTTPRoute{route})
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{declared}, out[0].Spec.Hostnames, "explicit route hostnames must not be overwritten")
}

func TestWithEffectiveHostnames_StableWhenParentMissing(t *testing.T) {
	t.Parallel()

	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: "missing", Namespace: &lsNS},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t)

	out := withEffectiveHostnames(context.Background(), cli, []*gatewayv1.HTTPRoute{route})
	require.Len(t, out, 1)
	assert.Empty(t, out[0].Spec.Hostnames, "missing parent must not synthesise hostnames")
}

// buildGatewayFakeClient registers the gateway-api v1 scheme and seeds the
// fake client with the given objects.
func buildGatewayFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range objs {
		builder = builder.WithObjects(obj)
	}

	return builder.Build()
}
