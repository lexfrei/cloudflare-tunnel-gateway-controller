package config_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

const (
	testTunnelUUID  = "550e8400-e29b-41d4-a716-446655440000"
	testAccountTag  = "abcdef0123456789abcdef0123456789"
	testGwNamespace = "tenant-a"
)

// testTunnelToken builds a syntactically-valid cloudflared connector token
// (base64 JSON {"a","s","t"}).
func testTunnelToken(t *testing.T) string {
	t.Helper()

	payload, err := json.Marshal(map[string]any{
		"a": testAccountTag,
		"s": base64.StdEncoding.EncodeToString([]byte("tunnel-secret")),
		"t": testTunnelUUID,
	})
	require.NoError(t, err)

	return base64.StdEncoding.EncodeToString(payload)
}

func perGatewayScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	return scheme
}

// gatewayWithInfra builds a Gateway whose infrastructure.parametersRef points
// at a GatewayConfig of the given name (group/kind overridable for the
// invalid cases).
func gatewayWithInfra(group, kind, name string) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: testGwNamespace},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: &gatewayv1.LocalParametersReference{
					Group: gatewayv1.Group(group),
					Kind:  gatewayv1.Kind(kind),
					Name:  name,
				},
			},
		},
	}
}

// classFixtures returns the GatewayClass + GatewayClassConfig + API-token
// Secret chain used for class-level credential fallback.
func classFixtures() []runtime.Object {
	return []runtime.Object{
		&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: "cf.k8s.lex.la/tunnel-controller",
				ParametersRef: &gatewayv1.ParametersReference{
					Group: "cf.k8s.lex.la",
					Kind:  "GatewayClassConfig",
					Name:  "class-config",
				},
			},
		},
		&v1alpha1.GatewayClassConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "class-config"},
			Spec: v1alpha1.GatewayClassConfigSpec{
				TunnelID: "99999999-9999-4999-8999-999999999999",
				CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
					Name:      "class-credentials",
					Namespace: "cf-system",
				},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "class-credentials", Namespace: "cf-system"},
			Data:       map[string][]byte{"api-token": []byte("class-api-token")},
		},
	}
}

func tokenSecret(t *testing.T) *corev1.Secret {
	t.Helper()

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-tunnel-token", Namespace: testGwNamespace},
		Data:       map[string][]byte{"tunnel-token": []byte(testTunnelToken(t))},
	}
}

// generatedAuthSecret is the controller-created config-API auth Secret a
// Gateway named "edge" (the gatewayWithInfra fixture) gets when its
// GatewayConfig declares no explicit authTokenSecretRef.
func generatedAuthSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cf-proxy-edge-auth", Namespace: testGwNamespace},
		Data:       map[string][]byte{"auth-token": []byte("generated-bearer")},
	}
}

func newGatewayResolver(t *testing.T, objects ...runtime.Object) *config.Resolver {
	t.Helper()

	builder := fake.NewClientBuilder().WithScheme(perGatewayScheme(t))
	for _, obj := range objects {
		builder = builder.WithRuntimeObjects(obj)
	}

	return config.NewResolver(builder.Build(), "cf-system", cfmetrics.NewNoopCollector())
}

// TestResolveForGateway_SharedModeReturnsNil pins back-compat: a Gateway
// without infrastructure.parametersRef stays on the shared data plane.
func TestResolveForGateway_SharedModeReturnsNil(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: testGwNamespace},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel"},
	}

	resolver := newGatewayResolver(t)

	resolved, err := resolver.ResolveForGateway(context.Background(), gateway)
	require.NoError(t, err)
	assert.Nil(t, resolved, "no parametersRef means shared mode, not an error")
}

// TestResolveForGateway_HappyPath_ClassCredentialFallback pins the credential
// precedence: tunnel identity from the connector token, API token from the
// GatewayClass config when the GatewayConfig declares no override.
func TestResolveForGateway_HappyPath_ClassCredentialFallback(t *testing.T) {
	t.Parallel()

	gwConfig := &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: testGwNamespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-tunnel-token"},
		},
	}

	objects := append(classFixtures(), gwConfig, tokenSecret(t), generatedAuthSecret())
	resolver := newGatewayResolver(t, objects...)

	resolved, err := resolver.ResolveForGateway(context.Background(), gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"))
	require.NoError(t, err)
	require.NotNil(t, resolved)

	assert.Equal(t, testTunnelUUID, resolved.TunnelID, "tunnel ID must come from the connector token")
	assert.Equal(t, testAccountTag, resolved.AccountID, "account must come from the connector token, never the class")
	assert.Equal(t, "class-api-token", resolved.APIToken, "API token falls back to the GatewayClass credentials")
	assert.Equal(t, testTunnelToken(t), resolved.TunnelToken)
	assert.Equal(t, testGwNamespace, resolved.TunnelTokenSecret.Namespace)
	assert.Equal(t, "edge-tunnel-token", resolved.TunnelTokenSecret.Name)
	require.NotNil(t, resolved.GatewayConfig)
	assert.Equal(t, "edge-config", resolved.GatewayConfig.Name)
}

// TestResolveForGateway_CredentialOverride pins that a GatewayConfig-level
// cloudflareCredentialsSecretRef wins over the class credentials, defaulting
// its namespace to the GatewayConfig's own namespace.
func TestResolveForGateway_CredentialOverride(t *testing.T) {
	t.Parallel()

	gwConfig := &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: testGwNamespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef:           v1alpha1.LocalSecretReference{Name: "edge-tunnel-token"},
			CloudflareCredentialsSecretRef: &v1alpha1.LocalSecretReference{Name: "tenant-credentials"},
		},
	}
	tenantCredentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-credentials", Namespace: testGwNamespace},
		Data:       map[string][]byte{"api-token": []byte("tenant-api-token")},
	}

	objects := append(classFixtures(), gwConfig, tokenSecret(t), tenantCredentials, generatedAuthSecret())
	resolver := newGatewayResolver(t, objects...)

	resolved, err := resolver.ResolveForGateway(context.Background(), gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"))
	require.NoError(t, err)
	assert.Equal(t, "tenant-api-token", resolved.APIToken)
}

// TestResolveForGateway_AuthToken pins the optional config-API bearer token.
func TestResolveForGateway_AuthToken(t *testing.T) {
	t.Parallel()

	gwConfig := &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: testGwNamespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-tunnel-token"},
			AuthTokenSecretRef:   &v1alpha1.LocalSecretReference{Name: "edge-auth"},
		},
	}
	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-auth", Namespace: testGwNamespace},
		Data:       map[string][]byte{"auth-token": []byte("bearer-secret")},
	}

	objects := append(classFixtures(), gwConfig, tokenSecret(t), authSecret)
	resolver := newGatewayResolver(t, objects...)

	resolved, err := resolver.ResolveForGateway(context.Background(), gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"))
	require.NoError(t, err)
	assert.Equal(t, "bearer-secret", resolved.AuthToken)
}

// TestResolveForGateway_GeneratedAuthTokenFallback pins the fail-secure
// default: a GatewayConfig WITHOUT authTokenSecretRef resolves its push auth
// from the controller-generated Secret — never an empty token, which would
// push to (and render) an unauthenticated tenant config API.
func TestResolveForGateway_GeneratedAuthTokenFallback(t *testing.T) {
	t.Parallel()

	gwConfig := &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: testGwNamespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-tunnel-token"},
		},
	}
	generated := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cf-proxy-edge-auth", Namespace: testGwNamespace},
		Data:       map[string][]byte{"auth-token": []byte("generated-bearer")},
	}

	objects := append(classFixtures(), gwConfig, tokenSecret(t), generated)
	resolver := newGatewayResolver(t, objects...)

	resolved, err := resolver.ResolveForGateway(context.Background(), gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"))
	require.NoError(t, err)
	assert.Equal(t, "generated-bearer", resolved.AuthToken,
		"without an explicit ref the generated Secret IS the push credential")
}

// TestResolveForGateway_MissingGeneratedAuthSecretIsTransient pins the
// bootstrap window: the generated Secret is created by the infra reconciler,
// so its absence is a self-healing not-yet state — a plain error for backoff,
// NOT ErrInvalidParameters (nothing in the user's spec is wrong), and never a
// silent empty token.
func TestResolveForGateway_MissingGeneratedAuthSecretIsTransient(t *testing.T) {
	t.Parallel()

	gwConfig := &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: testGwNamespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-tunnel-token"},
		},
	}

	objects := append(classFixtures(), gwConfig, tokenSecret(t))
	resolver := newGatewayResolver(t, objects...)

	_, err := resolver.ResolveForGateway(context.Background(), gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"))
	require.Error(t, err, "an absent generated auth Secret must not resolve to an unauthenticated plane")
	assert.NotErrorIs(t, err, config.ErrInvalidParameters,
		"the bootstrap window is transient, not a user spec problem")
}

// TestResolveForGateway_InvalidParameters pins the spec-mandated rejection
// shape: an unsupported group/kind, a missing GatewayConfig, a missing or
// malformed token Secret all classify as ErrInvalidParameters so the Gateway
// reconciler can set Accepted=False with reason InvalidParameters.
func TestResolveForGateway_InvalidParameters(t *testing.T) {
	t.Parallel()

	// Factory, not a shared pointer: each parallel subtest gets its own object
	// because fake.ClientBuilder.Build mutates the supplied objects'
	// ResourceVersion (data race on a shared instance).
	validGwConfig := func() *v1alpha1.GatewayConfig {
		return &v1alpha1.GatewayConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: testGwNamespace},
			Spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-tunnel-token"},
			},
		}
	}

	tests := []struct {
		name    string
		gateway *gatewayv1.Gateway
		objects []runtime.Object
	}{
		{
			name:    "unsupported kind",
			gateway: gatewayWithInfra("cf.k8s.lex.la", "ConfigMap", "edge-config"),
			objects: []runtime.Object{validGwConfig(), tokenSecret(t)},
		},
		{
			name:    "unsupported group",
			gateway: gatewayWithInfra("example.com", "GatewayConfig", "edge-config"),
			objects: []runtime.Object{validGwConfig(), tokenSecret(t)},
		},
		{
			name:    "missing GatewayConfig",
			gateway: gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "absent"),
			objects: []runtime.Object{tokenSecret(t)},
		},
		{
			name:    "missing token secret",
			gateway: gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"),
			objects: []runtime.Object{validGwConfig()},
		},
		{
			name:    "malformed token",
			gateway: gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"),
			objects: []runtime.Object{validGwConfig(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "edge-tunnel-token", Namespace: testGwNamespace},
				Data:       map[string][]byte{"tunnel-token": []byte("not-a-token")},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			objects := append(classFixtures(), tt.objects...)
			resolver := newGatewayResolver(t, objects...)

			_, err := resolver.ResolveForGateway(context.Background(), tt.gateway)
			require.Error(t, err)
			assert.ErrorIs(t, err, config.ErrInvalidParameters,
				"the reconciler maps this onto Accepted=False/InvalidParameters")
		})
	}
}

// TestResolveForGateway_ClassFallbackMissingSecretIsInvalidParameters pins
// that a deterministic failure in the CLASS-credential fallback path (here:
// the class credentials Secret is gone) classifies as ErrInvalidParameters,
// just like the override path — so the Gateway surfaces Accepted=False
// instead of silently backing off forever with no status.
func TestResolveForGateway_ClassFallbackMissingSecretIsInvalidParameters(t *testing.T) {
	t.Parallel()

	gwConfig := &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: testGwNamespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-tunnel-token"},
		},
	}

	// classFixtures()[2] is the class-credentials Secret; drop it so the class
	// fallback's Secret Get returns NotFound.
	fixtures := classFixtures()
	objects := append(fixtures[:2:2], gwConfig, tokenSecret(t), generatedAuthSecret())

	resolver := newGatewayResolver(t, objects...)

	_, err := resolver.ResolveForGateway(context.Background(), gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"))
	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrInvalidParameters,
		"a missing class credentials Secret is a deterministic user-fixable problem -> InvalidParameters")
}

// TestResolveForGateway_ClassFallbackEmptyTokenIsInvalidParameters pins that a
// deterministic non-NotFound failure in the class-credential fallback (here:
// the class credentials Secret exists but its api-token key is empty)
// classifies as ErrInvalidParameters too — not just the NotFound case — so the
// Gateway surfaces Accepted=False instead of backing off forever.
func TestResolveForGateway_ClassFallbackEmptyTokenIsInvalidParameters(t *testing.T) {
	t.Parallel()

	gwConfig := &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: testGwNamespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-tunnel-token"},
		},
	}

	// Replace the class-credentials Secret with one whose api-token key is
	// present but empty — a deterministic, user-fixable config error.
	fixtures := classFixtures()
	emptyTokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "class-credentials", Namespace: "cf-system"},
		Data:       map[string][]byte{"api-token": []byte("")},
	}
	objects := append(fixtures[:2:2], emptyTokenSecret, gwConfig, tokenSecret(t), generatedAuthSecret())

	resolver := newGatewayResolver(t, objects...)

	_, err := resolver.ResolveForGateway(context.Background(), gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"))
	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrInvalidParameters,
		"an empty class api-token key is a deterministic user-fixable problem -> InvalidParameters")
}

// TestResolveForGateway_OverrideCredentialFailClosedArms pins the
// GatewayConfig-level cloudflareCredentialsSecretRef override's fail-closed
// branches — a missing Secret and a present-but-empty api-token key — both
// classify as ErrInvalidParameters (the security-relevant arms, previously
// covered only on the class-fallback path).
func TestResolveForGateway_OverrideCredentialFailClosedArms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		objects []runtime.Object
	}{
		{
			name: "override credentials Secret missing",
			objects: []runtime.Object{&corev1.Secret{
				// the override Secret "override-creds" is deliberately absent
				ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: testGwNamespace},
			}},
		},
		{
			name: "override credentials api-token key empty",
			objects: []runtime.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "override-creds", Namespace: testGwNamespace},
				Data:       map[string][]byte{"api-token": []byte("")},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gwConfig := &v1alpha1.GatewayConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: testGwNamespace},
				Spec: v1alpha1.GatewayConfigSpec{
					TunnelTokenSecretRef:           v1alpha1.LocalSecretReference{Name: "edge-tunnel-token"},
					CloudflareCredentialsSecretRef: &v1alpha1.LocalSecretReference{Name: "override-creds"},
				},
			}

			objects := append(classFixtures(), gwConfig, tokenSecret(t), generatedAuthSecret())
			objects = append(objects, tt.objects...)
			resolver := newGatewayResolver(t, objects...)

			_, err := resolver.ResolveForGateway(context.Background(),
				gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"))
			require.Error(t, err)
			assert.ErrorIs(t, err, config.ErrInvalidParameters,
				"an unusable override credential is a user-fixable spec problem -> InvalidParameters")
		})
	}
}

// TestResolveForGateway_ExplicitAuthSecretMissingIsInvalidParameters pins that
// an explicit authTokenSecretRef pointing at an absent Secret fails closed as
// ErrInvalidParameters — distinct from the generated-Secret bootstrap window,
// which is transient (the user named a Secret that does not exist, so it is a
// spec problem, not a self-healing not-yet state).
func TestResolveForGateway_ExplicitAuthSecretMissingIsInvalidParameters(t *testing.T) {
	t.Parallel()

	gwConfig := &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: testGwNamespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-tunnel-token"},
			AuthTokenSecretRef:   &v1alpha1.LocalSecretReference{Name: "absent-auth"},
		},
	}

	objects := append(classFixtures(), gwConfig, tokenSecret(t))
	resolver := newGatewayResolver(t, objects...)

	_, err := resolver.ResolveForGateway(context.Background(),
		gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"))
	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrInvalidParameters,
		"a named-but-absent auth Secret is a user spec problem -> InvalidParameters")
}

// TestHasInfrastructureParametersRef pins the opt-in predicate the sync
// partitioner uses.
func TestHasInfrastructureParametersRef(t *testing.T) {
	t.Parallel()

	assert.False(t, config.HasInfrastructureParametersRef(&gatewayv1.Gateway{}))
	assert.True(t, config.HasInfrastructureParametersRef(gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "x")))
}

// errTransientAPIServer simulates an infrastructure failure (apiserver
// timeout, throttling) — NOT a user-fixable referent problem. It is a real
// apimachinery StatusError (Reason=Timeout) rather than a bare error, because
// the class-credential fallback path classifies retryability via
// apierrors.Is* predicates: a bare error there would be misread as a permanent
// config problem, which is not what a genuine apiserver timeout is.
var errTransientAPIServer = apierrors.NewTimeoutError("apiserver timeout", 1)

// TestResolveForGateway_TransientErrorsAreNotInvalidParameters pins the
// sentinel's contract: only deterministic referent failures (NotFound,
// missing key, garbled token) classify as ErrInvalidParameters. A transient
// infrastructure error must keep its own identity so the reconcilers retry
// with backoff instead of (a) stamping Accepted=False/InvalidParameters on a
// healthy Gateway and (b) failing its routes closed for the duration of an
// apiserver hiccup.
func TestResolveForGateway_TransientErrorsAreNotInvalidParameters(t *testing.T) {
	t.Parallel()

	gwConfig := &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: testGwNamespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-tunnel-token"},
		},
	}

	tests := []struct {
		name   string
		failOn string // object name whose Get fails transiently
	}{
		{name: "transient GatewayConfig read failure", failOn: "edge-config"},
		{name: "transient token Secret read failure", failOn: "edge-tunnel-token"},
		// The class-credential API-token fallback (gwConfig sets no
		// CloudflareCredentialsSecretRef override) reads the GatewayClass →
		// GatewayClassConfig → class-credentials Secret chain. A transient
		// failure on any link must propagate as retryable, exercising the
		// isRetryableAPIError arm in resolveGatewayAPIToken rather than being
		// misclassified as a permanent InvalidParameters.
		{name: "transient GatewayClass read failure", failOn: "cloudflare-tunnel"},
		{name: "transient GatewayClassConfig read failure", failOn: "class-config"},
		{name: "transient class-credentials Secret read failure", failOn: "class-credentials"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			objects := append(classFixtures(), gwConfig.DeepCopy(), tokenSecret(t))

			builder := fake.NewClientBuilder().WithScheme(perGatewayScheme(t))
			for _, obj := range objects {
				builder = builder.WithRuntimeObjects(obj)
			}

			failOn := tt.failOn
			builder = builder.WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if key.Name == failOn {
						return errTransientAPIServer
					}

					return cli.Get(ctx, key, obj, opts...)
				},
			})

			resolver := config.NewResolver(builder.Build(), "cf-system", cfmetrics.NewNoopCollector())

			_, err := resolver.ResolveForGateway(context.Background(),
				gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"))
			require.Error(t, err)
			assert.NotErrorIs(t, err, config.ErrInvalidParameters,
				"a transient infrastructure error must NOT classify as InvalidParameters")
		})
	}
}
