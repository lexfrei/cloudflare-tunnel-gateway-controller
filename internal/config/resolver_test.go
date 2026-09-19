package config_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

func TestNewResolver(t *testing.T) {
	t.Parallel()

	fakeClient := setupFakeClient()
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	require.NotNil(t, resolver)
}

func TestResolveFromGatewayClass_Valid(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-api-token"),
		},
	}

	tunnelSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel-token",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"tunnel-token": []byte("test-tunnel-token"),
		},
	}

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			AccountID: "test-account-id",
			TunnelID:  "12345678-1234-1234-1234-123456789abc",
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, tunnelSecret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, "test-api-token", resolved.APIToken)
	assert.Equal(t, "test-account-id", resolved.AccountID)
	assert.Equal(t, "12345678-1234-1234-1234-123456789abc", resolved.TunnelID)
	assert.Equal(t, "test-config", resolved.ConfigName)
}

func TestResolveFromGatewayClass_MissingParametersRef(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-class",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef:  nil,
		},
	}

	fakeClient := setupFakeClient(gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no parametersRef")
}

func TestResolveFromGatewayClass_WrongGroup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-class",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: "wrong.group",
				Kind:  "GatewayClassConfig",
				Name:  "test-config",
			},
		},
	}

	fakeClient := setupFakeClient(gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported parametersRef group")
}

func TestResolveFromGatewayClass_WrongKind(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-class",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  "WrongKind",
				Name:  "test-config",
			},
		},
	}

	fakeClient := setupFakeClient(gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported parametersRef kind")
}

func TestResolveFromGatewayClass_ConfigNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClass := newGatewayClass("test-class", "non-existent-config")

	fakeClient := setupFakeClient(gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get GatewayClassConfig")
}

func TestResolveFromGatewayClass_SecretNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "non-existent-secret",
				Namespace: "default",
			},
			TunnelID: "12345678-1234-1234-1234-123456789abc",
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get Cloudflare credentials secret")
}

func TestResolveFromGatewayClass_MissingAPIToken(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"wrong-key": []byte("test-api-token"),
		},
	}

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			TunnelID: "12345678-1234-1234-1234-123456789abc",
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not contain key api-token")
}

func TestResolveFromGatewayClassName_Valid(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-api-token"),
		},
	}

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			TunnelID: "12345678-1234-1234-1234-123456789abc",
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClassName(ctx, "test-class")

	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, "test-api-token", resolved.APIToken)
}

func TestResolveFromGatewayClassName_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupFakeClient()
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClassName(ctx, "non-existent")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get GatewayClass")
}

func TestResolveConfig_DefaultNamespace(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "my-default-ns",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-api-token"),
		},
	}

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "",
			},
			TunnelID: "12345678-1234-1234-1234-123456789abc",
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "my-default-ns", cfmetrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	assert.Equal(t, "test-api-token", resolved.APIToken)
}

func TestResolveConfig_AccountIDFromSecret(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token":  []byte("test-api-token"),
			"account-id": []byte("secret-account-id"),
		},
	}

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			AccountID: "",
			TunnelID:  "12345678-1234-1234-1234-123456789abc",
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	assert.Equal(t, "secret-account-id", resolved.AccountID)
}

func TestResolveConfig_AccountIDFromSpec(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token":  []byte("test-api-token"),
			"account-id": []byte("secret-account-id"),
		},
	}

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			AccountID: "spec-account-id",
			TunnelID:  "12345678-1234-1234-1234-123456789abc",
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	assert.Equal(t, "spec-account-id", resolved.AccountID)
}

func TestResolveConfig_CustomAPITokenKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"my-custom-token-key": []byte("test-api-token"),
		},
	}

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
				Key:       "my-custom-token-key",
			},
			TunnelID: "12345678-1234-1234-1234-123456789abc",
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	assert.Equal(t, "test-api-token", resolved.APIToken)
}

func TestCreateCloudflareClient(t *testing.T) {
	t.Parallel()

	fakeClient := setupFakeClient()
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	resolved := &config.ResolvedConfig{
		APIToken: "test-api-token",
	}

	cfClient := resolver.CreateCloudflareClient(resolved)

	require.NotNil(t, cfClient)
}

func TestResolveAccountID_FromResolvedConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupFakeClient()
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	resolved := &config.ResolvedConfig{
		AccountID:  "already-resolved-account-id",
		ConfigName: "test-config",
	}

	accountID, err := resolver.ResolveAccountID(ctx, nil, resolved)

	require.NoError(t, err)
	assert.Equal(t, "already-resolved-account-id", accountID)
}

func TestGetConfigForGatewayClass_Valid(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID: "12345678-1234-1234-1234-123456789abc",
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	cfg, err := resolver.GetConfigForGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	assert.Equal(t, "test-config", cfg.Name)
	assert.Equal(t, "12345678-1234-1234-1234-123456789abc", cfg.Spec.TunnelID)
}

func TestGetConfigForGatewayClass_MissingParametersRef(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-class",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef:  nil,
		},
	}

	fakeClient := setupFakeClient(gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	_, err := resolver.GetConfigForGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no parametersRef")
}

func TestGetConfigForGatewayClass_WrongGroupKind(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-class",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: "wrong.group",
				Kind:  "WrongKind",
				Name:  "test-config",
			},
		},
	}

	fakeClient := setupFakeClient(gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	_, err := resolver.GetConfigForGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported parametersRef")
}

func TestGetConfigForGatewayClass_ConfigNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClass := newGatewayClass("test-class", "non-existent-config")

	fakeClient := setupFakeClient(gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	_, err := resolver.GetConfigForGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get GatewayClassConfig")
}

// fakeAccountsAPI serves the Cloudflare accounts endpoint, handing each API
// token the account that token belongs to and counting how often it was asked.
type fakeAccountsAPI struct {
	server        *httptest.Server
	accountByAuth map[string]string
	calls         atomic.Int32
}

func newFakeAccountsAPI(t *testing.T, accountByToken map[string]string) *fakeAccountsAPI {
	t.Helper()

	api := &fakeAccountsAPI{accountByAuth: make(map[string]string, len(accountByToken))}
	for token, account := range accountByToken {
		api.accountByAuth["Bearer "+token] = account
	}

	api.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		api.calls.Add(1)

		account, ok := api.accountByAuth[req.Header.Get("Authorization")]
		if !ok {
			writer.WriteHeader(http.StatusUnauthorized)

			return
		}

		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"success": true,
			"errors":  []any{},
			"result":  []map[string]any{{"id": account, "name": account}},
		})
	}))

	t.Cleanup(api.server.Close)

	return api
}

func (a *fakeAccountsAPI) clientFor(token string) *cloudflare.Client {
	return cloudflare.NewClient(option.WithAPIToken(token), option.WithBaseURL(a.server.URL))
}

// TestResolveAccountID_RotatedCredentialIsNotServedFromCache covers rotating a
// GatewayClassConfig's credentials Secret to a token belonging to a DIFFERENT
// Cloudflare account. The config name does not change across such a rotation,
// so a cache keyed on the name alone keeps answering with the old account for
// the life of the process, and every tunnel write is addressed to an account
// the new token has no access to until someone restarts the controller.
func TestResolveAccountID_RotatedCredentialIsNotServedFromCache(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	api := newFakeAccountsAPI(t, map[string]string{
		"token-first":  "account-first",
		"token-second": "account-second",
	})

	resolver := config.NewResolver(setupFakeClient(), "default", cfmetrics.NewNoopCollector())

	first, err := resolver.ResolveAccountID(ctx, api.clientFor("token-first"), &config.ResolvedConfig{
		ConfigName: "test-config",
		APIToken:   "token-first",
	})
	require.NoError(t, err)
	assert.Equal(t, "account-first", first)

	second, err := resolver.ResolveAccountID(ctx, api.clientFor("token-second"), &config.ResolvedConfig{
		ConfigName: "test-config",
		APIToken:   "token-second",
	})
	require.NoError(t, err)
	assert.Equal(t, "account-second", second, "a rotated token must re-detect, not replay the previous account")
}

// TestResolveAccountID_CachesPerUnchangedCredential pins the reason the cache
// exists: repeated resolution on an unchanged credential must not re-ask the
// Cloudflare API.
func TestResolveAccountID_CachesPerUnchangedCredential(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	api := newFakeAccountsAPI(t, map[string]string{"token-first": "account-first"})

	resolver := config.NewResolver(setupFakeClient(), "default", cfmetrics.NewNoopCollector())

	for range 3 {
		accountID, err := resolver.ResolveAccountID(ctx, api.clientFor("token-first"), &config.ResolvedConfig{
			ConfigName: "test-config",
			APIToken:   "token-first",
		})
		require.NoError(t, err)
		assert.Equal(t, "account-first", accountID)
	}

	assert.Equal(t, int32(1), api.calls.Load(), "an unchanged credential is resolved once and then served from cache")
}

func setupFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
}

func newGatewayClass(name, configName string) *gatewayv1.GatewayClass {
	return &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  config.ParametersRefKind,
				Name:  configName,
			},
		},
	}
}
