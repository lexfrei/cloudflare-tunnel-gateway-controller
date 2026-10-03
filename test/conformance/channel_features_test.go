//go:build conformance

package conformance

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/gateway-api/pkg/consts"
	"sigs.k8s.io/gateway-api/pkg/features"
)

// TestChannelFeatures pins that the retry features are claimed only against
// Experimental-channel CRDs: the Standard HTTPRoute CRD prunes rules[].retry,
// so a run against it would serve the retry route without a policy and fail.
func TestChannelFeatures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		channel string
		want    []features.FeatureName
	}{
		{
			name:    "experimental",
			channel: "experimental",
			want: []features.FeatureName{
				features.SupportHTTPRouteRetry,
				features.SupportHTTPRouteRetryBackendTimeout,
				features.SupportHTTPRouteRetryConnectionError,
			},
		},
		{name: "standard", channel: "standard", want: nil},
		{name: "no channel annotation", channel: "", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			scheme := runtime.NewScheme()
			require.NoError(t, apiextensionsv1.AddToScheme(scheme))

			crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{
				Name: "httproutes.gateway.networking.k8s.io",
			}}
			if tt.channel != "" {
				crd.Annotations = map[string]string{consts.ChannelAnnotation: tt.channel}
			}

			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(crd).Build()

			got, err := channelFeatures(t.Context(), cl)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("missing CRD is an error", func(t *testing.T) {
		t.Parallel()

		scheme := runtime.NewScheme()
		require.NoError(t, apiextensionsv1.AddToScheme(scheme))

		_, err := channelFeatures(t.Context(), fake.NewClientBuilder().WithScheme(scheme).Build())
		require.Error(t, err)
	})
}
