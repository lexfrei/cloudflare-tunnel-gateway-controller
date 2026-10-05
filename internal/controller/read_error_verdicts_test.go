package controller

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// failLists fails every List into a list of type T while fail reports true.
func failLists[T client.ObjectList](fail *atomic.Bool) interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, cli client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(T); ok && fail.Load() {
				return errSimulatedCacheMiss
			}

			return cli.List(ctx, list, opts...)
		},
	}
}

// gatewayStatusWriters are the two Gateway status writers: the resolved one
// and the one for a configuration that did not resolve.
func gatewayStatusWriters() map[string]func(*GatewayReconciler, *gatewayv1.Gateway) error {
	return map[string]func(*GatewayReconciler, *gatewayv1.Gateway) error{
		"resolved": func(r *GatewayReconciler, gw *gatewayv1.Gateway) error {
			return r.updateStatus(context.Background(), gw, &config.ResolvedConfig{TunnelID: "tunnel"}, false)
		},
		"config error": func(r *GatewayReconciler, gw *gatewayv1.Gateway) error {
			return r.setConfigErrorStatus(context.Background(), gw, config.MarkInvalidParameters(errSimulatedCacheMiss))
		},
	}
}

// assertStatusWriteSkippedOnReadError writes the Gateway's status once with
// every read working, then again with fail set, and pins that the second
// write returns an error and leaves the first status in place.
func assertStatusWriteSkippedOnReadError(
	t *testing.T,
	gateway *gatewayv1.Gateway,
	funcs func(*atomic.Bool) interceptor.Funcs,
	objects ...client.Object,
) {
	t.Helper()

	for name, write := range gatewayStatusWriters() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gw := gateway.DeepCopy()
			objs := make([]client.Object, 0, 1+len(objects))
			objs = append(objs, gw)

			for _, obj := range objects {
				objs = append(objs, obj.DeepCopyObject().(client.Object))
			}

			base := setupGatewayFakeClientWithBeta1(objs...)

			var fail atomic.Bool

			cli := interceptor.NewClient(base, funcs(&fail))
			reconciler := &GatewayReconciler{Client: cli, Scheme: base.Scheme(), ControllerName: "test-controller"}

			_ = write(reconciler, gw)

			var before gatewayv1.Gateway
			require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(gw), &before))
			require.NotEmpty(t, before.Status.Listeners)

			fail.Store(true)

			require.Error(t, write(reconciler, gw), "a read error must be returned so the reconcile retries")

			var after gatewayv1.Gateway
			require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(gw), &after))
			assert.Equal(t, before.Status, after.Status, "a read error must not rewrite the status")
		})
	}
}

// TestGatewayStatus_UnbuildableListenerViewSkipsWrite pins that a Gateway
// whose merged listener view cannot be built writes no status: written
// without the view, its own conflicted listeners would lose Conflicted=True
// and the Gateway its ListenersNotValid reason.
func TestGatewayStatus_UnbuildableListenerViewSkipsWrite(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel", Listeners: ownConflictListeners()},
	}

	assertStatusWriteSkippedOnReadError(t, gateway, failLists[*gatewayv1.ListenerSetList])
}

func crossNamespaceCertGateway() *gatewayv1.Gateway {
	certNS := gatewayv1.Namespace("certs")

	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{{
				Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
				TLS: &gatewayv1.ListenerTLSConfig{
					CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "tls", Namespace: &certNS}},
				},
			}},
		},
	}
}

func listenerTLSSecret(namespace, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte(testTLSCertPEM),
			corev1.TLSPrivateKeyKey: []byte(testTLSKeyPEM),
		},
	}
}

func gatewaySecretGrant(namespace, fromNamespace string) *gatewayv1beta1.ReferenceGrant {
	return &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "grant", Namespace: namespace},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{Group: gatewayv1.GroupName, Kind: kindGateway, Namespace: gatewayv1.Namespace(fromNamespace)},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{{Group: "", Kind: kindSecret}},
		},
	}
}

// TestGatewayStatus_UnreadableCertRefSkipsWrite pins that a listener
// certificate reference whose ReferenceGrants or Secret cannot be read is not
// reported RefNotPermitted or InvalidCertificateRef: the error is returned
// and the listener keeps the verdict it had.
func TestGatewayStatus_UnreadableCertRefSkipsWrite(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*atomic.Bool) interceptor.Funcs{
		"ReferenceGrant List": failLists[*gatewayv1beta1.ReferenceGrantList],
		"Secret Get": func(fail *atomic.Bool) interceptor.Funcs {
			return failReads[*corev1.Secret]("tls", fail)
		},
	}

	for name, funcs := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assertStatusWriteSkippedOnReadError(t, crossNamespaceCertGateway(), funcs,
				listenerTLSSecret("certs", "tls"), gatewaySecretGrant("certs", "infra"))
		})
	}
}

// TestGatewayStatus_UnreadableClientCertKeepsResolvedRefs pins that a backend
// client certificate whose ReferenceGrants or Secret cannot be read is not
// reported InvalidClientCertificateRef: the Gateway keeps its ResolvedRefs
// verdict, and the reconcile is retried.
func TestGatewayStatus_UnreadableClientCertKeepsResolvedRefs(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*atomic.Bool) interceptor.Funcs{
		"ReferenceGrant List": failLists[*gatewayv1beta1.ReferenceGrantList],
		"Secret Get": func(fail *atomic.Bool) interceptor.Funcs {
			return failReads[*corev1.Secret]("client-cert", fail)
		},
	}

	for name, funcs := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			certPEM, keyPEM := generateClientKeypair(t)
			certNS := gatewayv1.Namespace("certs")
			gateway := gatewayWithClientCertRef("infra", "gw", "client-cert", &certNS)
			gateway.Spec.GatewayClassName = "cloudflare-tunnel"
			gateway.Spec.Listeners = httpListener()

			base := setupGatewayFakeClientWithBeta1(gateway,
				clientCertSecret("certs", "client-cert", certPEM, keyPEM), gatewaySecretGrant("certs", "infra"))

			var fail atomic.Bool

			cli := interceptor.NewClient(base, funcs(&fail))
			reconciler := &GatewayReconciler{Client: cli, Scheme: base.Scheme(), ControllerName: "test-controller"}
			resolved := &config.ResolvedConfig{TunnelID: "tunnel"}

			require.NoError(t, reconciler.updateStatus(context.Background(), gateway, resolved, false))

			fail.Store(true)

			require.Error(t, reconciler.updateStatus(context.Background(), gateway, resolved, false),
				"a read error must be returned so the reconcile retries")

			var updated gatewayv1.Gateway
			require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(gateway), &updated))

			cond := meta.FindStatusCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionResolvedRefs))
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionTrue, cond.Status)
		})
	}
}

// TestResolveListenerEntryRefs_UnlistableGrantIsReturned pins the ListenerSet
// twin of the listener certificate check: a ReferenceGrant List error is
// returned for retry instead of being reported RefNotPermitted.
func TestResolveListenerEntryRefs_UnlistableGrantIsReturned(t *testing.T) {
	t.Parallel()

	certNS := gatewayv1.Namespace("certs")
	ls := &gatewayv1.ListenerSet{ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "ns"}}
	entry := &gatewayv1.ListenerEntry{
		Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
		TLS: &gatewayv1.ListenerTLSConfig{
			CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "tls", Namespace: &certNS}},
		},
	}

	cli := interceptor.NewClient(setupGatewayFakeClientWithBeta1(listenerTLSSecret("certs", "tls")),
		failLists[*gatewayv1beta1.ReferenceGrantList](failing()))

	_, err := resolveListenerEntryRefs(context.Background(), cli, ls, entry)
	require.ErrorIs(t, err, errSimulatedCacheMiss)
}

// TestFindRoutesForReferenceGrant_MirrorBackend pins that a grant event
// re-evaluates a route whose only cross-namespace reference is a
// RequestMirror target, on a rule or on a backendRef, so revoking the grant
// stops the mirror. A change to the mirror Service re-evaluates it as well.
func TestFindRoutesForReferenceGrant_MirrorBackend(t *testing.T) {
	t.Parallel()

	mirrorNS := gatewayv1.Namespace("mirror")
	mirror := gatewayv1.HTTPRouteFilter{
		Type:          gatewayv1.HTTPRouteFilterRequestMirror,
		RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{BackendRef: gatewayv1.BackendObjectReference{Name: "shadow", Namespace: &mirrorNS}},
	}
	backend := gatewayv1.HTTPBackendRef{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "app"}}}
	backendWithMirror := backend
	backendWithMirror.Filters = []gatewayv1.HTTPRouteFilter{mirror}

	tests := map[string]gatewayv1.HTTPRouteRule{
		"rule filter":       {Filters: []gatewayv1.HTTPRouteFilter{mirror}, BackendRefs: []gatewayv1.HTTPBackendRef{backend}},
		"backendRef filter": {BackendRefs: []gatewayv1.HTTPBackendRef{backendWithMirror}},
	}

	for name, rule := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			route := &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "app"},
				Spec:       gatewayv1.HTTPRouteSpec{Rules: []gatewayv1.HTTPRouteRule{rule}},
			}
			grant := &gatewayv1beta1.ReferenceGrant{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "mirror"}}

			routes := []Route{HTTPRouteWrapper{route}}
			require.Len(t, FindRoutesForReferenceGrant(grant, routes), 1)

			shadow := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "shadow", Namespace: "mirror"}}
			assert.Len(t, FindRoutesForService(shadow, routes), 1, "a mirror Service change re-evaluates the route too")
		})
	}
}
