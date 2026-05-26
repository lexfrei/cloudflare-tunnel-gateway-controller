package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestNewProxySecretReconciler_ParseShapes pins the input-validation
// contract: empty, malformed, partial, and valid cluster-DNS shapes
// all classify deterministically. The sentinel errors are matchable
// with errors.Is so the manager wiring can decide whether to refuse
// startup or skip-and-log.
func TestNewProxySecretReconciler_ParseShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		tokenSecret     string
		deploymentLabel string
		wantSentinel    error
		wantNamespace   string
		wantName        string
		wantLabelKey    string
		wantLabelValue  string
	}{
		{
			name:         "empty token secret fails fast",
			tokenSecret:  "",
			wantSentinel: errEmptyProxyTokenSecret,
		},
		{
			name:         "whitespace-only token secret fails fast",
			tokenSecret:  "   ",
			wantSentinel: errEmptyProxyTokenSecret,
		},
		{
			name:         "missing namespace fails",
			tokenSecret:  "/cloudflare-tunnel-token",
			wantSentinel: errInvalidProxyTokenSecret,
		},
		{
			name:         "missing name fails",
			tokenSecret:  "cloudflare-tunnel-system/",
			wantSentinel: errInvalidProxyTokenSecret,
		},
		{
			name:         "no slash fails",
			tokenSecret:  "cloudflare-tunnel-token",
			wantSentinel: errInvalidProxyTokenSecret,
		},
		{
			name:           "valid shape with default deployment label",
			tokenSecret:    "cloudflare-tunnel-system/cloudflare-tunnel-token",
			wantNamespace:  "cloudflare-tunnel-system",
			wantName:       "cloudflare-tunnel-token",
			wantLabelKey:   "app.kubernetes.io/component",
			wantLabelValue: "proxy",
		},
		{
			name:            "valid shape with custom deployment label",
			tokenSecret:     "cf-tunnel/token-secret",
			deploymentLabel: "role=proxy-data-plane",
			wantNamespace:   "cf-tunnel",
			wantName:        "token-secret",
			wantLabelKey:    "role",
			wantLabelValue:  "proxy-data-plane",
		},
		{
			name:            "malformed deployment label fails",
			tokenSecret:     "ns/name",
			deploymentLabel: "no-equals-sign",
			wantSentinel:    errInvalidProxyDeploymentLabel,
		},
		{
			name:            "deployment label missing key fails",
			tokenSecret:     "ns/name",
			deploymentLabel: "=value",
			wantSentinel:    errInvalidProxyDeploymentLabel,
		},
		{
			name:            "deployment label missing value fails",
			tokenSecret:     "ns/name",
			deploymentLabel: "key=",
			wantSentinel:    errInvalidProxyDeploymentLabel,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := NewProxySecretReconciler(nil, tc.tokenSecret, tc.deploymentLabel)
			if tc.wantSentinel != nil {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantSentinel),
					"want errors.Is(err, %v); got %v", tc.wantSentinel, err)

				return
			}

			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.wantNamespace, got.TokenSecretNamespace)
			assert.Equal(t, tc.wantName, got.TokenSecretName)
			assert.Equal(t, tc.wantLabelKey, got.DeploymentLabelKey)
			assert.Equal(t, tc.wantLabelValue, got.DeploymentLabelValue)
		})
	}
}

// TestHashSecretData_Deterministic pins that the same data produces the
// same revision regardless of map insertion order. The reconciler relies
// on this to skip no-op patches; a non-deterministic hash would cause
// spurious rollouts on benign Secret resourceVersion bumps.
func TestHashSecretData_Deterministic(t *testing.T) {
	t.Parallel()

	a := map[string][]byte{"k1": []byte("v1"), "k2": []byte("v2"), "k3": []byte("v3")}
	b := map[string][]byte{"k3": []byte("v3"), "k1": []byte("v1"), "k2": []byte("v2")}

	assert.Equal(t, hashSecretData(a), hashSecretData(b),
		"key-order independence: same data must hash to the same revision")
}

// TestHashSecretData_DistinguishesContent pins that any change to the
// data map produces a different hash. Single-key change, key rename,
// and value rotation must all collide-free.
func TestHashSecretData_DistinguishesContent(t *testing.T) {
	t.Parallel()

	base := map[string][]byte{"tunnel-token": []byte("eyJhIjoiYiJ9")}

	rotated := map[string][]byte{"tunnel-token": []byte("eyJhIjoiYyJ9")}
	renamed := map[string][]byte{"token": []byte("eyJhIjoiYiJ9")}
	extended := map[string][]byte{"tunnel-token": []byte("eyJhIjoiYiJ9"), "extra": []byte("noise")}

	for name, other := range map[string]map[string][]byte{
		"value rotation":  rotated,
		"key rename":      renamed,
		"extra key added": extended,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.NotEqual(t, hashSecretData(base), hashSecretData(other),
				"%s must produce a distinct revision", name)
		})
	}
}

// TestProxySecretReconciler_PatchesDeploymentOnSecretChange pins the
// end-to-end behaviour: a Secret event reaches Reconcile, the matching
// proxy Deployment's pod template annotation is set to the Secret's
// data hash, and Kubernetes' native rolling-restart kicks in.
//
// The fake client doesn't run the Deployment controller, so we
// observe the annotation directly. Issue #114.
func TestProxySecretReconciler_PatchesDeploymentOnSecretChange(t *testing.T) {
	t.Parallel()

	const (
		ns      = "cloudflare-tunnel-system"
		secret  = "cloudflare-tunnel-token"
		deployN = "cf-tunnel-proxy"
	)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))

	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secret, Namespace: ns},
		Data:       map[string][]byte{"tunnel-token": []byte("freshly-rotated-jwt")},
	}

	proxyDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployN,
			Namespace: ns,
			Labels:    map[string]string{"app.kubernetes.io/component": "proxy"},
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app.kubernetes.io/component": "proxy"},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(tokenSecret, proxyDeploy).
		Build()

	r := &ProxySecretReconciler{
		Client:               fakeClient,
		TokenSecretNamespace: ns,
		TokenSecretName:      secret,
		DeploymentLabelKey:   "app.kubernetes.io/component",
		DeploymentLabelValue: "proxy",
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: secret},
	})
	require.NoError(t, err)

	var got appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: deployN}, &got))

	annotations := got.Spec.Template.Annotations
	require.NotNil(t, annotations, "pod template must have annotations after reconcile")

	wantRevision := hashSecretData(tokenSecret.Data)
	assert.Equal(t, wantRevision, annotations[tokenRevisionAnnotation],
		"pod template revision annotation must equal hash of Secret data")
}

// TestProxySecretReconciler_IdempotentOnUnchangedData pins the no-op
// guard: re-reconciling the same Secret value MUST NOT bump the
// Deployment's generation, otherwise every benign metadata-controller
// event on the Secret would silently roll the proxy pods.
func TestProxySecretReconciler_IdempotentOnUnchangedData(t *testing.T) {
	t.Parallel()

	const (
		ns      = "cloudflare-tunnel-system"
		secretN = "cloudflare-tunnel-token"
		deployN = "cf-tunnel-proxy"
	)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))

	data := map[string][]byte{"tunnel-token": []byte("stable-value")}

	preStamped := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       deployN,
			Namespace:  ns,
			Labels:     map[string]string{"app.kubernetes.io/component": "proxy"},
			Generation: 7,
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app.kubernetes.io/component": "proxy"},
					Annotations: map[string]string{
						tokenRevisionAnnotation: hashSecretData(data),
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretN, Namespace: ns},
				Data:       data,
			},
			preStamped,
		).
		Build()

	r := &ProxySecretReconciler{
		Client:               fakeClient,
		TokenSecretNamespace: ns,
		TokenSecretName:      secretN,
		DeploymentLabelKey:   "app.kubernetes.io/component",
		DeploymentLabelValue: "proxy",
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: secretN},
	})
	require.NoError(t, err)

	var got appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: deployN}, &got))

	assert.Equal(t, hashSecretData(data), got.Spec.Template.Annotations[tokenRevisionAnnotation],
		"annotation must remain at the same revision")
	assert.Equal(t, int64(7), got.Generation,
		"Generation must NOT bump when the Secret hash is unchanged "+
			"-- otherwise Kubernetes rolls the pods on every benign Secret resourceVersion event")
}

// TestProxySecretReconciler_SecretDeletedIsNoop pins that a Secret
// deletion does NOT scramble the Deployment annotation. The proxy will
// eventually fail on its own when the env-from-secretKeyRef resolves
// to a missing value; preemptively bumping the annotation only adds
// noise and would mask the real source-of-truth Secret event.
func TestProxySecretReconciler_SecretDeletedIsNoop(t *testing.T) {
	t.Parallel()

	const (
		ns      = "cloudflare-tunnel-system"
		secretN = "cloudflare-tunnel-token"
	)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := &ProxySecretReconciler{
		Client:               fakeClient,
		TokenSecretNamespace: ns,
		TokenSecretName:      secretN,
		DeploymentLabelKey:   "app.kubernetes.io/component",
		DeploymentLabelValue: "proxy",
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: secretN},
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res, "Secret-not-found path must be a clean no-op")
}
