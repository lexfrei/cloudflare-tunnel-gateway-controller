package proxy

import (
	"fmt"
	"net/url"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// ExternalBackendGroup is the API group of the cf.k8s.lex.la ExternalBackend CRD.
	ExternalBackendGroup = "cf.k8s.lex.la"
	// ExternalBackendKind is the Kind of an ExternalBackend backendRef.
	ExternalBackendKind = "ExternalBackend"

	// externalBackendScheme is the URL scheme of the placeholder (sentinel) URL
	// the converter emits for an ExternalBackend backendRef. The converter has no
	// Kubernetes client, so it cannot read the ExternalBackend's spec; it instead
	// encodes the ref's namespace/name into this sentinel, which the controller
	// rewrites to the real scheme://host:port/path before the config is pushed
	// (see resolveExternalBackends). A sentinel that somehow survives to the
	// proxy is never dialed because the same step marks an unresolvable backend
	// 500.
	externalBackendScheme = "externalbackend"
)

// IsExternalBackendRef reports whether the ref targets a cf.k8s.lex.la
// ExternalBackend. Like ServiceImport, the group has no implicit default and
// must be set explicitly.
func IsExternalBackendRef(ref gatewayv1.BackendObjectReference) bool {
	return ref.Group != nil && string(*ref.Group) == ExternalBackendGroup &&
		ref.Kind != nil && string(*ref.Kind) == ExternalBackendKind
}

// ExternalBackendSentinelURL builds the placeholder URL that encodes an
// ExternalBackend's namespace and name. The controller resolves it to the real
// backend URL before pushing the config.
func ExternalBackendSentinelURL(namespace, name string) string {
	return fmt.Sprintf("%s://%s?ns=%s", externalBackendScheme, name, url.QueryEscape(namespace))
}

// ParseExternalBackendSentinel extracts the ExternalBackend namespace and name
// from a sentinel URL. The bool is false for any URL that is not an
// ExternalBackend sentinel, so callers can cheaply skip ordinary backend URLs.
func ParseExternalBackendSentinel(rawURL string) (string, string, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != externalBackendScheme {
		return "", "", false
	}

	return parsed.Query().Get("ns"), parsed.Host, true
}
