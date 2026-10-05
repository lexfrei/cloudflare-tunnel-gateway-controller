package ingress_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
)

var errGrantListFailed = errors.New("simulated ReferenceGrant List failure")

// TestBuild_UnlistableGrantIsUndecided pins that a cross-namespace backendRef
// whose ReferenceGrants cannot be listed is reported undecided, not
// RefNotPermitted: a read error is not a denial.
func TestBuild_UnlistableGrantIsUndecided(t *testing.T) {
	t.Parallel()

	cli := unlistableGrantClient()
	builder := ingress.NewBuilder("cluster.local", referencegrant.NewValidator(cli), cli, nil, nil)

	result := builder.Build(context.Background(), crossNamespaceRoutes())

	require.Len(t, result.FailedRefs, 1)
	assert.True(t, result.FailedRefs[0].Undecided)
	assert.Empty(t, result.FailedRefs[0].Reason)
	assert.Equal(t, "r", result.FailedRefs[0].RouteName)
}

// TestBuild_PersistentGrantReadErrorLogsOnce pins that a grant read failure
// that persists across syncs is logged at error once, not on every retry.
func TestBuild_PersistentGrantReadErrorLogsOnce(t *testing.T) {
	t.Parallel()

	cli := unlistableGrantClient()
	logger, logs := logging.TestLogger(t)
	builder := ingress.NewBuilder("cluster.local", referencegrant.NewValidator(cli), cli, nil, logger)

	repeats := logging.NewRepeats()
	ctx := logging.WithRepeats(context.Background(), repeats)

	for range 2 {
		repeats.NextPass()
		builder.Build(ctx, crossNamespaceRoutes())
	}

	assert.Equal(t, 1, strings.Count(logs.String(), `"level":"ERROR"`), logs.String())
}

func unlistableGrantClient() client.Client {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1beta1.Install(scheme))

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "backend"}}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cli client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*gatewayv1beta1.ReferenceGrantList); ok {
					return errGrantListFailed
				}

				return cli.List(ctx, list, opts...)
			},
		}).Build()
}

func crossNamespaceRoutes() []gatewayv1.HTTPRoute {
	ns := gatewayv1.Namespace("backend")

	return []gatewayv1.HTTPRoute{{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"app.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{newHTTPBackendRef("api", &ns, int32Ptr(80))},
			}},
		},
	}}
}
