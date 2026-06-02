package v1alpha1

import (
	"fmt"
	"strings"

	"github.com/cockroachdb/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	errInvalidScheme  = errors.New("scheme must be http or https")
	errEmptyHost      = errors.New("host must not be empty")
	errPortOutOfRange = errors.New("port out of range [1,65535]")
	errPathPrefix     = errors.New("path must begin with \"/\"")
)

// ExternalBackendScheme is the wire protocol used to reach an external backend.
// +kubebuilder:validation:Enum=http;https
type ExternalBackendScheme string

const (
	// ExternalBackendSchemeHTTP reaches the backend over plaintext HTTP.
	ExternalBackendSchemeHTTP ExternalBackendScheme = "http"
	// ExternalBackendSchemeHTTPS reaches the backend over TLS.
	ExternalBackendSchemeHTTPS ExternalBackendScheme = "https"
)

// ExternalBackendSpec defines an out-of-cluster HTTP(S) endpoint that an
// HTTPRoute or GRPCRoute may target as a backendRef. It exists because the
// in-process L7 proxy ultimately just dials a URL, so a route can point at an
// arbitrary external origin without a Service standing in for it. Unlike a
// Service of type ExternalName (which only carries a DNS name and infers the
// scheme from the port), this type makes the scheme explicit and lets the host
// be an address that is not a valid Service name.
type ExternalBackendSpec struct {
	// Scheme is the protocol used to dial the backend: "http" or "https".
	// +kubebuilder:validation:Required
	Scheme ExternalBackendScheme `json:"scheme"`

	// Host is the backend hostname or IP address (no scheme, port, or path).
	// IPv6 literals must be bracketed (e.g. "[2001:db8::1]"). The controller
	// performs a final URL parse; this pattern only rejects obvious mistakes
	// such as embedding a scheme or path.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._:\[\]-]+$`
	Host string `json:"host"`

	// Port is the backend TCP port.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Path is an optional base path prepended to the request path when the
	// proxy dials the backend. Must begin with "/".
	// +optional
	// +kubebuilder:validation:Pattern=`^/.*$`
	Path string `json:"path,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=extbackend
// +kubebuilder:printcolumn:name="Scheme",type=string,JSONPath=`.spec.scheme`
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.host`
// +kubebuilder:printcolumn:name="Port",type=integer,JSONPath=`.spec.port`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ExternalBackend is the Schema for the externalbackends API. It is a
// namespaced, spec-only resource (no status): a route referencing a missing or
// malformed ExternalBackend surfaces the failure on the route's own
// ResolvedRefs condition, mirroring how an unresolvable Service backendRef is
// reported.
type ExternalBackend struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ExternalBackendSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ExternalBackendList contains a list of ExternalBackend.
type ExternalBackendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ExternalBackend `json:"items"`
}

// URL renders the backend's dialable URL: scheme://host:port followed by the
// optional base path. The host is emitted verbatim, so a bracketed IPv6 literal
// stays bracketed.
func (b *ExternalBackendSpec) URL() string {
	url := fmt.Sprintf("%s://%s:%d", b.Scheme, b.Host, b.Port)
	if b.Path != "" {
		url += b.Path
	}

	return url
}

// Validate reports whether the spec is internally consistent beyond what the
// CRD schema enforces at admission. It is defensive: the kube-apiserver already
// rejects an empty scheme/host or an out-of-range port, but the controller may
// see an object created before a schema tightening, so it re-checks here.
func (b *ExternalBackendSpec) Validate() error {
	switch b.Scheme {
	case ExternalBackendSchemeHTTP, ExternalBackendSchemeHTTPS:
	default:
		return errors.Wrapf(errInvalidScheme, "got %q", b.Scheme)
	}

	if b.Host == "" {
		return errEmptyHost
	}

	if b.Port < 1 || b.Port > 65535 {
		return errors.Wrapf(errPortOutOfRange, "got %d", b.Port)
	}

	if b.Path != "" && !strings.HasPrefix(b.Path, "/") {
		return errors.Wrapf(errPathPrefix, "got %q", b.Path)
	}

	return nil
}
