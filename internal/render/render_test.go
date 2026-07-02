package render_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

func testInput(gatewayName string) *render.Input {
	return &render.Input{
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

// TestProxyNetworkPolicy_LocksConfigAPI pins the ingress-only NetworkPolicy:
// it selects the proxy pods, opens ONLY the config-API port (8081) to the
// controller namespace (+ optional monitoring), never the proxy port (8080),
// and declares no egress.
func TestProxyNetworkPolicy_LocksConfigAPI(t *testing.T) {
	t.Parallel()

	gateway := testInput("edge").Gateway

	netpol := render.ProxyNetworkPolicy(render.NetworkPolicyInput{
		Gateway:             gateway,
		ControllerNamespace: "cf-system",
	})

	assert.Equal(t, "cf-proxy-edge-netpol", netpol.Name)
	assert.Equal(t, "tenant-a", netpol.Namespace)
	assert.Empty(t, netpol.OwnerReferences, "ownerRefs are the reconciler's job, not the renderer's")
	assert.Equal(t, "cloudflare-tunnel-gateway-proxy", netpol.Spec.PodSelector.MatchLabels["app.kubernetes.io/name"])
	assert.Equal(t, "edge", netpol.Spec.PodSelector.MatchLabels[render.GatewayLabel])

	require.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, netpol.Spec.PolicyTypes,
		"ingress-only — egress must stay open")
	assert.Empty(t, netpol.Spec.Egress)

	require.Len(t, netpol.Spec.Ingress, 1)
	rule := netpol.Spec.Ingress[0]
	require.Len(t, rule.Ports, 1, "only the config-API port is admitted, never the proxy port")
	assert.Equal(t, int32(8081), rule.Ports[0].Port.IntVal)

	require.Len(t, rule.From, 1, "controller namespace only when no monitoring selector")
	assert.Equal(t, "cf-system", rule.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
}

// TestProxyNetworkPolicy_AdmitsMonitoring pins that a monitoring selector adds
// a second allowed source (for /metrics scrape on the shared config-API port).
func TestProxyNetworkPolicy_AdmitsMonitoring(t *testing.T) {
	t.Parallel()

	monitoring := &metav1.LabelSelector{MatchLabels: map[string]string{"team": "observability"}}

	netpol := render.ProxyNetworkPolicy(render.NetworkPolicyInput{
		Gateway:                     testInput("edge").Gateway,
		ControllerNamespace:         "cf-system",
		MonitoringNamespaceSelector: monitoring,
	})

	require.Len(t, netpol.Spec.Ingress[0].From, 2)
	assert.Equal(t, "observability", netpol.Spec.Ingress[0].From[1].NamespaceSelector.MatchLabels["team"])
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

// TestProxyDeployment_WellKnownGatewayLabels pins the GEP-1762 well-known
// labels (gateway.networking.k8s.io/gateway-name,
// .../gateway-class-name — kubernetes-sigs/gateway-api#4705): every rendered
// resource's metadata (and the pod template, which is metadata too) must
// carry both, but the immutable Deployment selector must NOT — adding a key
// there would be a breaking, unrollback-able change to an existing Gateway's
// data plane.
func TestProxyDeployment_WellKnownGatewayLabels(t *testing.T) {
	t.Parallel()

	deployment := render.ProxyDeployment(testInput("edge"))

	assert.Equal(t, "edge", deployment.Labels[string(gatewayv1.GatewayNameLabelKey)])
	assert.Equal(t, "cloudflare-tunnel", deployment.Labels[string(gatewayv1.GatewayClassNameLabelKey)])
	assert.Equal(t, "edge", deployment.Spec.Template.Labels[string(gatewayv1.GatewayNameLabelKey)])
	assert.Equal(t, "cloudflare-tunnel", deployment.Spec.Template.Labels[string(gatewayv1.GatewayClassNameLabelKey)])

	assert.NotContains(t, deployment.Spec.Selector.MatchLabels, string(gatewayv1.GatewayNameLabelKey),
		"the selector is immutable — a well-known label must never be added to it")
	assert.NotContains(t, deployment.Spec.Selector.MatchLabels, string(gatewayv1.GatewayClassNameLabelKey),
		"the selector is immutable — a well-known label must never be added to it")
}

// TestProxyDeployment_WellKnownGatewayLabels_LongNamesTruncate pins that both
// well-known label values go through the same 63-char truncation scheme as
// the controller's own GatewayLabel — Gateway names AND GatewayClassNames are
// DNS-1123 subdomains up to 253 chars, so the raw value would make the
// apiserver reject the whole render on every reconcile.
func TestProxyDeployment_WellKnownGatewayLabels_LongNamesTruncate(t *testing.T) {
	t.Parallel()

	longName := strings.Repeat("very-long-gateway-name-", 4)
	longClass := strings.Repeat("very-long-gateway-class-", 4)

	input := testInput(longName)
	input.Gateway.Spec.GatewayClassName = gatewayv1.ObjectName(longClass)

	deployment := render.ProxyDeployment(input)

	nameValue := deployment.Labels[string(gatewayv1.GatewayNameLabelKey)]
	classValue := deployment.Labels[string(gatewayv1.GatewayClassNameLabelKey)]

	assert.LessOrEqual(t, len(nameValue), 63, "label values cap at 63 characters")
	assert.LessOrEqual(t, len(classValue), 63, "label values cap at 63 characters")
	assert.Equal(t, render.GatewayLabelValue(longName), nameValue)
	assert.Equal(t, render.GatewayLabelValue(longClass), classValue)
}

// TestConfigService_WellKnownGatewayLabels and
// TestProxyNetworkPolicy_WellKnownGatewayLabels pin the same GEP-1762 labels on
// the remaining rendered kinds' metadata, without touching their (also
// immutable-in-effect) selectors.
func TestConfigService_WellKnownGatewayLabels(t *testing.T) {
	t.Parallel()

	service := render.ConfigService(testInput("edge"))

	assert.Equal(t, "edge", service.Labels[string(gatewayv1.GatewayNameLabelKey)])
	assert.Equal(t, "cloudflare-tunnel", service.Labels[string(gatewayv1.GatewayClassNameLabelKey)])
	assert.NotContains(t, service.Spec.Selector, string(gatewayv1.GatewayNameLabelKey),
		"the Service selector must not carry the well-known labels")
}

func TestProxyNetworkPolicy_WellKnownGatewayLabels(t *testing.T) {
	t.Parallel()

	netpol := render.ProxyNetworkPolicy(render.NetworkPolicyInput{
		Gateway:             testInput("edge").Gateway,
		ControllerNamespace: "cf-system",
	})

	assert.Equal(t, "edge", netpol.Labels[string(gatewayv1.GatewayNameLabelKey)])
	assert.Equal(t, "cloudflare-tunnel", netpol.Labels[string(gatewayv1.GatewayClassNameLabelKey)])
	assert.NotContains(t, netpol.Spec.PodSelector.MatchLabels, string(gatewayv1.GatewayNameLabelKey),
		"the NetworkPolicy podSelector must not carry the well-known labels")
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

	// Both set: the CRD CEL forbids this, but on a CEL-disabled cluster the
	// renderer must still let autoscaling win (Replicas nil) rather than pin a
	// fixed count the HPA would immediately fight.
	both := testInput("edge")
	both.Config.Spec.Replicas = &three
	both.Config.Spec.Autoscaling = &v1alpha1.ProxyAutoscaling{MaxReplicas: 5, TargetInflightPerPod: 50}
	withBoth := render.ProxyDeployment(both)
	assert.Nil(t, withBoth.Spec.Replicas,
		"autoscaling must win over an explicit replica count when both are set")
}

// TestProxyResources_ExplicitBlockIsDeepCopied pins that an explicit resources
// block is deep-copied into the rendered container: mutating the rendered
// ResourceList must never write back through to the source GatewayConfig (a
// shallow deref would alias the same inner map).
func TestProxyResources_ExplicitBlockIsDeepCopied(t *testing.T) {
	t.Parallel()

	in := testInput("edge")
	in.Config.Spec.Resources = &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
	}

	rendered := render.ProxyDeployment(in).Spec.Template.Spec.Containers[0].Resources
	rendered.Limits[corev1.ResourceCPU] = resource.MustParse("999m")

	assert.Equal(t, "250m", in.Config.Spec.Resources.Limits.Cpu().String(),
		"the rendered resources must not alias the source GatewayConfig's map")
}

// TestProxyDeployment_RollingUpdateStrategy pins the rollout strategy and the
// apiserver-defaulted fields rendered explicitly. The 25%/25% surge keeps a
// quorum of tunnel connectors registered through a rollout (a connector gap is
// a tunnel-availability hazard); RevisionHistoryLimit and ProgressDeadline are
// rendered to match what the apiserver stores, so the reconciler's full-spec
// apply converges instead of hot-looping (the convergence envtest's pin).
func TestProxyDeployment_RollingUpdateStrategy(t *testing.T) {
	t.Parallel()

	spec := render.ProxyDeployment(testInput("edge")).Spec

	assert.Equal(t, appsv1.RollingUpdateDeploymentStrategyType, spec.Strategy.Type)
	require.NotNil(t, spec.Strategy.RollingUpdate)
	require.NotNil(t, spec.Strategy.RollingUpdate.MaxUnavailable)
	assert.Equal(t, "25%", spec.Strategy.RollingUpdate.MaxUnavailable.String(),
		"a connector gap during rollout is a tunnel-availability hazard")
	require.NotNil(t, spec.Strategy.RollingUpdate.MaxSurge)
	assert.Equal(t, "25%", spec.Strategy.RollingUpdate.MaxSurge.String())

	require.NotNil(t, spec.RevisionHistoryLimit)
	assert.Equal(t, int32(10), *spec.RevisionHistoryLimit)
	require.NotNil(t, spec.ProgressDeadlineSeconds)
	assert.Equal(t, int32(600), *spec.ProgressDeadlineSeconds)
}

// TestProxyDeployment_HardenedSecurityContext pins the per-Gateway proxy's
// security hardening. This data plane runs a TENANT-CHOSEN image (spec.image),
// so the pod- and container-level restrictions are the blast-radius bound, not
// a nicety. Without this test, silently weakening any field — dropping
// runAsNonRoot, flipping readOnlyRootFilesystem, restoring a capability —
// would pass every other render assertion.
func TestProxyDeployment_HardenedSecurityContext(t *testing.T) {
	t.Parallel()

	deployment := render.ProxyDeployment(testInput("edge"))

	podSC := deployment.Spec.Template.Spec.SecurityContext
	require.NotNil(t, podSC, "pod SecurityContext must be set")
	require.NotNil(t, podSC.RunAsNonRoot)
	assert.True(t, *podSC.RunAsNonRoot, "pod must run as non-root")
	require.NotNil(t, podSC.RunAsUser)
	assert.Equal(t, int64(65534), *podSC.RunAsUser, "pod must run as the nobody UID")
	require.NotNil(t, podSC.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, podSC.SeccompProfile.Type,
		"pod must use the RuntimeDefault seccomp profile")

	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
	containerSC := deployment.Spec.Template.Spec.Containers[0].SecurityContext
	require.NotNil(t, containerSC, "container SecurityContext must be set")
	require.NotNil(t, containerSC.AllowPrivilegeEscalation)
	assert.False(t, *containerSC.AllowPrivilegeEscalation, "privilege escalation must be denied")
	require.NotNil(t, containerSC.ReadOnlyRootFilesystem)
	assert.True(t, *containerSC.ReadOnlyRootFilesystem, "the root filesystem must be read-only")
	require.NotNil(t, containerSC.Capabilities)
	assert.Equal(t, []corev1.Capability{"ALL"}, containerSC.Capabilities.Drop,
		"all Linux capabilities must be dropped")
	assert.Empty(t, containerSC.Capabilities.Add, "no capability may be added back")
}

// TestProxyDeployment_DefaultResources pins the chart-parity default
// requests/limits applied when a GatewayConfig sets no resources. A regression
// to an empty block (no requests) would let a tenant's proxy run unbounded on
// a shared node — the requests are the scheduling floor that keeps one tenant
// from starving another.
func TestProxyDeployment_DefaultResources(t *testing.T) {
	t.Parallel()

	in := testInput("edge")
	in.Config.Spec.Resources = nil

	resources := render.ProxyDeployment(in).Spec.Template.Spec.Containers[0].Resources

	assert.Equal(t, "500m", resources.Limits.Cpu().String())
	assert.Equal(t, "512Mi", resources.Limits.Memory().String())
	assert.Equal(t, "100m", resources.Requests.Cpu().String())
	assert.Equal(t, "128Mi", resources.Requests.Memory().String())
}

// TestProxyDeployment_ProbesTargetConfigPort pins two probe invariants the
// CoreShape test does not: every probe targets the config-API port (the only
// port that serves /healthz and /readyz — a probe on the data port would
// always fail), and SuccessThreshold is exactly 1. The apiserver requires
// SuccessThreshold == 1 for liveness/startup probes and rejects 0; a rendered
// 0 would make every reconcile's apply fail validation.
func TestProxyDeployment_ProbesTargetConfigPort(t *testing.T) {
	t.Parallel()

	container := render.ProxyDeployment(testInput("edge")).Spec.Template.Spec.Containers[0]

	for name, probe := range map[string]*corev1.Probe{
		"startup":   container.StartupProbe,
		"liveness":  container.LivenessProbe,
		"readiness": container.ReadinessProbe,
	} {
		require.NotNil(t, probe, "%s probe must be set", name)
		require.NotNil(t, probe.HTTPGet, "%s probe must be an HTTP GET", name)
		assert.Equal(t, "config-api", probe.HTTPGet.Port.StrVal,
			"%s probe must hit the config-API port", name)
		assert.Equal(t, int32(1), probe.SuccessThreshold,
			"%s probe SuccessThreshold must be exactly 1 (apiserver rejects 0)", name)
	}
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

// TestRenderedNames_NoSameKindCollisionAcrossGateways pins the property
// adoption safety actually relies on: SAME-kind rendered names stay distinct
// across distinct Gateways, even for adversarial names that embed another
// builder's suffix. Cross-KIND string aliasing (e.g. DeploymentName("x-config")
// == ConfigServiceName("x")) is possible but benign — adoption is kind-scoped
// and owner-UID-guarded — whereas a same-kind collision would let two tenants'
// Deployments fight over one object.
func TestRenderedNames_NoSameKindCollisionAcrossGateways(t *testing.T) {
	t.Parallel()

	// truncateName has two output regimes — pass-through for already-valid
	// labels and a `<body>-<8hex>` hash form for the rest. If those spaces
	// overlap, a Gateway literally named like another's hashed output collides.
	// longName forces the hash path; collider is that exact rendered output with
	// the builder prefix stripped, so it would pass through unchanged and alias.
	longName := strings.Repeat("g", 80)
	collider := strings.TrimPrefix(render.DeploymentName(testInput(longName).Gateway), "cf-proxy-")

	gatewayNames := []string{
		"edge", "edge-config", "edge-netpol", "edge-auth", "other", "edge-config-config",
		longName, collider,
	}

	builders := map[string]func(string) string{
		"Deployment":    func(n string) string { return render.DeploymentName(testInput(n).Gateway) },
		"ConfigService": func(n string) string { return render.ConfigServiceName(testInput(n).Gateway) },
		"NetworkPolicy": func(n string) string { return render.NetworkPolicyName(testInput(n).Gateway) },
		"AuthSecret":    func(n string) string { return render.GeneratedAuthSecretName(testInput(n).Gateway) },
	}

	for kind, build := range builders {
		seen := make(map[string]string, len(gatewayNames))

		for _, gatewayName := range gatewayNames {
			rendered := build(gatewayName)
			if prev, dup := seen[rendered]; dup {
				t.Fatalf("%s name collision: Gateways %q and %q both render %q", kind, prev, gatewayName, rendered)
			}

			seen[rendered] = gatewayName
		}
	}
}

// TestProxyDeployment_AuthTokenHashAnnotation pins the config-API auth-token
// rotation contract: the pod template carries a hash of the bearer token so a
// rotated authTokenSecretRef rolls the pods. Without it the proxy keeps the
// old PROXY_AUTH_TOKEN (a SecretKeyRef env, read once at start) while the
// controller pushes with the new token — every push 401s until a manual
// rollout. The Secret watch already re-renders on rotation; this annotation is
// what makes that re-render a non-identical pod template.
func TestProxyDeployment_AuthTokenHashAnnotation(t *testing.T) {
	t.Parallel()

	base := testInput("edge")
	base.AuthToken = "auth-token-v1"

	first := render.ProxyDeployment(base)
	hash := first.Spec.Template.Annotations["cf.k8s.lex.la/auth-token-hash"]
	require.NotEmpty(t, hash, "the pod template must hash the config-API auth token")

	rotated := testInput("edge")
	rotated.AuthToken = "auth-token-v2"
	second := render.ProxyDeployment(rotated)
	assert.NotEqual(t, hash, second.Spec.Template.Annotations["cf.k8s.lex.la/auth-token-hash"],
		"a rotated auth token must change the pod template (rolling restart)")

	// The two token hashes are independent: rotating only the auth token must
	// not disturb the connector-token hash, and vice versa.
	assert.Equal(t, first.Spec.Template.Annotations["cf.k8s.lex.la/tunnel-token-hash"],
		second.Spec.Template.Annotations["cf.k8s.lex.la/tunnel-token-hash"],
		"rotating only the auth token must leave the connector-token hash untouched")
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

// TestProxyDeployment_ExplicitEmptyResourcesDropDefaults pins that a non-nil
// empty `resources: {}` is a deliberate opt-out of the chart-parity defaults,
// not a request to re-apply them — the rendered container carries no
// requests/limits.
func TestProxyDeployment_ExplicitEmptyResourcesDropDefaults(t *testing.T) {
	t.Parallel()

	input := testInput("edge")
	input.Config.Spec.Resources = &corev1.ResourceRequirements{}

	container := render.ProxyDeployment(input).Spec.Template.Spec.Containers[0]
	assert.Empty(t, container.Resources.Limits, "explicit empty resources must not re-apply default limits")
	assert.Empty(t, container.Resources.Requests, "explicit empty resources must not re-apply default requests")
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
