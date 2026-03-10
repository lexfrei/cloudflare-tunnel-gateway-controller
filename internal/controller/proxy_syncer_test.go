package controller_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/controller"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func setupProxySyncerScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))

	return scheme
}

func TestProxySyncer_SyncRoutes(t *testing.T) {
	t.Parallel()

	scheme := setupProxySyncerScheme()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	gatewayClassName := gatewayv1.ObjectName("cloudflare-tunnel")
	sectionName := gatewayv1.SectionName("https")

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayClassName,
			Listeners: []gatewayv1.Listener{
				{
					Name:     sectionName,
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
				},
			},
		},
	}

	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name:        "my-gateway",
						SectionName: &sectionName,
					},
				},
			},
			Hostnames: []gatewayv1.Hostname{"example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path: &gatewayv1.HTTPPathMatch{
								Type:  &pathPrefix,
								Value: strPtr("/"),
							},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						makeBackendRef("web-svc", 80, 1),
					},
				},
			},
		},
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-svc",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Port: 80},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gateway, httpRoute, svc).
		WithStatusSubresource(httpRoute).
		Build()

	// Set up a mock config API endpoint that records received configs.
	var receivedConfig proxy.Config
	var pushCount atomic.Int32

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			pushCount.Add(1)

			err := json.NewDecoder(req.Body).Decode(&receivedConfig)
			if err != nil {
				writer.WriteHeader(http.StatusBadRequest)

				return
			}
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	syncer := controller.NewProxySyncer(
		fakeClient,
		scheme,
		"cluster.local",
		string(gatewayClassName),
		slog.Default(),
	)

	endpoints := []string{configServer.URL + "/config"}

	err := syncer.SyncRoutes(context.Background(), endpoints)
	require.NoError(t, err)

	assert.Equal(t, int32(1), pushCount.Load())
	assert.NotEmpty(t, receivedConfig.Rules)
}

func TestProxySyncer_NoRoutes(t *testing.T) {
	t.Parallel()

	scheme := setupProxySyncerScheme()

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	syncer := controller.NewProxySyncer(
		fakeClient,
		scheme,
		"cluster.local",
		"cloudflare-tunnel",
		slog.Default(),
	)

	err := syncer.SyncRoutes(context.Background(), []string{"http://127.0.0.1:1/config"})
	// No routes, but should still push (empty config) — or no error if no endpoints.
	// With unreachable endpoint it errors, but that's expected.
	assert.Error(t, err)
}

// Helper functions.

func strPtr(s string) *string {
	return &s
}

func makeBackendRef(name string, port, weight int) gatewayv1.HTTPBackendRef {
	portNum := gatewayv1.PortNumber(port)
	weightInt := int32(weight)

	return gatewayv1.HTTPBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(name),
				Port: &portNum,
			},
			Weight: &weightInt,
		},
	}
}
