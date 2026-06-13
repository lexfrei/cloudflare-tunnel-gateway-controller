package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultProxyReplicas is the per-Gateway proxy replica count when neither
// spec.replicas nor spec.autoscaling is set. Two connectors is the HA floor:
// a single pod restarting drops every tunnel connection for its Gateway.
const DefaultProxyReplicas int32 = 2

// DefaultInFlightMetricName is the Pods-type custom metric the rendered HPA
// scales on by default — the proxy data plane's in-flight request gauge.
const DefaultInFlightMetricName = "cftunnel_proxy_requests_in_flight"

// LocalSecretReference references a Secret in the SAME namespace as the
// referencing resource — deliberately no Namespace field. A GatewayConfig is
// reached through Gateway.spec.infrastructure.parametersRef, which the
// Gateway API defines as namespace-local; the credentials it carries follow
// the same boundary so one tenant's Gateway cannot point at another tenant's
// Secrets.
type LocalSecretReference struct {
	// Name of the Secret.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key in the Secret. Defaults depend on context: "tunnel-token" for
	// tunnelTokenSecretRef, "auth-token" for authTokenSecretRef.
	// +optional
	Key string `json:"key,omitempty"`
}

// KeyOr returns the explicit Key or the given per-context fallback.
func (r *LocalSecretReference) KeyOr(fallback string) string {
	if r.Key == "" {
		return fallback
	}

	return r.Key
}

// ProxyAutoscaling configures a HorizontalPodAutoscaler for the per-Gateway
// proxy Deployment, scaling on the proxy's in-flight request gauge (an
// I/O-bound L7 hop saturates on concurrency, not CPU). Serving the metric to
// the HPA requires a metrics adapter (prometheus-adapter or KEDA) exposing it
// through the custom-metrics API; without one the HPA reports
// FailedGetPodsMetric and holds minReplicas.
type ProxyAutoscaling struct {
	// MinReplicas is the lower bound. Defaults to 2 — the HA floor for
	// tunnel connectors.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the upper bound. Capped at 100: replica counts are
	// tenant-controlled input on a shared cluster, and an unbounded value is
	// a noisy-neighbour attack. Aggregate resource usage still needs a
	// per-namespace ResourceQuota — the cap bounds one Gateway, not a tenant.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxReplicas int32 `json:"maxReplicas"`

	// TargetInflightPerPod is the average in-flight request count per pod the
	// HPA aims for.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	TargetInflightPerPod int32 `json:"targetInflightPerPod"`

	// MetricName overrides the Pods-type custom metric the HPA consumes.
	// Defaults to the proxy's in-flight gauge.
	// +optional
	MetricName string `json:"metricName,omitempty"`
}

// EffectiveMinReplicas returns MinReplicas or the HA default.
func (a *ProxyAutoscaling) EffectiveMinReplicas() int32 {
	if a.MinReplicas == nil {
		return DefaultProxyReplicas
	}

	return *a.MinReplicas
}

// EffectiveMetricName returns MetricName or the default in-flight gauge.
func (a *ProxyAutoscaling) EffectiveMetricName() string {
	if a.MetricName == "" {
		return DefaultInFlightMetricName
	}

	return a.MetricName
}

// GatewayConfigSpec configures a DEDICATED data plane for one Gateway: its
// own proxy Deployment and its own Cloudflare Tunnel, instead of the shared
// chart-deployed proxy pool serving every Gateway of the class. Referenced
// from Gateway.spec.infrastructure.parametersRef
// (group=cf.k8s.lex.la, kind=GatewayConfig, same namespace). A Gateway
// without a parametersRef keeps the shared data plane unchanged.
//
// The tunnel identity (tunnel ID and account) is PARSED from the connector
// token — there is deliberately no separate tunnelID field, which would
// invite token/ID mismatch bugs.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.replicas) && has(self.autoscaling))",message="replicas and autoscaling are mutually exclusive: the HPA owns the replica count when autoscaling is set"
type GatewayConfigSpec struct {
	// TunnelTokenSecretRef references the Secret (same namespace) holding the
	// Cloudflare Tunnel connector token under the "tunnel-token" key (or Key).
	// The token determines the tunnel this Gateway's data plane serves.
	// +kubebuilder:validation:Required
	TunnelTokenSecretRef LocalSecretReference `json:"tunnelTokenSecretRef"`

	// CloudflareCredentialsSecretRef optionally overrides the API credentials
	// used to write this Gateway's tunnel ingress document, from a Secret in
	// the SAME namespace under the "api-token" key (or Key). Defaults to the
	// credentials resolved from the Gateway's GatewayClass → GatewayClassConfig
	// (class defaults, Gateway overrides). Namespace-local like every other
	// reference here — a cross-namespace option would let a tenant point the
	// controller at another tenant's credentials.
	// +optional
	CloudflareCredentialsSecretRef *LocalSecretReference `json:"cloudflareCredentialsSecretRef,omitempty"`

	// AuthTokenSecretRef optionally references a Secret (same namespace) with
	// a bearer token (key "auth-token" or Key) protecting this Gateway's proxy
	// config API; the controller authenticates its config pushes with it.
	// +optional
	AuthTokenSecretRef *LocalSecretReference `json:"authTokenSecretRef,omitempty"`

	// Replicas is the fixed proxy replica count. Defaults to 2 (the HA floor
	// for tunnel connectors). Mutually exclusive with autoscaling. Capped at
	// 100 — see Autoscaling.MaxReplicas for the rationale.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Replicas *int32 `json:"replicas,omitempty"`

	// Autoscaling renders a HorizontalPodAutoscaler for the proxy Deployment
	// instead of a fixed replica count.
	// +optional
	// +kubebuilder:validation:XValidation:rule="has(self.minReplicas) ? self.maxReplicas >= self.minReplicas : self.maxReplicas >= 2",message="maxReplicas must be >= minReplicas (minReplicas defaults to 2 when unset)"
	Autoscaling *ProxyAutoscaling `json:"autoscaling,omitempty"`

	// Resources sets the proxy container's resource requests and limits.
	// Defaults to the controller's built-in proxy defaults when unset.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Image overrides the proxy container image for this Gateway. Defaults to
	// the controller's --proxy-image flag (set by the Helm chart to the
	// release's proxy image).
	// +optional
	Image string `json:"image,omitempty"`
}

// GatewayConfigStatus defines the observed state of GatewayConfig. Reserved:
// the controller does not write it today (config problems surface on the
// REFERENCING Gateway as Accepted=False/InvalidParameters, the actionable
// place), and its RBAC deliberately grants no status write. The subresource
// exists so adding conditions later is not a breaking CRD change.
type GatewayConfigStatus struct {
	// Conditions describe the current state of the GatewayConfig.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=gwconfig
// +kubebuilder:printcolumn:name="Tunnel Token Secret",type=string,JSONPath=`.spec.tunnelTokenSecretRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GatewayConfig is the Schema for the gatewayconfigs API: the per-Gateway
// data-plane configuration referenced by
// Gateway.spec.infrastructure.parametersRef. Its presence opts the Gateway
// into hard data-plane isolation — a dedicated proxy Deployment and a
// dedicated Cloudflare Tunnel.
type GatewayConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GatewayConfigSpec   `json:"spec,omitempty"`
	Status GatewayConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GatewayConfigList contains a list of GatewayConfig.
type GatewayConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []GatewayConfig `json:"items"`
}
