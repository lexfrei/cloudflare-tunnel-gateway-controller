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
	// +optional
	Key string `json:"key,omitempty"`
}

// GatewayClassConfigSpec defines the desired state of GatewayClassConfig.
type GatewayClassConfigSpec struct {
	// CloudflareCredentialsSecretRef references a Secret containing Cloudflare API credentials.
	// The Secret must contain an "api-token" key with a valid Cloudflare API token.
	// +kubebuilder:validation:Required
	CloudflareCredentialsSecretRef SecretReference `json:"cloudflareCredentialsSecretRef"`

	// AccountID is the Cloudflare account ID. Optional - if not specified, it will be
	// read from the credentials secret ("account-id" key) or auto-detected if the API token
	// has access to only one account.
	//
	// When set, must be a 32-character lowercase hexadecimal string -- the format
	// Cloudflare uses for account IDs. Validated server-side via a CRD-level CEL
	// rule (Kubernetes >= 1.25) so an invalid value is rejected at admission time,
	// before the controller has to reconcile it. Empty string passes through
	// because the field is optional.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == '' || self.matches('^[a-f0-9]{32}$')",message="accountID must be a 32-character lowercase hexadecimal string (Cloudflare account ID format)"
	AccountID string `json:"accountId,omitempty"`

	// TunnelID is the Cloudflare Tunnel UUID.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`
	TunnelID string `json:"tunnelID"` //nolint:tagliatelle // Cloudflare API uses tunnelID
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

	Items []GatewayClassConfig `json:"items"`
}

// GetAPITokenKey returns the key for API token in the secret.
func (r *SecretReference) GetAPITokenKey() string {
	if r.Key == "" {
		return "api-token"
	}

	return r.Key
}
