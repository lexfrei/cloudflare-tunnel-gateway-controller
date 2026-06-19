//go:build envtest

package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

// TestGatewayReconciler_SetupWithManager registers BOTH Gateway-typed
// reconcilers on one manager, exactly like production manager.go does. This
// pins the controller-name uniqueness contract: controller-runtime derives
// the controller name from the For type (and validates it in a
// process-global registry), so GatewayReconciler owns the implicit "gateway"
// name and GatewayInfraReconciler MUST carry an explicit Named() — without
// it the second registration fails and the binary never starts. The two
// registrations deliberately live in ONE test: the name registry is global
// to the test binary, so a second test registering "gateway" would conflict
// with this one.
func TestGatewayReconciler_SetupWithManager(t *testing.T) {
	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Scheme: envScheme,
		Metrics: server.Options{
			BindAddress: "0", // disable metrics for test
		},
	})
	require.NoError(t, err)

	configResolver := config.NewResolver(envK8sClient, "default", cfmetrics.NewNoopCollector())

	r := &GatewayReconciler{
		Client:         envK8sClient,
		Scheme:         envScheme,
		ControllerName: "test-controller",
		ConfigResolver: configResolver,
	}

	err = r.SetupWithManager(mgr)
	require.NoError(t, err)

	infraReconciler := &GatewayInfraReconciler{
		Client:              envK8sClient,
		Scheme:              envScheme,
		ControllerName:      "test-controller",
		ConfigResolver:      configResolver,
		RenderDefaults:      render.Defaults{ProxyImage: "example.com/proxy:test"},
		RenderNetworkPolicy: true,
	}
	require.NoError(t, infraReconciler.SetupWithManager(mgr),
		"both Gateway-typed reconcilers must register on one manager — production startup does exactly this")
}

func TestHTTPRouteReconciler_SetupWithManager(t *testing.T) {
	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Scheme: envScheme,
		Metrics: server.Options{
			BindAddress: "0",
		},
	})
	require.NoError(t, err)

	configResolver := config.NewResolver(envK8sClient, "default", cfmetrics.NewNoopCollector())

	routeSyncer := NewRouteSyncer(
		envK8sClient,
		envScheme,
		"cluster.local",
		"test-controller",
		configResolver,
		cfmetrics.NewNoopCollector(),
		nil,
	)

	r := &HTTPRouteReconciler{
		Client:         envK8sClient,
		Scheme:         envScheme,
		ControllerName: "test-controller",
		RouteSyncer:    routeSyncer,
	}

	err = r.SetupWithManager(mgr)
	require.NoError(t, err)
}

func TestGRPCRouteReconciler_SetupWithManager(t *testing.T) {
	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Scheme: envScheme,
		Metrics: server.Options{
			BindAddress: "0",
		},
	})
	require.NoError(t, err)

	configResolver := config.NewResolver(envK8sClient, "default", cfmetrics.NewNoopCollector())

	routeSyncer := NewRouteSyncer(
		envK8sClient,
		envScheme,
		"cluster.local",
		"test-controller",
		configResolver,
		cfmetrics.NewNoopCollector(),
		nil,
	)

	r := &GRPCRouteReconciler{
		Client:         envK8sClient,
		Scheme:         envScheme,
		ControllerName: "test-controller",
		RouteSyncer:    routeSyncer,
	}

	err = r.SetupWithManager(mgr)
	require.NoError(t, err)
}

func TestGatewayClassConfigReconciler_SetupWithManager(t *testing.T) {
	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Scheme: envScheme,
		Metrics: server.Options{
			BindAddress: "0",
		},
	})
	require.NoError(t, err)

	r := &GatewayClassConfigReconciler{
		Client:           envK8sClient,
		Scheme:           envScheme,
		DefaultNamespace: "default",
	}

	err = r.SetupWithManager(mgr)
	require.NoError(t, err)
}
