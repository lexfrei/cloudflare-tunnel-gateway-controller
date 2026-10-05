// Package render builds the per-Gateway data-plane resources (proxy
// Deployment, config Service, optional HPA) from a Gateway + GatewayConfig
// pair. Pure functions — no client, no status, no ownerRefs (the reconciler
// owns those) — so every rendering decision is unit-testable in isolation.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net"
	"net/url"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// GatewayLabel is the per-Gateway selector label stamped on every rendered
// resource. The proxy endpoint watcher and the config pusher key on it.
const GatewayLabel = "cf.k8s.lex.la/gateway"

// tokenHashAnnotation carries a digest of the connector token on the pod
// template so a token rotation rolls the pods.
//
//nolint:gosec // G101 false positive: this is an annotation KEY, not a credential
const tokenHashAnnotation = "cf.k8s.lex.la/tunnel-token-hash"

// authTokenHashAnnotation carries a digest of the config-API bearer token on
// the pod template so rotating an authTokenSecretRef rolls the pods (the proxy
// reads PROXY_AUTH_TOKEN once at start; without the roll it 401s every push).
//
//nolint:gosec // G101 false positive: this is an annotation KEY, not a credential
const authTokenHashAnnotation = "cf.k8s.lex.la/auth-token-hash"

// Data-plane contract constants. The rendered pods run the proxy binary with
// its built-in defaults; these mirror them and the chart's shared-proxy
// conventions.
// nobodyUID is the chart-parity runAsUser for the proxy pods.
const nobodyUID int64 = 65534

const (
	defaultConfigAPIPort = 8081
	proxyPort            = 8080
	// terminationGracePeriodSeconds = connector drain window (proxy default
	// 30s) + 15s headroom so kubelet never SIGKILLs mid-drain.
	terminationGracePeriodSeconds int64 = 45

	// apiserver-defaulted Deployment/pod fields, rendered explicitly so the
	// reconciler's full-spec apply converges instead of looping (see
	// ProxyDeployment).
	revisionHistoryLimit      int32 = 10
	progressDeadlineSeconds   int32 = 600
	defaultServiceAccountName       = "default"
	defaultSchedulerName            = "default-scheduler"

	containerName      = "proxy"
	configPortName     = "config-api"
	proxyPortName      = "proxy"
	namePrefix         = "cf-proxy-"
	configNameSuffix   = "-config"
	netpolNameSuffix   = "-netpol"
	namespaceNameLabel = "kubernetes.io/metadata.name"
	// MaxDNSLabelLength is the Kubernetes object-name segment / label-value
	// length cap; exported for consumers reasoning about truncated
	// GatewayLabel values.
	MaxDNSLabelLength = 63
	// nameHashLength is the hex width of the disambiguating hash suffix appended
	// to overflowing/sanitized names (truncateName). 8 hex = 32 bits: by the
	// birthday bound a 50% collision needs ~2^16 (~65k) distinct hashed names,
	// and only names that overflow 63 chars or carry non-DNS characters are
	// hashed at all — far above the realistic per-namespace Gateway cardinality.
	// Do NOT widen this: the suffix is part of every rendered object's name, so
	// changing it renames (and so recreates) every per-Gateway Deployment,
	// Service, and NetworkPolicy in place.
	nameHashLength = 8

	tunnelTokenKey = "tunnel-token"
	authTokenKey   = "auth-token"
)

const (
	// configTLSNameSuffix is followed by "-<slot index>".
	configTLSNameSuffix = "-config-tls"
	configTLSVolumeName = "config-tls"
	configTLSMountPath  = "/etc/cf-proxy/config-tls"
	// secretVolumeDefaultMode is the apiserver default, rendered explicitly so
	// the full-spec apply converges.
	secretVolumeDefaultMode int32 = 0o644
)

// Defaults carries the controller-level rendering defaults (Helm-wired).
type Defaults struct {
	// ProxyImage is the image for rendered proxy containers (the controller's
	// --proxy-image flag); GatewayConfig.spec.image overrides per Gateway.
	ProxyImage string
	// TunnelProtocol is the proxy's edge transport (--tunnel-protocol).
	// "auto"/"" is the binary default and is not rendered.
	TunnelProtocol string
	// WSIdleTimeout is the idle bound on an established WebSocket session
	// (--ws-idle-timeout). Empty leaves the proxy binary's own default and is
	// not rendered. The shared plane reads the same chart value from its own
	// env; GatewayConfig has no timeout field, so this is the only route by
	// which the value reaches a per-Gateway plane.
	WSIdleTimeout string
	// MirrorMaxInFlight is the per-filter mirror dispatch limit
	// (--mirror-max-in-flight). Zero or less leaves the proxy binary's own
	// default and is not rendered.
	MirrorMaxInFlight int
	// ConfigAPIPort is the port the proxy serves its config API, probes and
	// metrics on (--proxy-config-api-port). Zero or less means the proxy
	// binary's own 8081, which is rendered as no listen-address env.
	ConfigAPIPort int32
	// AllowXOriginalHost makes the plane trust the client-supplied
	// X-Original-Host header (--proxy-allow-x-original-host). It is an operator
	// decision for test deployments; GatewayConfig has no such field.
	AllowXOriginalHost bool
}

// configAPIPort resolves a configured config-API port, zero or less meaning
// the proxy binary's default.
func configAPIPort(port int32) int32 {
	if port <= 0 {
		return defaultConfigAPIPort
	}

	return port
}

// NetworkPolicyInput carries the controller-level config the per-Gateway
// NetworkPolicy needs beyond the Gateway itself (controller namespace,
// monitoring selector) — a different concern from the per-render Input, so it
// is a separate parameter rather than more fields on Input.
type NetworkPolicyInput struct {
	Gateway *gatewayv1.Gateway
	// ControllerNamespace is the only namespace that legitimately pushes config
	// to a per-Gateway proxy; the policy admits the config-API port from here.
	ControllerNamespace string
	// MonitoringNamespaceSelector, when set, additionally admits the config-API
	// port (which also serves /metrics) from matching namespaces so Prometheus
	// can scrape. Nil = the controller pod only.
	MonitoringNamespaceSelector *metav1.LabelSelector
	// ConfigAPIPort is the config-API port the policy opens, as in
	// Defaults.ConfigAPIPort.
	ConfigAPIPort int32
	// ControllerPodSelector narrows the controller peer from "that namespace"
	// to "that pod". Nil admits the namespace, which is what a controller
	// running ahead of its chart gets. The reverse skew does not reach here: a
	// chart passing the flag to an older image fails on an unknown flag.
	ControllerPodSelector *metav1.LabelSelector
}

// Input is everything a render pass needs.
type Input struct {
	Gateway *gatewayv1.Gateway
	Config  *v1alpha1.GatewayConfig
	// TunnelToken is the raw connector token; only its hash is rendered.
	TunnelToken string
	// AuthToken is the resolved config-API bearer token (BYO override or the
	// controller-generated one); only its hash is rendered, so a rotated
	// authTokenSecretRef rolls the pods instead of stranding them on the old
	// PROXY_AUTH_TOKEN env value.
	AuthToken string
	Defaults  Defaults
	// ConfigTLSSecretName is the controller-issued config API leaf to mount.
	// Empty renders the plaintext config API.
	ConfigTLSSecretName string
}

// The name builders share a prefix and differ by suffix, so an adversarial
// Gateway name CAN make two builders alias across kinds (e.g.
// DeploymentName("x-config") == ConfigServiceName("x")). That is benign: every
// apply path is kind-scoped and adoption is owner-UID-guarded (assertAdoptable),
// so a Service apply never touches a Deployment of the same name. The property
// that matters — same-kind names stay distinct across Gateways — is pinned by
// TestRenderedNames_NoSameKindCollisionAcrossGateways.

// DeploymentName returns the per-Gateway proxy Deployment name.
func DeploymentName(gateway *gatewayv1.Gateway) string {
	return truncateName(namePrefix + gateway.Name)
}

// ConfigServiceName returns the per-Gateway headless config Service name.
func ConfigServiceName(gateway *gatewayv1.Gateway) string {
	return truncateName(namePrefix + gateway.Name + configNameSuffix)
}

// NetworkPolicyName returns the per-Gateway proxy NetworkPolicy name.
func NetworkPolicyName(gateway *gatewayv1.Gateway) string {
	return truncateName(namePrefix + gateway.Name + netpolNameSuffix)
}

// ConfigEndpointURL returns the config-push endpoint for the Gateway's
// rendered data plane, in the same form the chart wires for the shared proxy.
// port is the configured config-API port, as in Defaults.ConfigAPIPort.
func ConfigEndpointURL(gateway *gatewayv1.Gateway, clusterDomain string, port int32) string {
	return fmt.Sprintf("http://%s.%s.svc.%s:%d/config",
		ConfigServiceName(gateway), gateway.Namespace, clusterDomain, configAPIPort(port))
}

// ConfigServerName returns the config Service's DNS name: the name the
// plane's config API certificate carries and the push verifies.
func ConfigServerName(gateway *gatewayv1.Gateway, clusterDomain string) string {
	return fmt.Sprintf("%s.%s.svc.%s", ConfigServiceName(gateway), gateway.Namespace, clusterDomain)
}

// ConfigTLSEndpointURL is ConfigEndpointURL for a plane serving config API
// TLS.
func ConfigTLSEndpointURL(gateway *gatewayv1.Gateway, clusterDomain string, port int32) string {
	endpoint := url.URL{
		Scheme: "https",
		Host:   net.JoinHostPort(ConfigServerName(gateway, clusterDomain), strconv.Itoa(int(configAPIPort(port)))),
		Path:   "/config",
	}

	return endpoint.String()
}

// ConfigTLSSecretName returns the name of a Gateway's config API leaf in slot
// index. A leaf is never rewritten: renewal or a leaf that fails
// verification moves the plane to the next slot.
//
// The index is appended after truncation, so it survives in the name even for
// a Gateway whose name is hashed, and ConfigTLSSecretSlot can read it back.
func ConfigTLSSecretName(gateway *gatewayv1.Gateway, index int) string {
	return configTLSSecretBase(gateway) + "-" + strconv.Itoa(index)
}

// MaxConfigTLSSlot bounds the slots ConfigTLSSecretSlot recognises.
const MaxConfigTLSSlot = 4096

// configTLSSlotWidth is the room kept for "-<index>", index below
// MaxConfigTLSSlot.
const configTLSSlotWidth = len("-4095")

func configTLSSecretBase(gateway *gatewayv1.Gateway) string {
	return truncateNameTo(namePrefix+gateway.Name+configTLSNameSuffix, MaxDNSLabelLength-configTLSSlotWidth)
}

// ConfigTLSSecretSlot reports which of the Gateway's leaf slots a Secret name
// is, if any.
func ConfigTLSSecretSlot(gateway *gatewayv1.Gateway, name string) (int, bool) {
	base, digits, found := strings.CutLast(name, "-")
	if !found {
		return 0, false
	}

	index, err := strconv.Atoi(digits)
	if err != nil || index >= MaxConfigTLSSlot || strconv.Itoa(index) != digits {
		return 0, false
	}

	return index, base == configTLSSecretBase(gateway)
}

// ConfigTLSSecretIndex reports which leaf slot a rendered Deployment mounts.
func ConfigTLSSecretIndex(gateway *gatewayv1.Gateway, deployment *appsv1.Deployment) (int, bool) {
	volumes := deployment.Spec.Template.Spec.Volumes
	for i := range volumes {
		volume := &volumes[i]
		if volume.Name != configTLSVolumeName || volume.Secret == nil {
			continue
		}

		if index, ok := ConfigTLSSecretSlot(gateway, volume.Secret.SecretName); ok {
			return index, true
		}
	}

	return 0, false
}

// GeneratedAuthSecretName returns the name of the controller-generated
// config-API auth Secret for a Gateway whose GatewayConfig declares no
// authTokenSecretRef. The data plane is NEVER rendered without push auth —
// an unauthenticated tenant config API would accept PUT /config from any pod
// that can reach it.
func GeneratedAuthSecretName(gateway *gatewayv1.Gateway) string {
	return truncateName(namePrefix + gateway.Name + "-auth")
}

// GatewayLabelValue returns the GatewayLabel value for a Gateway name. Label
// values cap at 63 characters while Gateway names go up to 253, so long names
// truncate with the same deterministic hash-suffix scheme as resource names —
// an over-long raw value would make the apiserver reject the whole render on
// every reconcile. Consumers mapping a label value back to a Gateway must
// compare through this function, never against raw names.
func GatewayLabelValue(gatewayName string) string {
	return truncateName(gatewayName)
}

// selectorLabels is the immutable Deployment selector / Service selector set.
// Deliberately minimal: selectors cannot be changed after creation.
func selectorLabels(gateway *gatewayv1.Gateway) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name": "cloudflare-tunnel-gateway-proxy",
		GatewayLabel:             GatewayLabelValue(gateway.Name),
	}
}

// ResourceLabels is the full label set for rendered resource metadata:
// infrastructure.labels first (Gateway API SHOULD), controller-owned keys on
// top so a tenant cannot spoof the selector or management markers.
func ResourceLabels(gateway *gatewayv1.Gateway) map[string]string {
	labels := make(map[string]string)

	if gateway.Spec.Infrastructure != nil {
		for key, value := range gateway.Spec.Infrastructure.Labels {
			labels[string(key)] = string(value)
		}
	}

	maps.Copy(labels, selectorLabels(gateway))

	labels["app.kubernetes.io/component"] = "proxy"
	labels["app.kubernetes.io/managed-by"] = "cloudflare-tunnel-gateway-controller"
	// GEP-1762 well-known labels (kubernetes-sigs/gateway-api#4705): metadata
	// only, NEVER the immutable selector (selectorLabels, above, is untouched)
	// — ecosystem tooling that understands the Gateway API convention can
	// discover this per-Gateway data plane without a controller-specific
	// label. Values go through the same truncation as GatewayLabel: Gateway
	// names and GatewayClassNames are both DNS-1123 subdomains up to 253
	// chars, past the 63-char label-value cap.
	labels[gatewayv1.GatewayNameLabelKey] = GatewayLabelValue(gateway.Name)
	labels[gatewayv1.GatewayClassNameLabelKey] = GatewayLabelValue(string(gateway.Spec.GatewayClassName))

	return labels
}

// ResourceAnnotations propagates infrastructure.annotations.
func ResourceAnnotations(gateway *gatewayv1.Gateway) map[string]string {
	if gateway.Spec.Infrastructure == nil || len(gateway.Spec.Infrastructure.Annotations) == 0 {
		return nil
	}

	annotations := make(map[string]string, len(gateway.Spec.Infrastructure.Annotations))
	for key, value := range gateway.Spec.Infrastructure.Annotations {
		annotations[string(key)] = string(value)
	}

	return annotations
}

// ProxyDeployment renders the per-Gateway proxy Deployment. Input is taken by
// pointer: it is at the gocritic hugeParam budget, and both rotation tokens
// live in it.
func ProxyDeployment(input *Input) *appsv1.Deployment {
	podAnnotations := ResourceAnnotations(input.Gateway)
	if podAnnotations == nil {
		podAnnotations = make(map[string]string, 2)
	}

	// Hash BOTH rotation-relevant tokens into the pod template so rotating
	// either Secret produces a different template and rolls the pods. Both
	// reach the proxy via SecretKeyRef env vars (read once at start), so
	// without this a rotated token would strand the running pod on the old
	// value while the controller starts using the new one.
	podAnnotations[tokenHashAnnotation] = hashToken(input.TunnelToken)
	podAnnotations[authTokenHashAnnotation] = hashToken(input.AuthToken)

	return &appsv1.Deployment{
		Name:      DeploymentName(input.Gateway),
		Namespace: input.Gateway.Namespace,
		Labels:    ResourceLabels(input.Gateway),
		// A fresh ResourceAnnotations call (not podAnnotations): the
		// Deployment object's OWN annotations must NOT carry the pod
		// template's rotation-hash entries, and ResourceAnnotations returns
		// an independent map so this cannot alias the mutated podAnnotations.
		Annotations: ResourceAnnotations(input.Gateway),
		Spec: appsv1.DeploymentSpec{
			Replicas: replicaCount(input.Config),
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(input.Gateway)},
			// Render the apiserver-defaulted fields explicitly so the rendered
			// spec equals what the apiserver stores. Without them, the
			// reconciler's full-spec apply re-zeroes the defaulted values every
			// reconcile, so CreateOrUpdate always sees a diff and Updates
			// forever (a hot-loop pinned by the convergence envtest).
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: new(intstr.FromString("25%")),
					MaxSurge:       new(intstr.FromString("25%")),
				},
			},
			RevisionHistoryLimit:    new(revisionHistoryLimit),
			ProgressDeadlineSeconds: new(progressDeadlineSeconds),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      ResourceLabels(input.Gateway),
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: new(terminationGracePeriodSeconds),
					// The proxy never talks to the Kubernetes API, and
					// spec.image is tenant-chosen: do not hand the namespace
					// default ServiceAccount token to an arbitrary image.
					AutomountServiceAccountToken: new(false),
					SecurityContext:              podSecurityContext(),
					Containers:                   []corev1.Container{proxyContainer(input)},
					Volumes:                      configTLSVolumes(input),
					// apiserver-defaulted pod fields, rendered explicitly (see
					// the Strategy comment above).
					RestartPolicy:            corev1.RestartPolicyAlways,
					DNSPolicy:                corev1.DNSClusterFirst,
					SchedulerName:            defaultSchedulerName,
					ServiceAccountName:       defaultServiceAccountName,
					DeprecatedServiceAccount: defaultServiceAccountName,
				},
			},
		},
	}
}

// replicaCount: explicit replicas win; autoscaling leaves the count nil so
// the HPA owns it (a rendered value would fight the autoscaler on every
// reconcile); otherwise the HA default.
func replicaCount(config *v1alpha1.GatewayConfig) *int32 {
	if config.Spec.Autoscaling != nil {
		return nil
	}

	if config.Spec.Replicas != nil {
		return new(*config.Spec.Replicas)
	}

	return new(v1alpha1.DefaultProxyReplicas)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

// proxyContainer renders the single proxy container, mirroring the chart's
// shared-proxy deployment (ports, probes, security context, env contract).
func proxyContainer(input *Input) corev1.Container {
	return corev1.Container{
		Name:            containerName,
		Image:           proxyImage(input),
		ImagePullPolicy: corev1.PullIfNotPresent,
		// apiserver-defaulted container fields, rendered explicitly so the
		// reconciler's full-spec apply converges (see ProxyDeployment).
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		SecurityContext:          containerSecurityContext(),
		Env:                      proxyEnv(input),
		VolumeMounts:             configTLSVolumeMounts(input),
		Ports: []corev1.ContainerPort{
			{Name: configPortName, ContainerPort: configAPIPort(input.Defaults.ConfigAPIPort), Protocol: corev1.ProtocolTCP},
			{Name: proxyPortName, ContainerPort: proxyPort, Protocol: corev1.ProtocolTCP},
		},
		StartupProbe:   httpProbe("/healthz", startupProbeSpec, probeScheme(input)),
		LivenessProbe:  httpProbe("/healthz", livenessProbeSpec, probeScheme(input)),
		ReadinessProbe: httpProbe("/readyz", readinessProbeSpec, probeScheme(input)),
		Resources:      proxyResources(input.Config),
	}
}

// probeScheme follows the config API listener, which also serves the probes.
// kubelet does not verify the certificate of an HTTPS probe.
func probeScheme(input *Input) corev1.URIScheme {
	if input.ConfigTLSSecretName != "" {
		return corev1.URISchemeHTTPS
	}

	return corev1.URISchemeHTTP
}

func configTLSVolumes(input *Input) []corev1.Volume {
	if input.ConfigTLSSecretName == "" {
		return nil
	}

	return []corev1.Volume{{
		Name: configTLSVolumeName,
		Secret: &corev1.SecretVolumeSource{
			SecretName:  input.ConfigTLSSecretName,
			DefaultMode: new(secretVolumeDefaultMode),
		},
	}}
}

func configTLSVolumeMounts(input *Input) []corev1.VolumeMount {
	if input.ConfigTLSSecretName == "" {
		return nil
	}

	return []corev1.VolumeMount{{Name: configTLSVolumeName, MountPath: configTLSMountPath, ReadOnly: true}}
}

func proxyImage(input *Input) string {
	if input.Config.Spec.Image != "" {
		return input.Config.Spec.Image
	}

	return input.Defaults.ProxyImage
}

// proxyEnv wires the connector token (and optional knobs) exactly like the
// chart does for the shared proxy. Binary defaults (the proxy port, an
// unchanged config-API port, grace period, metrics-on) are deliberately not
// rendered.
func proxyEnv(input *Input) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{
			Name: "TUNNEL_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					Name: input.Config.Spec.TunnelTokenSecretRef.Name,
					Key:  input.Config.Spec.TunnelTokenSecretRef.KeyOr(tunnelTokenKey),
				},
			},
		},
	}

	// Push auth is ALWAYS wired — fail secure: without an explicit ref the
	// controller-generated Secret protects the config API (an unauthenticated
	// tenant plane would accept PUT /config from any pod that reaches it).
	authName, authKey := GeneratedAuthSecretName(input.Gateway), authTokenKey
	if ref := input.Config.Spec.AuthTokenSecretRef; ref != nil {
		authName, authKey = ref.Name, ref.KeyOr(authTokenKey)
	}

	env = append(env, corev1.EnvVar{
		Name: "PROXY_AUTH_TOKEN",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				Name: authName,
				Key:  authKey,
			},
		},
	})

	if protocol := input.Defaults.TunnelProtocol; protocol != "" && protocol != "auto" {
		env = append(env, corev1.EnvVar{Name: "PROXY_TUNNEL_PROTOCOL", Value: protocol})
	}

	if idle := input.Defaults.WSIdleTimeout; idle != "" {
		env = append(env, corev1.EnvVar{Name: "PROXY_WS_IDLE_TIMEOUT", Value: idle})
	}

	if limit := input.Defaults.MirrorMaxInFlight; limit > 0 {
		env = append(env, corev1.EnvVar{Name: "PROXY_MIRROR_MAX_IN_FLIGHT", Value: strconv.Itoa(limit)})
	}

	if input.Defaults.AllowXOriginalHost {
		env = append(env, corev1.EnvVar{Name: "PROXY_ALLOW_X_ORIGINAL_HOST", Value: "true"})
	}

	if port := configAPIPort(input.Defaults.ConfigAPIPort); port != defaultConfigAPIPort {
		env = append(env, corev1.EnvVar{Name: "PROXY_CONFIG_ADDR", Value: ":" + strconv.Itoa(int(port))})
	}

	if input.ConfigTLSSecretName != "" {
		env = append(env,
			corev1.EnvVar{Name: "PROXY_CONFIG_TLS_CERT_FILE", Value: configTLSMountPath + "/" + corev1.TLSCertKey},
			corev1.EnvVar{Name: "PROXY_CONFIG_TLS_KEY_FILE", Value: configTLSMountPath + "/" + corev1.TLSPrivateKeyKey})
	}

	return env
}

func proxyResources(config *v1alpha1.GatewayConfig) corev1.ResourceRequirements {
	if config.Spec.Resources != nil {
		// An explicit value wins — INCLUDING a non-nil empty {}: `resources: {}`
		// is a deliberate opt-out of the chart-parity defaults (no
		// requests/limits), not a request to re-apply them. DeepCopy so the
		// rendered object never aliases the inner ResourceList maps of the
		// source GatewayConfig.
		return *config.Spec.Resources.DeepCopy()
	}

	// Chart parity: the shared proxy's default requests/limits.
	return corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
}

func podSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   new(true),
		RunAsUser:      new(nobodyUID),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func containerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		ReadOnlyRootFilesystem:   new(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// probeSpec bundles probe timings (chart parity with the shared proxy).
type probeSpec struct {
	initialDelay, period, timeout, failureThreshold int32
}

// Probe timings mirror the chart's shared-proxy defaults verbatim (the
// startup budget of 30×5s=150s exists because connector registration against
// the Cloudflare edge can be slow on cold starts). The field names in the
// struct literals ARE the documentation; hoisting twelve one-use numeric
// constants would only obscure the table.
//
//nolint:gochecknoglobals,mnd // chart-parity timing table, field-named literals
var (
	startupProbeSpec   = probeSpec{initialDelay: 0, period: 5, timeout: 3, failureThreshold: 30}
	livenessProbeSpec  = probeSpec{initialDelay: 15, period: 20, timeout: 5, failureThreshold: 3}
	readinessProbeSpec = probeSpec{initialDelay: 5, period: 10, timeout: 3, failureThreshold: 3}
)

func httpProbe(path string, spec probeSpec, scheme corev1.URIScheme) *corev1.Probe {
	return &corev1.Probe{
		HTTPGet: &corev1.HTTPGetAction{
			Path:   path,
			Port:   intstr.FromString(configPortName),
			Scheme: scheme,
		},
		InitialDelaySeconds: spec.initialDelay,
		PeriodSeconds:       spec.period,
		TimeoutSeconds:      spec.timeout,
		FailureThreshold:    spec.failureThreshold,
		// apiserver defaults SuccessThreshold to 1 (and requires exactly 1 for
		// liveness/startup); render it so the apply converges.
		SuccessThreshold: 1,
	}
}

// ConfigService renders the per-Gateway headless config Service. Pod IPs are
// published before readiness because the controller pushes config to
// not-yet-ready pods — their readiness depends on that very config.
func ConfigService(input *Input) *corev1.Service {
	return &corev1.Service{
		Name:        ConfigServiceName(input.Gateway),
		Namespace:   input.Gateway.Namespace,
		Labels:      ResourceLabels(input.Gateway),
		Annotations: ResourceAnnotations(input.Gateway),
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeClusterIP,
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 selectorLabels(input.Gateway),
			Ports: []corev1.ServicePort{
				{
					Name:       configPortName,
					Port:       configAPIPort(input.Defaults.ConfigAPIPort),
					TargetPort: intstr.FromString(configPortName),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// ProxyNetworkPolicy renders an ingress-only NetworkPolicy locking the
// per-Gateway proxy's config-API port to the config pusher — the
// controller pod where the chart supplies its selector, its namespace
// otherwise — plus an optional monitoring selector (for /metrics scrape).
// The proxy port (8080) takes NO in-cluster ingress — traffic arrives through
// the outbound tunnel, not an inbound connection. Egress is left open (the
// proxy must reach arbitrary backends and the Cloudflare edge). PolicyTypes is
// set explicitly so the apiserver never infers it and drifts the apply.
func ProxyNetworkPolicy(input NetworkPolicyInput) *networkingv1.NetworkPolicy {
	protocolTCP := corev1.ProtocolTCP
	configPort := intstr.FromInt32(configAPIPort(input.ConfigAPIPort))

	// Both selectors in ONE peer are AND'd: the controller pod in the controller
	// namespace. Split across two peers they would be OR'd, which would admit
	// every pod in that namespace and every pod carrying those labels anywhere.
	controllerPeer := networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{namespaceNameLabel: input.ControllerNamespace},
		},
		PodSelector: input.ControllerPodSelector.DeepCopy(),
	}

	from := []networkingv1.NetworkPolicyPeer{controllerPeer}
	if input.MonitoringNamespaceSelector != nil {
		from = append(from, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: input.MonitoringNamespaceSelector.DeepCopy(),
		})
	}

	return &networkingv1.NetworkPolicy{
		Name:        NetworkPolicyName(input.Gateway),
		Namespace:   input.Gateway.Namespace,
		Labels:      ResourceLabels(input.Gateway),
		Annotations: ResourceAnnotations(input.Gateway),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: selectorLabels(input.Gateway)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From:  from,
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocolTCP, Port: &configPort}},
				},
			},
		},
	}
}

// truncateName turns a Gateway name into a valid rendered resource name. It
// bounds the result to the 63-char DNS label limit AND sanitizes characters
// that are legal in a Gateway name (a DNS-1123 SUBDOMAIN, dots allowed, up to
// 253 chars) but illegal in a Service/label name (a DNS-1123 LABEL, no dots).
// Whenever the name overflows OR sanitization changed it, a deterministic
// hash of the ORIGINAL name replaces the tail so distinct inputs (e.g.
// "my.edge" vs "my-edge") never collide on the same rendered name.
func truncateName(name string) string {
	return truncateNameTo(name, MaxDNSLabelLength)
}

// truncateNameTo is truncateName with a tighter length cap, for a name that
// gets a suffix of its own appended afterwards.
func truncateNameTo(name string, maxLength int) string {
	sanitized := sanitizeDNSLabel(name)

	// Pass through only a name that is ALREADY a valid rendered name: no
	// sanitization needed, within the length cap, and starting+ending on an
	// alphanumeric (a leading/trailing dash is a valid char but an invalid
	// DNS-1123 label). A name already shaped like the hash path's
	// `-<8hex>` tail must NOT pass through — a different long name could hash to
	// exactly it, aliasing two Gateways onto one rendered name — so it is forced
	// down the hash path, keeping the pass-through and hash output spaces
	// disjoint. Anything else takes the hash path, which trims the edges. Real
	// Gateway names (DNS-1123 subdomains) always pass through.
	if sanitized == name && len(name) <= maxLength && isAlphanumericEdged(name) && !hasHashSuffixShape(name) {
		return name
	}

	sum := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(sum[:])[:nameHashLength]
	keep := min(len(sanitized), maxLength-nameHashLength-1)

	// Trim dashes from BOTH ends: a leading dash (e.g. ".edge" → "-edge")
	// would make the result an invalid DNS-1123 label. The hash suffix is
	// always alphanumeric, so the tail stays valid; an all-dash body trims to
	// empty and the name becomes just the hash.
	body := strings.Trim(sanitized[:keep], "-")
	if body == "" {
		return suffix
	}

	return body + "-" + suffix
}

// hasHashSuffixShape reports whether name already ends in the "-<8 lowercase
// hex>" tail the hash path produces. Such names must take the hash path rather
// than pass through, so a pass-through result can never equal a hashed result
// (the two output spaces stay disjoint, preventing cross-Gateway aliasing).
func hasHashSuffixShape(name string) bool {
	if len(name) < nameHashLength+1 || name[len(name)-nameHashLength-1] != '-' {
		return false
	}

	for _, char := range name[len(name)-nameHashLength:] {
		isHex := (char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')
		if !isHex {
			return false
		}
	}

	return true
}

// sanitizeDNSLabel lowercases and replaces every character that is not a
// lowercase alphanumeric with a dash, yielding a DNS-1123-label-safe body
// (the caller guarantees length and uniqueness via the hash suffix).
func sanitizeDNSLabel(name string) string {
	var builder strings.Builder

	builder.Grow(len(name))

	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			builder.WriteRune(r)

			continue
		}

		builder.WriteByte('-')
	}

	return builder.String()
}

// isAlphanumericEdged reports whether name is non-empty and both its first and
// last bytes are lowercase ASCII alphanumerics. A DNS-1123 label must start and
// end on an alphanumeric, so a name with a leading or trailing dash (e.g.
// "-edge", "edge-") is rejected here and routed to the hash path, which trims
// the edges. Callers use this only on the early-return path where name already
// equals its lowercased sanitized form, so checking bytes is sufficient.
func isAlphanumericEdged(name string) bool {
	if name == "" {
		return false
	}

	isAlnum := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
	}

	return isAlnum(name[0]) && isAlnum(name[len(name)-1])
}
