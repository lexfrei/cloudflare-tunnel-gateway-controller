package routebinding

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
)

// failingNamespaceClient serves the given objects but fails every Namespace
// read with readErr.
func failingNamespaceClient(readErr error, objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = gatewayv1.Install(scheme)

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Namespace); ok {
					return readErr
				}

				return cli.Get(ctx, key, obj, opts...)
			},
		}).Build()
}

func teamSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}
}

// TestNamespaceReadError_IsReturned pins that a route namespace that cannot be
// read leaves the binding undecided: the error reaches the caller, which
// records the parent Pending and retries, instead of a NotAllowedByListeners
// refusal nothing brings back. A namespace that does not exist is a decision.
func TestNamespaceReadError_IsReturned(t *testing.T) {
	t.Parallel()

	fromSelector := gatewayv1.NamespacesFromSelector
	route := &RouteInfo{Namespace: "apps", Kind: KindHTTPRoute}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{selectorListener("http", teamSelector())},
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &fromSelector, Selector: teamSelector()},
			},
		},
	}

	listenerSet := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "apps"},
		Spec: gatewayv1.ListenerSetSpec{
			Listeners: []gatewayv1.ListenerEntry{{
				Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
				AllowedRoutes: selectorListener("http", teamSelector()).AllowedRoutes,
			}},
		},
	}

	checks := map[string]func(*Validator) error{
		"gateway listener": func(v *Validator) error {
			_, err := v.ValidateBinding(context.Background(), gateway, route)

			return err
		},
		"listenerset entry": func(v *Validator) error {
			_, err := v.ValidateBindingForListenerSet(context.Background(), listenerSet, route)

			return err
		},
		"allowedListeners": func(v *Validator) error {
			_, err := v.EvaluateListenerSetAcceptance(context.Background(), gateway, listenerSet)

			return err
		},
	}

	for name, check := range checks {
		t.Run(name+" transient", func(t *testing.T) {
			t.Parallel()

			err := check(NewValidator(failingNamespaceClient(apierrors.NewTimeoutError("slow", 1))))
			require.Error(t, err)
		})

		t.Run(name+" not found", func(t *testing.T) {
			t.Parallel()

			notFound := apierrors.NewNotFound(corev1.Resource("namespaces"), "apps")
			require.NoError(t, check(NewValidator(failingNamespaceClient(notFound))))
		})
	}
}

// TestNamespaceReadError_NotReportedAsInvalidSelector pins that an
// unreadable namespace is not mistaken for an unparseable selector on a
// sibling listener's behalf: a read error decides nothing about the listener.
func TestNamespaceReadError_NotReportedAsInvalidSelector(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				selectorListener("broken", bogusSelector()),
				selectorListener("good", teamSelector()),
			},
		},
	}

	route := &RouteInfo{Namespace: "apps", Kind: KindHTTPRoute}

	result, err := NewValidator(failingNamespaceClient(apierrors.NewTimeoutError("slow", 1))).
		ValidateBinding(context.Background(), gateway, route)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "BogusOperator")
	assert.Empty(t, result.Message)
}

// TestReportingValidator_WarnsOncePerState pins that the binding pass warns
// about an unevaluated listener once, drops the same warning in later passes
// to debug, and warns again after a pass in which the problem was gone.
func TestReportingValidator_WarnsOncePerState(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{selectorListener("broken", bogusSelector())},
		},
	}

	route := &RouteInfo{Name: "r", Namespace: "apps", Kind: KindHTTPRoute}
	healthy := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "apps"},
		Spec:       gatewayv1.GatewaySpec{Listeners: httpListeners()},
	}

	repeats := logging.NewRepeats()
	validator := NewReportingValidator(setupFakeClient(), repeats)

	logger, logs := logging.TestLogger(t)
	ctx := logging.WithLogger(context.Background(), logger)

	pass := func(gw *gatewayv1.Gateway) {
		repeats.NextPass()

		_, err := validator.ValidateBinding(ctx, gw, route)
		require.NoError(t, err)
	}

	pass(gateway)
	pass(gateway)
	assert.Equal(t, 1, strings.Count(logs.String(), `"level":"WARN"`), logs.String())

	pass(healthy)
	pass(gateway)
	assert.Equal(t, 2, strings.Count(logs.String(), `"level":"WARN"`), logs.String())
}

func httpListeners() []gatewayv1.Listener {
	return []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}}
}

// TestReportingValidator_WarnsOncePerParent pins that a route whose parents
// each have a listener that cannot be evaluated warns once per parent, not
// once per sync because the parents' warnings displace each other.
func TestReportingValidator_WarnsOncePerParent(t *testing.T) {
	t.Parallel()

	brokenGateway := func(name string, listener gatewayv1.SectionName) *gatewayv1.Gateway {
		return &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "infra"},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{selectorListener(listener, bogusSelector())},
			},
		}
	}

	first, second := brokenGateway("gw-a", "broken-a"), brokenGateway("gw-b", "broken-b")
	route := &RouteInfo{Name: "r", Namespace: "apps", Kind: KindHTTPRoute}

	repeats := logging.NewRepeats()
	validator := NewReportingValidator(setupFakeClient(), repeats)

	logger, logs := logging.TestLogger(t)
	ctx := logging.WithLogger(context.Background(), logger)

	for range 3 {
		repeats.NextPass()

		for _, gw := range []*gatewayv1.Gateway{first, second} {
			_, err := validator.ValidateBinding(ctx, gw, route)
			require.NoError(t, err)
		}
	}

	assert.Equal(t, 2, strings.Count(logs.String(), `"level":"WARN"`), logs.String())
}

// TestNamespaceReadError_SiblingEntryStillBinds pins that a namespace read
// failure stays with the selector entry that needed it: a sibling entry that
// admits the route still binds it, and the result is marked incomplete so the
// caller retries for the entry it could not evaluate.
func TestNamespaceReadError_SiblingEntryStillBinds(t *testing.T) {
	t.Parallel()

	route := &RouteInfo{Namespace: "apps", Kind: KindHTTPRoute}
	same := gatewayv1.Listener{Name: "same", Port: 80, Protocol: gatewayv1.HTTPProtocolType}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "apps"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{selectorListener("sel", teamSelector()), same},
		},
	}

	listenerSet := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "apps"},
		Spec: gatewayv1.ListenerSetSpec{
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: "sel", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: selectorListener("sel", teamSelector()).AllowedRoutes,
				},
				{Name: "same", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}

	validator := NewValidator(failingNamespaceClient(apierrors.NewTimeoutError("slow", 1)))

	for name, bind := range map[string]func() (BindingResult, error){
		"gateway listener": func() (BindingResult, error) { return validator.ValidateBinding(context.Background(), gateway, route) },
		"listenerset entry": func() (BindingResult, error) {
			return validator.ValidateBindingForListenerSet(context.Background(), listenerSet, route)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			result, err := bind()
			require.NoError(t, err)
			assert.True(t, result.Accepted)
			assert.Equal(t, []gatewayv1.SectionName{"same"}, result.MatchedListeners)
			assert.True(t, result.Incomplete)
		})
	}
}

// TestNamespaceReadError_StillLogsSiblingInvalidSelector pins that a binding
// left undecided by a failed namespace read still reports a sibling listener
// whose selector does not parse.
func TestNamespaceReadError_StillLogsSiblingInvalidSelector(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				selectorListener("broken", bogusSelector()),
				selectorListener("sel", teamSelector()),
			},
		},
	}

	validator := NewReportingValidator(failingNamespaceClient(apierrors.NewTimeoutError("slow", 1)), logging.NewRepeats())

	logger, logs := logging.TestLogger(t)
	ctx := logging.WithLogger(context.Background(), logger)

	_, err := validator.ValidateBinding(ctx, gateway, &RouteInfo{Name: "r", Namespace: "apps", Kind: KindHTTPRoute})
	require.Error(t, err)
	assert.Contains(t, logs.String(), `listener \"broken\"`)
}

// TestReportingValidator_WarnsOncePerSection pins that two parentRefs of one
// route pinned to different listeners of one Gateway warn once each, rather
// than displacing each other's warning on every pass.
func TestReportingValidator_WarnsOncePerSection(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				selectorListener("http", teamSelector()),
				selectorListener("https", teamSelector()),
			},
		},
	}

	repeats := logging.NewRepeats()
	validator := NewReportingValidator(failingNamespaceClient(apierrors.NewTimeoutError("slow", 1)), repeats)

	logger, logs := logging.TestLogger(t)
	ctx := logging.WithLogger(context.Background(), logger)

	for range 3 {
		repeats.NextPass()

		for _, section := range []gatewayv1.SectionName{"http", "https"} {
			_, err := validator.ValidateBinding(ctx, gateway,
				&RouteInfo{Name: "r", Namespace: "apps", Kind: KindHTTPRoute, SectionName: &section})
			require.Error(t, err)
		}
	}

	assert.Equal(t, 2, strings.Count(logs.String(), `"level":"WARN"`), logs.String())
}
