//go:build envtest

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// newUnstartedTestManager builds a real, un-started manager against the
// envtest API server -- mgr.Start() is never called, matching how Run calls
// resolveProxyAuthToken before wiring any reconciler. Exercising the real
// mgr.GetConfig()/mgr.GetScheme() plumbing (rather than the fake client the
// rest of this file's unit tests use for ensureProxyAuthSecret directly) is
// the point: it is the one path production Run actually takes that no other
// test in this package invokes.
func newUnstartedTestManager(t *testing.T) ctrl.Manager {
	t.Helper()

	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Scheme:  envScheme,
		Metrics: server.Options{BindAddress: "0"},
	})
	require.NoError(t, err)

	return mgr
}

// TestResolveProxyAuthToken_GeneratesAgainstRealManager pins the production
// path end to end: a real (never-started) manager, ProxyAuthSecretGenerate
// true, and no ProxyAuthSecretKey override (must default to "auth-token").
// Asserts both the returned token and the Secret actually landing in the
// API server -- the two things a fake-client unit test cannot prove about
// mgr.GetConfig()/mgr.GetScheme() wiring.
func TestResolveProxyAuthToken_GeneratesAgainstRealManager(t *testing.T) {
	mgr := newUnstartedTestManager(t)

	key := types.NamespacedName{Namespace: "default", Name: "envtest-generated-auth-token"}
	cfg := &Config{
		ProxyAuthSecretRef:      key.Namespace + "/" + key.Name,
		ProxyAuthSecretGenerate: true,
	}

	resolved, err := resolveProxyAuthToken(context.Background(), mgr, cfg)
	require.NoError(t, err)
	assert.NotEmpty(t, resolved.ProxyAuthToken)

	var secret corev1.Secret
	require.NoError(t, envK8sClient.Get(context.Background(), key, &secret))
	assert.Equal(t, []byte(resolved.ProxyAuthToken), secret.Data["auth-token"],
		"defaulted dataKey must be \"auth-token\" when ProxyAuthSecretKey is empty")

	t.Cleanup(func() {
		_ = envK8sClient.Delete(context.Background(), &secret)
	})
}

// TestResolveProxyAuthToken_ReadsBringYourOwnAgainstRealManager pins the
// other production branch: an operator-named Secret with a custom key,
// generate=false, resolved through the same real-manager path. Also proves
// resolveProxyAuthToken forwards cfg.ProxyAuthSecretKey rather than always
// falling back to the default.
func TestResolveProxyAuthToken_ReadsBringYourOwnAgainstRealManager(t *testing.T) {
	mgr := newUnstartedTestManager(t)

	key := types.NamespacedName{Namespace: "default", Name: "envtest-byo-auth-token"}
	byo := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Data:       map[string][]byte{"custom-key": []byte("byo-envtest-token")},
	}
	require.NoError(t, envK8sClient.Create(context.Background(), byo))
	t.Cleanup(func() {
		_ = envK8sClient.Delete(context.Background(), byo)
	})

	cfg := &Config{
		ProxyAuthSecretRef:      key.Namespace + "/" + key.Name,
		ProxyAuthSecretKey:      "custom-key",
		ProxyAuthSecretGenerate: false,
	}

	resolved, err := resolveProxyAuthToken(context.Background(), mgr, cfg)
	require.NoError(t, err)
	assert.Equal(t, "byo-envtest-token", resolved.ProxyAuthToken)
}

// TestResolveProxyAuthToken_SecretRefWinsOverDirectToken pins the documented
// precedence (Config.ProxyAuthSecretRef's doc comment, the --proxy-auth-token
// CLI help text, and docs/configuration/controller.md all call this out):
// when both the direct-value and secret-ref paths are set, the secret-ref
// resolution wins and the direct value is discarded, never merged or
// preferred as a fallback.
func TestResolveProxyAuthToken_SecretRefWinsOverDirectToken(t *testing.T) {
	mgr := newUnstartedTestManager(t)

	key := types.NamespacedName{Namespace: "default", Name: "envtest-precedence-auth-token"}
	cfg := &Config{
		ProxyAuthToken:          "should-be-overridden",
		ProxyAuthSecretRef:      key.Namespace + "/" + key.Name,
		ProxyAuthSecretGenerate: true,
	}

	resolved, err := resolveProxyAuthToken(context.Background(), mgr, cfg)
	require.NoError(t, err)
	assert.NotEqual(t, "should-be-overridden", resolved.ProxyAuthToken)
	assert.NotEmpty(t, resolved.ProxyAuthToken)

	var secret corev1.Secret
	require.NoError(t, envK8sClient.Get(context.Background(), key, &secret))
	t.Cleanup(func() {
		_ = envK8sClient.Delete(context.Background(), &secret)
	})
}

// TestResolveProxyAuthToken_BringYourOwnMissingFailsClosedAgainstRealManager
// pins the entry point's own propagation of the fail-closed contract
// documented in docs/operations/troubleshooting.md and docs/guides/l7-proxy.md:
// a missing bring-your-own Secret must surface as an error out of
// resolveProxyAuthToken itself, through the real client.New(mgr.GetConfig(),
// ...) path Run() actually uses -- not just out of the lower-level
// ensureProxyAuthSecret, which TestEnsureProxyAuthSecret_BringYourOwnMissingFailsClosed
// already covers with a fake client.
func TestResolveProxyAuthToken_BringYourOwnMissingFailsClosedAgainstRealManager(t *testing.T) {
	mgr := newUnstartedTestManager(t)

	key := types.NamespacedName{Namespace: "default", Name: "envtest-byo-missing-auth-token"}
	cfg := &Config{
		ProxyAuthSecretRef:      key.Namespace + "/" + key.Name,
		ProxyAuthSecretGenerate: false,
	}

	resolved, err := resolveProxyAuthToken(context.Background(), mgr, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, errProxyAuthSecretNotFound)
	assert.Nil(t, resolved)

	var secret corev1.Secret
	assert.Error(t, envK8sClient.Get(context.Background(), key, &secret),
		"a failed bring-your-own resolution must never create the Secret it couldn't find")
}
