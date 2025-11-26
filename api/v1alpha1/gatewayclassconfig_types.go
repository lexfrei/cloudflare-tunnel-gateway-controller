package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretReference is a reference to a Kubernetes Secret.
type SecretReference struct {
	// Name of the Secret.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the Secret. Defaults to the namespace of the referencing resource.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Key in the Secret. Defaults depend on context:
	// - For cloudflareCredentialsSecretRef: "api-token"
	// - For tunnelTokenSecretRef: "tunnel-token"
	// +optional
	Key string `json:"key,omitempty"`
}

// AWGConfig configures the AmneziaWG sidecar for cloudflared.
type AWGConfig struct {
	// SecretName is the name of the Secret containing AWG configuration.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	SecretName string `json:"secretName"`

	// InterfaceName is the AWG network interface name.
	// Must be unique per node if running multiple instances.
	// +optional
	// +kubebuilder:default="awg0"
	InterfaceName string `json:"interfaceName,omitempty"`
}

// CloudflaredConfig configures the cloudflared deployment managed by the controller.
type CloudflaredConfig struct {
	// Enabled controls whether the controller manages cloudflared deployment.
	// When true, the controller will deploy cloudflared via Helm.
	// +optional
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`

	// Replicas is the number of cloudflared pods to run.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas,omitempty"`

	// Namespace is the namespace for cloudflared deployment.
	// +optional
	// +kubebuilder:default="cloudflare-tunnel-system"
	Namespace string `json:"namespace,omitempty"`

	// Protocol is the transport protocol for cloudflared.
	// +optional
	// +kubebuilder:validation:Enum=auto;quic;http2;""
	Protocol string `json:"protocol,omitempty"`

	// AWG configures the AmneziaWG sidecar.
	// +optional
	AWG *AWGConfig `json:"awg,omitempty"`
}

// GatewayClassConfigSpec defines the desired state of GatewayClassConfig.
type GatewayClassConfigSpec struct {
	// CloudflareCredentialsSecretRef references a Secret containing Cloudflare API credentials.
	// The Secret must contain an "api-token" key with a valid Cloudflare API token.
	// Optionally, it can contain an "account-id" key; if not present, account ID is auto-detected.
	// +kubebuilder:validation:Required
	CloudflareCredentialsSecretRef SecretReference `json:"cloudflareCredentialsSecretRef"`

	// TunnelID is the Cloudflare Tunnel UUID.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`
	TunnelID string `json:"tunnelID"`

	// TunnelTokenSecretRef references a Secret containing the tunnel token.
	// Required when cloudflared.enabled is true.
	// The Secret must contain a "tunnel-token" key.
	// +optional
	TunnelTokenSecretRef *SecretReference `json:"tunnelTokenSecretRef,omitempty"`

	// Cloudflared configures the cloudflared deployment.
	// +optional
	Cloudflared CloudflaredConfig `json:"cloudflared,omitempty"`
}

// GatewayClassConfigStatus defines the observed state of GatewayClassConfig.
type GatewayClassConfigStatus struct {
	// Conditions describe the current state of the GatewayClassConfig.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=gcconfig
// +kubebuilder:printcolumn:name="Tunnel ID",type=string,JSONPath=`.spec.tunnelID`
// +kubebuilder:printcolumn:name="Managed",type=boolean,JSONPath=`.spec.cloudflared.enabled`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GatewayClassConfig is the Schema for the gatewayclassconfigs API.
// It provides configuration for Cloudflare Tunnel Gateway API implementation.
type GatewayClassConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GatewayClassConfigSpec   `json:"spec,omitempty"`
	Status GatewayClassConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GatewayClassConfigList contains a list of GatewayClassConfig.
type GatewayClassConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GatewayClassConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GatewayClassConfig{}, &GatewayClassConfigList{})
}

// IsCloudflaredEnabled returns whether cloudflared management is enabled.
// Defaults to true if not explicitly set.
func (c *GatewayClassConfigSpec) IsCloudflaredEnabled() bool {
	if c.Cloudflared.Enabled == nil {
		return true
	}
	return *c.Cloudflared.Enabled
}

// GetCloudflaredReplicas returns the replica count, defaulting to 1.
func (c *GatewayClassConfigSpec) GetCloudflaredReplicas() int32 {
	if c.Cloudflared.Replicas == 0 {
		return 1
	}
	return c.Cloudflared.Replicas
}

// GetCloudflaredNamespace returns the namespace, defaulting to "cloudflare-tunnel-system".
func (c *GatewayClassConfigSpec) GetCloudflaredNamespace() string {
	if c.Cloudflared.Namespace == "" {
		return "cloudflare-tunnel-system"
	}
	return c.Cloudflared.Namespace
}

// GetAPITokenKey returns the key for API token in the secret.
func (r *SecretReference) GetAPITokenKey() string {
	if r.Key == "" {
		return "api-token"
	}
	return r.Key
}

// GetTunnelTokenKey returns the key for tunnel token in the secret.
func (r *SecretReference) GetTunnelTokenKey() string {
	if r.Key == "" {
		return "tunnel-token"
	}
	return r.Key
}
