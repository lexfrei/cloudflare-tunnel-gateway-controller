package routebinding

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func frontendValidationGateway(frontend *gatewayv1.FrontendTLSConfig) *gatewayv1.Gateway {
	fromAll := gatewayv1.NamespacesFromAll

	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{{
				Name:          "http",
				Protocol:      gatewayv1.HTTPProtocolType,
				Port:          80,
				AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: &fromAll}},
			}},
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &fromAll},
			},
			TLS: &gatewayv1.GatewayTLSConfig{Frontend: frontend},
		},
	}
}

func clientCertValidation() *gatewayv1.FrontendTLSConfig {
	return &gatewayv1.FrontendTLSConfig{
		Default: gatewayv1.TLSConfig{
			Validation: &gatewayv1.FrontendTLSValidation{
				CACertificateRefs: []gatewayv1.ObjectReference{{Kind: "ConfigMap", Name: "ca"}},
			},
		},
	}
}

func TestRequestsFrontendValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		gateway *gatewayv1.Gateway
		want    bool
	}{
		{name: "no tls block", gateway: &gatewayv1.Gateway{}, want: false},
		{name: "backend tls only", gateway: frontendValidationGateway(nil), want: false},
		{name: "default validation", gateway: frontendValidationGateway(clientCertValidation()), want: true},
		{
			name: "per-port validation only",
			gateway: frontendValidationGateway(&gatewayv1.FrontendTLSConfig{
				PerPort: []gatewayv1.TLSPortConfig{{
					Port: 443,
					TLS: gatewayv1.TLSConfig{Validation: &gatewayv1.FrontendTLSValidation{
						CACertificateRefs: []gatewayv1.ObjectReference{{Kind: "ConfigMap", Name: "ca"}},
					}},
				}},
			}),
			want: true,
		},
		{name: "empty frontend block", gateway: frontendValidationGateway(&gatewayv1.FrontendTLSConfig{}), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, RequestsFrontendValidation(tt.gateway))
		})
	}
}

func TestValidateBinding_RefusesGatewayRequestingFrontendValidation(t *testing.T) {
	t.Parallel()

	result, err := NewValidator(setupFakeClient()).ValidateBinding(context.Background(),
		frontendValidationGateway(clientCertValidation()),
		&RouteInfo{Name: "r", Namespace: "app", Kind: "HTTPRoute"})
	require.NoError(t, err)

	assert.False(t, result.Accepted)
	assert.Empty(t, result.MatchedListeners)
	assert.Equal(t, gatewayv1.RouteReasonNoMatchingParent, result.Reason)
	assert.Equal(t, ParentRequestsFrontendValidationMessage, result.Message)
}

func TestEvaluateListenerSetAcceptance_RefusesParentRequestingFrontendValidation(t *testing.T) {
	t.Parallel()

	listenerSet := &gatewayv1.ListenerSet{ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"}}

	result, err := NewValidator(setupFakeClient()).EvaluateListenerSetAcceptance(context.Background(),
		frontendValidationGateway(clientCertValidation()), listenerSet)
	require.NoError(t, err)

	assert.False(t, result.Accepted)
	assert.Equal(t, gatewayv1.ListenerSetReasonParentNotAccepted, result.Reason)
	assert.Equal(t, ParentRequestsFrontendValidationMessage, result.Message)
}
