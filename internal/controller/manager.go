package controller

import (
	"context"

	"github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/option"
	"github.com/cockroachdb/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type Config struct {
	AccountID        string
	TunnelID         string
	APIToken         string
	ClusterDomain    string
	GatewayClassName string
	ControllerName   string
	MetricsAddr      string
	HealthAddr       string
}

//nolint:funlen,noinlineerr // controller setup requires multiple steps
func Run(ctx context.Context, cfg *Config) error {
	logger := log.FromContext(ctx).WithName("manager")
	logger.Info("initializing controller manager")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Metrics: server.Options{
			BindAddress: cfg.MetricsAddr,
		},
		HealthProbeBindAddress: cfg.HealthAddr,
	})
	if err != nil {
		return errors.Wrap(err, "failed to create manager")
	}

	if err := gatewayv1.Install(mgr.GetScheme()); err != nil {
		return errors.Wrap(err, "failed to add gateway-api scheme")
	}

	cfClient := cloudflare.NewClient(
		option.WithAPIToken(cfg.APIToken),
	)

	gatewayReconciler := &GatewayReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		GatewayClassName: cfg.GatewayClassName,
		ControllerName:   cfg.ControllerName,
	}

	if err := gatewayReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup gateway controller")
	}

	httpRouteReconciler := &HTTPRouteReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		CFClient:         cfClient,
		AccountID:        cfg.AccountID,
		TunnelID:         cfg.TunnelID,
		ClusterDomain:    cfg.ClusterDomain,
		GatewayClassName: cfg.GatewayClassName,
		ControllerName:   cfg.ControllerName,
	}

	if err := httpRouteReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup httproute controller")
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return errors.Wrap(err, "failed to set up health check")
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return errors.Wrap(err, "failed to set up ready check")
	}

	logger.Info("starting manager")

	if err := mgr.Start(ctx); err != nil {
		return errors.Wrap(err, "failed to start manager")
	}

	return nil
}
