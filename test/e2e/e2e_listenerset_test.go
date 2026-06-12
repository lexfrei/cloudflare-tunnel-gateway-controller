//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestListenerSetEndToEnd attaches a ListenerSet to the e2e Gateway, binds an
// HTTPRoute to the ListenerSet via parentRef Kind=ListenerSet, and asserts:
//
//   - Gateway.status.attachedListenerSets reflects the new attachment,
//   - ListenerSet aggregate Accepted/Programmed conditions are True,
//   - the route is reachable through the real Cloudflare tunnel and lands on
//     the expected backend (echo-v1).
//
// The test temporarily patches the e2e Gateway to opt into allowedListeners
// (which the shared setupGateway intentionally does not set so other tests
// aren't affected by the multi-listener attach semantic) and restores it on
// cleanup. The selector-delegation test below shares this patch helper, so
// restoration must be conflict-safe — both tests mutate the shared Gateway.
func TestListenerSetEndToEnd(t *testing.T) {
	cfg := loadTestConfig(t)
	httpClient := tunnelClient()
	k8sClient := newK8sClient(t, cfg.KubeContext)

	setupTestNamespace(t, k8sClient, cfg)
	setupBackendsForListenerSet(t, k8sClient, cfg)
	setupGateway(t, k8sClient, cfg)

	restoreGateway := allowListenerSetAttachment(t, k8sClient, cfg)
	t.Cleanup(restoreGateway)

	ls := buildListenerSet("ls-e2e", cfg)
	createListenerSet(t, k8sClient, ls)

	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ls)
	})

	waitForListenerSetAccepted(t, k8sClient, ls)
	waitForGatewayAttachedListenerSets(t, k8sClient, cfg, 1)

	route := buildHTTPRouteForListenerSet("ls-e2e-route", cfg, ls.Name)
	createHTTPRoute(t, k8sClient, route)

	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), route)
	})

	waitForBackend(t, httpClient, cfg.TunnelHostname, "/ls-e2e", "echo-v1-", 90*time.Second)

	echo, resp, err := makeRequest(context.Background(), t, httpClient, cfg.TunnelHostname, http.MethodGet, "/ls-e2e", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "/ls-e2e", echo.Path)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "pod should be echo-v1, got: %s", echo.Pod)
}

func setupBackendsForListenerSet(t *testing.T, k8sClient client.Client, cfg testConfig) {
	t.Helper()
	// Existing setup_test.go already provisions echo-v1/v2/v3 backends; nothing
	// to do here. Helper exists to make the test body read top-to-bottom.
	_ = k8sClient
	_ = cfg
}

// allowListenerSetAttachment patches the e2e Gateway's spec.allowedListeners
// to permit same-namespace ListenerSet attachments. Returns a cleanup func
// that restores the original (unset) state so other e2e suites remain
// unaffected.
func allowListenerSetAttachment(t *testing.T, k8sClient client.Client, cfg testConfig) func() {
	t.Helper()

	ctx := context.Background()
	key := types.NamespacedName{Name: cfg.GatewayName, Namespace: cfg.Namespace}

	gw := &gatewayv1.Gateway{}
	require.NoError(t, k8sClient.Get(ctx, key, gw))

	originalAllowed := gw.Spec.AllowedListeners

	fromSame := gatewayv1.NamespacesFromSame
	patched := gw.DeepCopy()
	patched.Spec.AllowedListeners = &gatewayv1.AllowedListeners{
		Namespaces: &gatewayv1.ListenerNamespaces{From: &fromSame},
	}
	require.NoError(t, k8sClient.Update(ctx, patched))

	return func() {
		fresh := &gatewayv1.Gateway{}
		err := k8sClient.Get(context.Background(), key, fresh)
		if err != nil {
			return
		}

		fresh.Spec.AllowedListeners = originalAllowed
		_ = k8sClient.Update(context.Background(), fresh)
	}
}

func buildListenerSet(name string, cfg testConfig) *gatewayv1.ListenerSet {
	hostname := gatewayv1.Hostname(cfg.TunnelHostname)
	gatewayNS := gatewayv1.Namespace(cfg.Namespace)

	return &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.Namespace,
		},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{
				Name:      gatewayv1.ObjectName(cfg.GatewayName),
				Namespace: &gatewayNS,
			},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name:     "ls-http",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
					Hostname: &hostname,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{
							From: new(gatewayv1.NamespacesFromAll),
						},
					},
				},
			},
		},
	}
}

func createListenerSet(t *testing.T, k8sClient client.Client, ls *gatewayv1.ListenerSet) {
	t.Helper()
	ctx := context.Background()

	existing := &gatewayv1.ListenerSet{}

	err := k8sClient.Get(ctx, types.NamespacedName{Name: ls.Name, Namespace: ls.Namespace}, existing)
	if err == nil {
		require.NoError(t, k8sClient.Delete(ctx, existing))
		time.Sleep(time.Second)
	}

	require.NoError(t, k8sClient.Create(ctx, ls))
	t.Logf("created ListenerSet %s/%s", ls.Namespace, ls.Name)
}

func waitForListenerSetAccepted(t *testing.T, k8sClient client.Client, ls *gatewayv1.ListenerSet) {
	t.Helper()

	err := wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 90*time.Second, true,
		func(pollCtx context.Context) (bool, error) {
			current := &gatewayv1.ListenerSet{}
			getErr := k8sClient.Get(pollCtx, types.NamespacedName{Name: ls.Name, Namespace: ls.Namespace}, current)
			if getErr != nil {
				return false, nil //nolint:nilerr // transient API errors are expected while polling; retry until timeout
			}

			accepted := false
			programmed := false

			for _, cond := range current.Status.Conditions {
				if cond.Type == string(gatewayv1.ListenerSetConditionAccepted) && cond.Status == metav1.ConditionTrue {
					accepted = true
				}

				if cond.Type == string(gatewayv1.ListenerSetConditionProgrammed) && cond.Status == metav1.ConditionTrue {
					programmed = true
				}
			}

			return accepted && programmed, nil
		},
	)
	require.NoError(t, err, "ListenerSet did not become Accepted+Programmed in time")
}

func waitForGatewayAttachedListenerSets(t *testing.T, k8sClient client.Client, cfg testConfig, want int32) {
	t.Helper()

	err := wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 60*time.Second, true,
		func(pollCtx context.Context) (bool, error) {
			gw := &gatewayv1.Gateway{}
			getErr := k8sClient.Get(pollCtx, types.NamespacedName{Name: cfg.GatewayName, Namespace: cfg.Namespace}, gw)
			if getErr != nil {
				return false, nil //nolint:nilerr // transient API errors are expected while polling; retry until timeout
			}

			if gw.Status.AttachedListenerSets == nil {
				return false, nil
			}

			return *gw.Status.AttachedListenerSets == want, nil
		},
	)
	require.NoError(t, err, "Gateway.status.attachedListenerSets did not reach %d", want)
}

func buildHTTPRouteForListenerSet(name string, cfg testConfig, listenerSetName string) *gatewayv1.HTTPRoute {
	gatewayNS := gatewayv1.Namespace(cfg.Namespace)
	listenerSetKind := gatewayv1.Kind("ListenerSet")
	listenerSetGroup := gatewayv1.Group("gateway.networking.k8s.io")
	hostname := gatewayv1.Hostname(cfg.TunnelHostname)

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.TestNamespace,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Group:     &listenerSetGroup,
						Kind:      &listenerSetKind,
						Name:      gatewayv1.ObjectName(listenerSetName),
						Namespace: &gatewayNS,
					},
				},
			},
			Hostnames: []gatewayv1.Hostname{hostname},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: pathPrefix("/ls-e2e")},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						backendRef("echo-v1", 80, nil),
					},
				},
			},
		},
	}
}

// TestListenerSetSelectorDelegation pins the GEP-1713 tenant self-service
// delegation model end to end (#477): a Gateway with
// allowedListeners.namespaces.from=Selector admits ListenerSets only from
// namespaces matching the selector. A ListenerSet from a matching namespace
// reaches Accepted=True; one from a non-matching namespace is rejected with
// reason NotAllowed and never counts toward attachedListenerSets.
func TestListenerSetSelectorDelegation(t *testing.T) {
	cfg := loadTestConfig(t)
	k8sClient := newK8sClient(t, cfg.KubeContext)
	ctx := context.Background()

	setupTestNamespace(t, k8sClient, cfg)
	setupGateway(t, k8sClient, cfg)

	restoreGateway := allowListenerSetSelector(t, k8sClient, cfg, "ls-delegation", "allowed")
	t.Cleanup(restoreGateway)

	allowedNS := createLabelledNamespace(ctx, t, k8sClient, "ls-tenant-allowed-", map[string]string{
		"ls-delegation": "allowed",
	})
	deniedNS := createLabelledNamespace(ctx, t, k8sClient, "ls-tenant-denied-", nil)

	allowedLS := buildListenerSet("ls-selector-allowed", cfg)
	allowedLS.Namespace = allowedNS
	createListenerSet(t, k8sClient, allowedLS)

	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), allowedLS) })

	deniedLS := buildListenerSet("ls-selector-denied", cfg)
	deniedLS.Namespace = deniedNS
	createListenerSet(t, k8sClient, deniedLS)

	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), deniedLS) })

	waitForListenerSetAccepted(t, k8sClient, allowedLS)
	waitForListenerSetRejected(t, k8sClient, deniedLS)
	waitForGatewayAttachedListenerSets(t, k8sClient, cfg, 1)
}

// allowListenerSetSelector patches the e2e Gateway's allowedListeners to a
// namespace label Selector and returns a restore func.
func allowListenerSetSelector(
	t *testing.T,
	k8sClient client.Client,
	cfg testConfig,
	labelKey, labelValue string,
) func() {
	t.Helper()

	ctx := context.Background()
	key := types.NamespacedName{Name: cfg.GatewayName, Namespace: cfg.Namespace}

	gw := &gatewayv1.Gateway{}
	require.NoError(t, k8sClient.Get(ctx, key, gw))

	originalAllowed := gw.Spec.AllowedListeners

	fromSelector := gatewayv1.NamespacesFromSelector
	patched := gw.DeepCopy()
	patched.Spec.AllowedListeners = &gatewayv1.AllowedListeners{
		Namespaces: &gatewayv1.ListenerNamespaces{
			From: &fromSelector,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{labelKey: labelValue},
			},
		},
	}
	require.NoError(t, k8sClient.Update(ctx, patched))

	return func() {
		// Restore with conflict retry: a swallowed conflict here leaves the
		// shared Gateway opted into allowedListeners and poisons every later
		// test run on a reused cluster.
		restoreErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			fresh := &gatewayv1.Gateway{}

			getErr := k8sClient.Get(context.Background(), key, fresh)
			if getErr != nil {
				return fmt.Errorf("get gateway for restore: %w", getErr)
			}

			fresh.Spec.AllowedListeners = originalAllowed

			return k8sClient.Update(context.Background(), fresh)
		})
		if restoreErr != nil {
			t.Errorf("failed to restore the shared Gateway's allowedListeners: %v", restoreErr)
		}
	}
}

// createLabelledNamespace creates a generated-name namespace with the given
// labels and schedules its deletion.
func createLabelledNamespace(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	generateName string,
	labels map[string]string,
) string {
	t.Helper()

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: generateName,
		Labels:       labels,
	}}
	require.NoError(t, k8sClient.Create(ctx, namespace))

	//nolint:contextcheck // cleanup runs after the test context may be done
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), namespace) })

	return namespace.Name
}

// waitForListenerSetRejected polls until the ListenerSet carries
// Accepted=False with reason NotAllowed.
func waitForListenerSetRejected(t *testing.T, k8sClient client.Client, ls *gatewayv1.ListenerSet) {
	t.Helper()

	err := wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 90*time.Second, true,
		func(pollCtx context.Context) (bool, error) {
			current := &gatewayv1.ListenerSet{}
			getErr := k8sClient.Get(pollCtx, types.NamespacedName{Name: ls.Name, Namespace: ls.Namespace}, current)
			if getErr != nil {
				return false, nil //nolint:nilerr // transient API errors are expected while polling; retry until timeout
			}

			for _, cond := range current.Status.Conditions {
				if cond.Type == string(gatewayv1.ListenerSetConditionAccepted) &&
					cond.Status == metav1.ConditionFalse &&
					cond.Reason == string(gatewayv1.ListenerSetReasonNotAllowed) {
					return true, nil
				}
			}

			return false, nil
		},
	)
	require.NoError(t, err, "ListenerSet was not rejected with NotAllowed in time")
}
