//go:build envtest

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// proxyDeploymentFixture returns a minimal valid proxy Deployment carrying the
// label the reconciler selects on.
func proxyDeploymentFixture(namespace, name string) *appsv1.Deployment {
	labels := map[string]string{"app.kubernetes.io/component": "proxy", "app.kubernetes.io/instance": name}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "proxy", Image: "example.com/proxy:v1"}},
				},
			},
		},
	}
}

// waitForDeployment polls the apiserver until cond holds for the named
// Deployment, failing the test after a bounded wait.
func waitForDeployment(ctx context.Context, t *testing.T, key types.NamespacedName, cond func(*appsv1.Deployment) bool, msg string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	for {
		var dep appsv1.Deployment
		if err := envK8sClient.Get(ctx, key, &dep); err == nil && cond(&dep) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal(msg)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// TestProxySecretReconciler_DeploymentCreatedLaterStillRolls pins the
// Deployment watch: a proxy Deployment created while the controller is
// already running, or one that loses its annotations, gets its revision
// recorded with the token Secret unchanged, so a later rotation rolls it.
// Watching only the Secret leaves such a Deployment unrecorded and the
// rotation then reads as a first sighting.
func TestProxySecretReconciler_DeploymentCreatedLaterStillRolls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	namespace := driftNamespace(ctx, t)
	secretKey := types.NamespacedName{Namespace: namespace, Name: "tunnel-token"}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: namespace},
		Data:       map[string][]byte{"tunnel-token": []byte("install-time-jwt")},
	}
	require.NoError(t, envK8sClient.Create(ctx, secret))
	require.NoError(t, envK8sClient.Create(ctx, proxyDeploymentFixture(namespace, "early")))

	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Scheme:     envScheme,
		Metrics:    server.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	require.NoError(t, err)

	reconciler, err := NewProxySecretReconciler(mgr.GetClient(), namespace+"/"+secretKey.Name, "")
	require.NoError(t, err)
	require.NoError(t, reconciler.SetupWithManager(mgr))

	go func() { _ = mgr.Start(ctx) }()

	installRevision := hashSecretData(secret.Data)

	// The Secret's initial event has been handled once the pre-existing
	// Deployment carries the recorded revision.
	waitForDeployment(ctx, t, types.NamespacedName{Namespace: namespace, Name: "early"},
		func(dep *appsv1.Deployment) bool { return dep.Annotations[tokenRevisionAnnotation] == installRevision },
		"the Deployment present at startup never had its revision recorded")

	require.NoError(t, envK8sClient.Create(ctx, proxyDeploymentFixture(namespace, "late")))

	lateKey := types.NamespacedName{Namespace: namespace, Name: "late"}
	waitForDeployment(ctx, t, lateKey,
		func(dep *appsv1.Deployment) bool { return dep.Annotations[tokenRevisionAnnotation] == installRevision },
		"a Deployment created after startup must have its revision recorded without a Secret change")

	secret.Data = map[string][]byte{"tunnel-token": []byte("rotated-jwt")}
	require.NoError(t, envK8sClient.Update(ctx, secret))

	rotatedRevision := hashSecretData(secret.Data)
	waitForDeployment(ctx, t, lateKey,
		func(dep *appsv1.Deployment) bool {
			return dep.Spec.Template.Annotations[tokenRevisionAnnotation] == rotatedRevision
		},
		"a rotation after the late Deployment appeared must roll it")

	// A replace that drops both annotations (helm upgrade --force, kubectl
	// replace) must be recorded again, or the next rotation reads as a first
	// sighting and never rolls.
	var replaced appsv1.Deployment
	require.NoError(t, envK8sClient.Get(ctx, lateKey, &replaced))
	delete(replaced.Annotations, tokenRevisionAnnotation)
	delete(replaced.Spec.Template.Annotations, tokenRevisionAnnotation)
	require.NoError(t, envK8sClient.Update(ctx, &replaced))

	waitForDeployment(ctx, t, lateKey,
		func(dep *appsv1.Deployment) bool { return dep.Annotations[tokenRevisionAnnotation] == rotatedRevision },
		"a Deployment that lost its annotations must have its revision recorded again")

	secret.Data = map[string][]byte{"tunnel-token": []byte("rotated-again-jwt")}
	require.NoError(t, envK8sClient.Update(ctx, secret))

	secondRotation := hashSecretData(secret.Data)
	waitForDeployment(ctx, t, lateKey,
		func(dep *appsv1.Deployment) bool {
			return dep.Spec.Template.Annotations[tokenRevisionAnnotation] == secondRotation
		},
		"a rotation after the annotations were wiped must roll the Deployment")
}
