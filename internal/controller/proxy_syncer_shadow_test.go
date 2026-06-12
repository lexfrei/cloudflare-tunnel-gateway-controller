package controller_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/controller"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestProxySyncer_SyncRoutes_ShadowedRouteDiagnostic pins the end-to-end #474
// wiring: two HTTPRoutes claiming the identical (hostname, match) pair through
// a full SyncRoutes pass yield a Shadowed diagnostic attributed to the
// lower-precedence (younger) route, naming the winner.
func TestProxySyncer_SyncRoutes_ShadowedRouteDiagnostic(t *testing.T) {
	t.Parallel()

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	syncer := controller.NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	pathPrefix := gatewayv1.PathMatchPathPrefix
	makeRoute := func(namespace, name string, created time.Time) *gatewayv1.HTTPRoute {
		return &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: namespace,
				CreationTimestamp: metav1.NewTime(created),
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.team-a.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{makeBackendRef("svc", 80, 1)},
					},
				},
			},
		}
	}

	owner := makeRoute("team-a", "app", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	intruder := makeRoute("team-b", "capture", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))

	diagnostics, err := syncer.SyncRoutes(
		context.Background(),
		[]string{configServer.URL + "/config"},
		[]*gatewayv1.HTTPRoute{intruder, owner}, nil, nil, nil,
	)
	require.NoError(t, err)

	var shadowed []proxy.RouteDiagnostic

	for _, diag := range diagnostics {
		if diag.Target == proxy.DiagnosticShadowed {
			shadowed = append(shadowed, diag)
		}
	}

	require.Len(t, shadowed, 1, "exactly the intruder's pair must be flagged")
	assert.Equal(t, "team-b", shadowed[0].Namespace)
	assert.Equal(t, "capture", shadowed[0].Name)
	assert.Contains(t, shadowed[0].Message, "HTTPRoute team-a/app")
}
