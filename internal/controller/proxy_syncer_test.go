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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/controller"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func TestProxySyncer_SyncRoutes(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

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

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	syncer := controller.NewProxySyncer(
		"cluster.local",
		"",
		testClient,
		slog.Default(),
	)

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web-route", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							makeBackendRef("web-svc", 80, 1),
						},
					},
				},
			},
		},
	}

	endpoints := []string{configServer.URL + "/config"}

	err := syncer.SyncRoutes(context.Background(), endpoints, routes, nil)
	require.NoError(t, err)

	assert.Equal(t, int32(1), pushCount.Load())
	assert.NotEmpty(t, receivedConfig.Rules)
}

func TestProxySyncer_NoRoutes_PushesEmptyConfig(t *testing.T) {
	t.Parallel()

	var receivedConfig proxy.Config

	var pushCount atomic.Int32

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			pushCount.Add(1)

			decodeErr := json.NewDecoder(req.Body).Decode(&receivedConfig)
			if decodeErr != nil {
				writer.WriteHeader(http.StatusBadRequest)

				return
			}
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	syncer := controller.NewProxySyncer(
		"cluster.local",
		"",
		testClient,
		slog.Default(),
	)

	// Zero routes should still push a valid config with empty rules.
	// The proxy will return 404 for all requests until routes are added.
	err := syncer.SyncRoutes(context.Background(), []string{configServer.URL + "/config"}, nil, nil)
	require.NoError(t, err)

	assert.Equal(t, int32(1), pushCount.Load())
	assert.Empty(t, receivedConfig.Rules, "empty routes should produce empty rules")
	assert.True(t, receivedConfig.Version > 0, "version should be positive even with no routes")
}

func TestProxySyncer_SyncRoutes_H2CBackend(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	var receivedConfig proxy.Config

	var pushCount atomic.Int32

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			pushCount.Add(1)

			if decodeErr := json.NewDecoder(req.Body).Decode(&receivedConfig); decodeErr != nil {
				writer.WriteHeader(http.StatusBadRequest)

				return
			}
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	h2cProto := "kubernetes.io/h2c"
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "grpc-svc", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "grpc", Port: 8081, AppProtocol: &h2cProto},
				{Name: "http", Port: 80},
			},
		},
	}

	testClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()

	syncer := controller.NewProxySyncer("cluster.local", "", testClient, slog.Default())

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "grpc-route", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{makeBackendRef("grpc-svc", 8081, 1)},
					},
				},
			},
		},
	}

	err := syncer.SyncRoutes(context.Background(), []string{configServer.URL + "/config"}, routes, nil)
	require.NoError(t, err)

	require.Equal(t, int32(1), pushCount.Load())
	require.Len(t, receivedConfig.Rules, 1)
	require.Len(t, receivedConfig.Rules[0].Backends, 1)
	assert.Equal(t, proxy.BackendProtocolH2C, receivedConfig.Rules[0].Backends[0].Protocol,
		"backend on a Service port with appProtocol kubernetes.io/h2c must be marked h2c")
}

// Helper functions.

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
