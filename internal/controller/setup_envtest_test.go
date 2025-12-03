//go:build envtest

package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/metrics"
)

func TestGatewayReconciler_SetupWithManager(t *testing.T) {
	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Scheme: envScheme,
		Metrics: server.Options{
			BindAddress: "0", // disable metrics for test
		},
	})
	require.NoError(t, err)

	configResolver := config.NewResolver(envK8sClient, "default", metrics.NewNoopCollector())

	r := &GatewayReconciler{
		Client:           envK8sClient,
		Scheme:           envScheme,
		GatewayClassName: "test-gateway-class",
		ControllerName:   "test-controller",
		ConfigResolver:   configResolver,
		HelmManager:      nil, // not needed for setup test
	}

	err = r.SetupWithManager(mgr)
	require.NoError(t, err)
}

func TestHTTPRouteReconciler_SetupWithManager(t *testing.T) {
	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Scheme: envScheme,
		Metrics: server.Options{
			BindAddress: "0",
		},
	})
	require.NoError(t, err)

	configResolver := config.NewResolver(envK8sClient, "default", metrics.NewNoopCollector())

	routeSyncer := NewRouteSyncer(
		envK8sClient,
		envScheme,
		"cluster.local",
		"test-gateway-class",
		configResolver,
		metrics.NewNoopCollector(),
	)

	r := &HTTPRouteReconciler{
		Client:           envK8sClient,
		Scheme:           envScheme,
		GatewayClassName: "test-gateway-class",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
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

	configResolver := config.NewResolver(envK8sClient, "default", metrics.NewNoopCollector())

	routeSyncer := NewRouteSyncer(
		envK8sClient,
		envScheme,
		"cluster.local",
		"test-gateway-class",
		configResolver,
		metrics.NewNoopCollector(),
	)

	r := &GRPCRouteReconciler{
		Client:           envK8sClient,
		Scheme:           envScheme,
		GatewayClassName: "test-gateway-class",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
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
