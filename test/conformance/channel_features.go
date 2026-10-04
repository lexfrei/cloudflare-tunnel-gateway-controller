//go:build conformance

package conformance

import (
	"context"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/gateway-api/pkg/consts"
	"sigs.k8s.io/gateway-api/pkg/features"
)

const httpRouteCRDName = "httproutes.gateway.networking.k8s.io"

// channelFeatures returns the features that are claimed only when the
// cluster runs the Experimental-channel CRDs. rules[].retry exists only
// there: the Standard CRD prunes it, so the retry route would be served
// without a policy. The two siblings have no conformance test in this suite
// version; the proxy retries a per-attempt backendRequest timeout and a
// connection error whenever a retry stanza is set, which is what each feature
// names, except for a request body it cannot resend (docs/gateway-api/
// limitations.md, Retries).
func channelFeatures(ctx context.Context, cl client.Client) ([]features.FeatureName, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}

	err := cl.Get(ctx, client.ObjectKey{Name: httpRouteCRDName}, crd)
	if err != nil {
		return nil, fmt.Errorf("reading the %s CRD: %w", httpRouteCRDName, err)
	}

	if crd.Annotations[consts.ChannelAnnotation] != "experimental" {
		return nil, nil
	}

	return []features.FeatureName{
		features.SupportHTTPRouteRetry,
		features.SupportHTTPRouteRetryBackendTimeout,
		features.SupportHTTPRouteRetryConnectionError,
	}, nil
}
