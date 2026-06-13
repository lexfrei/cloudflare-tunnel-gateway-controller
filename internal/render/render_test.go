package render_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

func testInput(gatewayName string) render.Input {
	return render.Input{
		Gateway: &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: gatewayName, Namespace: "tenant-a"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel"},
		},
		Config: &v1alpha1.GatewayConfig{
			ObjectMeta: metav1.ObjectMeta{Name: gatewayName + "-config", Namespace: "tenant-a"},
			Spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-token"},
			},
		},
		TunnelToken: "raw-token-bytes",
		Defaults: render.Defaults{
			ProxyImage:     "ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller-proxy:v9.9.9",
			TunnelProtocol: "auto",
		},
	}
}

// TestProxyDeployment_CoreShape pins the load-bearing parts of the rendered
// Deployment: namespace-local placement, the tunnel token env wired from the
// GatewayConfig's Secret, the controller-supplied image, the HA replica
// default, probes on the config API, and the drain-aware grace period.
func TestProxyDeployment_CoreShape(t *testing.T) {
	t.Parallel()

	deployment := render.ProxyDeployment(testInput("edge"))

	assert.Equal(t, "cf-proxy-edge", deployment.Name)
	assert.Equal(t, "tenant-a", deployment.Namespace)

	require.NotNil(t, deployment.Spec.Replicas)
	assert.Equal(t, int32(2), *deployment.Spec.Replicas, "HA default: one connector pod is a tunnel availability hazard")

	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
	container := deployment.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller-proxy:v9.9.9", container.Image)

	var tokenEnv *corev1.EnvVar

	for i := range container.Env {
		if container.Env[i].Name == "TUNNEL_TOKEN" {
			tokenEnv = &container.Env[i]
		}
	}

	require.NotNil(t, tokenEnv, "TUNNEL_TOKEN env must be present")
	require.NotNil(t, tokenEnv.ValueFrom)
	require.NotNil(t, tokenEnv.ValueFrom.SecretKeyRef)
	assert.Equal(t, "edge-token", tokenEnv.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "tunnel-token", tokenEnv.ValueFrom.SecretKeyRef.Key)

	require.NotNil(t, deployment.Spec.Template.Spec.TerminationGracePeriodSeconds)
	assert.Equal(t, int64(45), *deployment.Spec.Template.Spec.TerminationGracePeriodSeconds,
		"drain window (30s) + 15s headroom so kubelet never SIGKILLs mid-drain")

	require.NotNil(t, container.ReadinessProbe)
	assert.Equal(t, "/readyz", container.ReadinessProbe.HTTPGet.Path)
	require.NotNil(t, container.LivenessProbe)
	assert.Equal(t, "/healthz", container.LivenessProbe.HTTPGet.Path)

	// Chart parity: the chart's proxy startupProbe budget is 30×5s=150s —
	// tunnel connector registration against the Cloudflare edge can be slow
	// on cold starts, and a tighter rendered budget would crash-loop planes
	// the shared chart deployment tolerates.
	require.NotNil(t, container.StartupProbe)
	assert.Equal(t, int32(30), container.StartupProbe.FailureThreshold)
	assert.Equal(t, int32(5), container.StartupProbe.PeriodSeconds)

	// The selector is the contract with the Service and must carry the
	// per-Gateway label so endpoint discovery is Gateway-scoped.
	assert.Equal(t, "edge", deployment.Spec.Selector.MatchLabels["cf.k8s.lex.la/gateway"])
	assert.Equal(t, deployment.Spec.Selector.MatchLabels, filterKeys(
		deployment.Spec.Template.Labels, deployment.Spec.Selector.MatchLabels))
}

// TestProxyDeployment_LongGatewayNameLabelTruncated pins label-value validity
// for Gateway names past the 63-character label cap (names go to 253): the
// raw name as a label value would make the apiserver reject the whole render
// on every reconcile. The truncated value must stay consistent between the
// selector and the pod template (the selector is immutable), and
// GatewayLabelValue is the only sanctioned mapping.
func TestProxyDeployment_LongGatewayNameLabelTruncated(t *testing.T) {
	t.Parallel()

	longName := strings.Repeat("very-long-gateway-name-", 4)
	input := testInput(longName)

	deployment := render.ProxyDeployment(input)

	labelValue := deployment.Spec.Selector.MatchLabels["cf.k8s.lex.la/gateway"]
	assert.LessOrEqual(t, len(labelValue), 63, "label values cap at 63 characters")
	assert.Equal(t, render.GatewayLabelValue(longName), labelValue)
	assert.Equal(t, labelValue, deployment.Spec.Template.Labels["cf.k8s.lex.la/gateway"],
		"selector and template must agree or the Deployment is invalid")

	service := render.ConfigService(input)
	assert.Equal(t, labelValue, service.Spec.Selector["cf.k8s.lex.la/gateway"],
		"the Service must select the same pods")
}

// TestRenderedNames_SanitizeDots pins valid rendered names for a Gateway whose
// name contains dots: Gateway names are DNS-1123 subdomains (dots legal),
// Service and label names are DNS-1123 labels (dots illegal). Unsanitized,
// `my.edge` → Service `cf-proxy-my.edge-config`, rejected by the apiserver on
// every reconcile. Distinct inputs must not collide on the rendered name.
func TestRenderedNames_SanitizeDots(t *testing.T) {
	t.Parallel()

	dotted := testInput("my.edge")

	deployment := render.ProxyDeployment(dotted)
	assert.NotContains(t, deployment.Name, ".", "rendered names must be DNS-1123 labels")

	service := render.ConfigService(dotted)
	assert.NotContains(t, service.Name, ".")
	assert.NotContains(t, service.Spec.Selector["cf.k8s.lex.la/gateway"], ".",
		"selector label values must not contain dots")

	// "my.edge" and "my-edge" must render to DIFFERENT names (no collision).
	assert.NotEqual(t,
		render.ProxyDeployment(testInput("my.edge")).Name,
		render.ProxyDeployment(testInput("my-edge")).Name,
		"distinct Gateway names must not collide on the rendered name")
}

// TestRenderedNames_AlwaysValidDNS1123 pins that every rendered name is a
// valid DNS-1123 label (lowercase alphanumeric or '-', starts/ends
// alphanumeric, <= 63 chars) across ordinary, dotted, uppercase, long, and
// dash-heavy Gateway names — the apiserver rejects anything else on every
// reconcile.
func TestRenderedNames_AlwaysValidDNS1123(t *testing.T) {
	t.Parallel()

	dns1123 := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

	names := []string{
		"edge", "My.Edge", "UPPER", strings.Repeat("a-", 40), "a.b.c.d", "x---y",
		".edge", "-edge", "edge.", "...", "9-lead",
	}

	for _, name := range names {
		input := testInput(name)

		for label, value := range map[string]string{
			"deployment": render.DeploymentName(input.Gateway),
			"service":    render.ConfigServiceName(input.Gateway),
			"authsecret": render.GeneratedAuthSecretName(input.Gateway),
			"labelvalue": render.GatewayLabelValue(name),
		} {
			assert.LessOrEqual(t, len(value), 63, "%s for %q exceeds 63 chars", label, name)
			assert.Regexp(t, dns1123, value, "%s for %q is not a valid DNS-1123 label", label, name)
		}
	}
}

func filterKeys(all map[string]string, want map[string]string) map[string]string {
	out := make(map[string]string, len(want))

	for key := range want {
		if value, ok := all[key]; ok {
			out[key] = value
		}
	}

	return out
}

// TestProxyDeployment_AuthTokenAlwaysWired pins the fail-secure default for
// the per-Gateway config API: WITHOUT an explicit authTokenSecretRef the env
// must wire to the controller-GENERATED auth Secret — an unauthenticated
// tenant config API would accept PUT /config from any pod that can reach it,
// the exact cross-tenant hijack the dedicated plane exists to prevent.
func TestProxyDeployment_AuthTokenAlwaysWired(t *testing.T) {
	t.Parallel()

	deployment := render.ProxyDeployment(testInput("edge"))
	container := deployment.Spec.Template.Spec.Containers[0]

	var authEnv *corev1.EnvVar

	for i := range container.Env {
		if container.Env[i].Name == "PROXY_AUTH_TOKEN" {
			authEnv = &container.Env[i]
		}
	}

	require.NotNil(t, authEnv, "PROXY_AUTH_TOKEN must be wired even without an explicit authTokenSecretRef")
	require.NotNil(t, authEnv.ValueFrom.SecretKeyRef)
	assert.Equal(t, render.GeneratedAuthSecretName(testInput("edge").Gateway), authEnv.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "auth-token", authEnv.ValueFrom.SecretKeyRef.Key)

	explicit := testInput("edge")
	explicit.Config.Spec.AuthTokenSecretRef = &v1alpha1.LocalSecretReference{Name: "tenant-auth"}
	explicitContainer := render.ProxyDeployment(explicit).Spec.Template.Spec.Containers[0]

	for _, env := range explicitContainer.Env {
		if env.Name == "PROXY_AUTH_TOKEN" {
			assert.Equal(t, "tenant-auth", env.ValueFrom.SecretKeyRef.Name,
				"an explicit ref must win over the generated Secret")
		}
	}
}

// TestProxyDeployment_NoServiceAccountTokenAutomount pins the hardening for
// tenant-chosen images: the proxy never talks to the Kubernetes API, and the
// pod would otherwise mount the namespace default ServiceAccount token —
// handing whatever that SA can do to an arbitrary spec.image.
func TestProxyDeployment_NoServiceAccountTokenAutomount(t *testing.T) {
	t.Parallel()

	deployment := render.ProxyDeployment(testInput("edge"))

	require.NotNil(t, deployment.Spec.Template.Spec.AutomountServiceAccountToken)
	assert.False(t, *deployment.Spec.Template.Spec.AutomountServiceAccountToken)
}

// TestProxyDeployment_ReplicaModes pins the replica ownership contract:
// explicit replicas win; autoscaling leaves replicas nil so the HPA owns the
// count and rolling updates don't fight it.
func TestProxyDeployment_ReplicaModes(t *testing.T) {
	t.Parallel()

	three := int32(3)

	explicit := testInput("edge")
	explicit.Config.Spec.Replicas = &three
	withExplicit := render.ProxyDeployment(explicit)
	require.NotNil(t, withExplicit.Spec.Replicas)
	assert.Equal(t, int32(3), *withExplicit.Spec.Replicas)

	autoscaled := testInput("edge")
	autoscaled.Config.Spec.Autoscaling = &v1alpha1.ProxyAutoscaling{MaxReplicas: 5, TargetInflightPerPod: 50}
	withHPA := render.ProxyDeployment(autoscaled)
	assert.Nil(t, withHPA.Spec.Replicas, "the HPA owns the replica count when autoscaling is set")
}

// TestProxyDeployment_TokenHashAnnotation pins the rotation contract: the pod
// template carries a hash of the connector token so a token rotation rolls
// the pods.
func TestProxyDeployment_TokenHashAnnotation(t *testing.T) {
	t.Parallel()

	first := render.ProxyDeployment(testInput("edge"))
	hash := first.Spec.Template.Annotations["cf.k8s.lex.la/tunnel-token-hash"]
	require.NotEmpty(t, hash)

	rotated := testInput("edge")
	rotated.TunnelToken = "different-token"
	second := render.ProxyDeployment(rotated)
	assert.NotEqual(t, hash, second.Spec.Template.Annotations["cf.k8s.lex.la/tunnel-token-hash"],
		"a rotated token must change the pod template (rolling restart)")
}

// TestProxyDeployment_InfrastructureLabelsPropagate pins the Gateway API
// SHOULD: infrastructure.labels/annotations are applied to rendered resources
// and the pod template — with controller-owned keys protected from override.
func TestProxyDeployment_InfrastructureLabelsPropagate(t *testing.T) {
	t.Parallel()

	input := testInput("edge")
	input.Gateway.Spec.Infrastructure = &gatewayv1.GatewayInfrastructure{
		Labels: map[gatewayv1.LabelKey]gatewayv1.LabelValue{
			"team":                  "a",
			"cf.k8s.lex.la/gateway": "spoofed",
		},
		Annotations: map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue{
			"note": "tenant-supplied",
		},
	}

	deployment := render.ProxyDeployment(input)

	assert.Equal(t, "a", deployment.Labels["team"])
	assert.Equal(t, "a", deployment.Spec.Template.Labels["team"])
	assert.Equal(t, "tenant-supplied", deployment.Annotations["note"])
	assert.Equal(t, "edge", deployment.Labels["cf.k8s.lex.la/gateway"],
		"controller-owned label keys win over infrastructure labels")
	assert.Equal(t, "edge", deployment.Spec.Template.Labels["cf.k8s.lex.la/gateway"])
}

// TestProxyDeployment_OptionalKnobs pins image/resources/auth/protocol
// overrides.
func TestProxyDeployment_OptionalKnobs(t *testing.T) {
	t.Parallel()

	input := testInput("edge")
	input.Config.Spec.Image = "example.com/custom-proxy:v1"
	input.Config.Spec.Resources = &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
	}
	input.Config.Spec.AuthTokenSecretRef = &v1alpha1.LocalSecretReference{Name: "edge-auth"}
	input.Defaults.TunnelProtocol = "http2"

	deployment := render.ProxyDeployment(input)
	container := deployment.Spec.Template.Spec.Containers[0]

	assert.Equal(t, "example.com/custom-proxy:v1", container.Image)
	assert.True(t, container.Resources.Limits[corev1.ResourceCPU].Equal(resource.MustParse("2")))

	envByName := map[string]corev1.EnvVar{}
	for _, env := range container.Env {
		envByName[env.Name] = env
	}

	require.Contains(t, envByName, "PROXY_AUTH_TOKEN")
	assert.Equal(t, "edge-auth", envByName["PROXY_AUTH_TOKEN"].ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "auth-token", envByName["PROXY_AUTH_TOKEN"].ValueFrom.SecretKeyRef.Key)

	require.Contains(t, envByName, "PROXY_TUNNEL_PROTOCOL")
	assert.Equal(t, "http2", envByName["PROXY_TUNNEL_PROTOCOL"].Value)
}

// TestConfigService_Shape pins the headless config Service: pod IPs published
// before readiness (the controller pushes config to not-yet-ready pods —
// readiness depends on that very config), Gateway-scoped selector.
func TestConfigService_Shape(t *testing.T) {
	t.Parallel()

	service := render.ConfigService(testInput("edge"))

	assert.Equal(t, "cf-proxy-edge-config", service.Name)
	assert.Equal(t, "tenant-a", service.Namespace)
	assert.Equal(t, corev1.ClusterIPNone, service.Spec.ClusterIP)
	assert.True(t, service.Spec.PublishNotReadyAddresses)
	assert.Equal(t, "edge", service.Spec.Selector["cf.k8s.lex.la/gateway"])
	require.Len(t, service.Spec.Ports, 1)
	assert.Equal(t, int32(8081), service.Spec.Ports[0].Port)
}

// TestNames_TruncatedDeterministically pins the 63-char DNS label contract:
// long Gateway names truncate with a stable hash suffix, identical for the
// same input and distinct for different inputs.
func TestNames_TruncatedDeterministically(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("g", 80)

	first := render.ProxyDeployment(testInput(long))
	second := render.ProxyDeployment(testInput(long))
	other := render.ProxyDeployment(testInput(strings.Repeat("h", 80)))

	assert.LessOrEqual(t, len(first.Name), 63)
	assert.Equal(t, first.Name, second.Name, "naming must be deterministic")
	assert.NotEqual(t, first.Name, other.Name, "different gateways must not collide")

	service := render.ConfigService(testInput(long))
	assert.LessOrEqual(t, len(service.Name), 63)
}

// TestConfigEndpointURL pins the URL the controller pushes config to.
func TestConfigEndpointURL(t *testing.T) {
	t.Parallel()

	input := testInput("edge")

	url := render.ConfigEndpointURL(input.Gateway, "cluster.local")
	assert.Equal(t, "http://cf-proxy-edge-config.tenant-a.svc.cluster.local:8081/config", url)
}
