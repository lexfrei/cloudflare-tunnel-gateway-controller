package controller

import (
	"context"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/listenermerge"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// attachRuleListeners stamps every rule of cfg with the listeners its route
// is attached through, which the proxy needs for listener isolation.
func attachRuleListeners(cfg *proxy.Config, attached routeListeners) {
	for idx := range cfg.Rules {
		if idx >= len(cfg.Provenance) {
			return
		}

		route := types.NamespacedName{Namespace: cfg.Provenance[idx].Namespace, Name: cfg.Provenance[idx].Name}

		if listeners, ok := attached[route]; ok {
			cfg.Rules[idx].Listeners = listeners
		}
	}
}

// gatewayListenerHostnames returns, for every Gateway a rule is attached to,
// the hostnames of its programmed listeners, ListenerSet entries included. A
// Gateway that cannot be read is left out, and the proxy then does not
// isolate rules by that Gateway.
func gatewayListenerHostnames(
	ctx context.Context,
	cli client.Client,
	views *listenerViewCache,
	rules []proxy.RouteRule,
) map[string][]string {
	out := make(map[string][]string)
	unreadable := make(map[string]bool)

	for idx := range rules {
		for key := range rules[idx].Listeners {
			if _, done := out[key]; done || unreadable[key] {
				continue
			}

			hostnames, ok := programmedListenerHostnames(ctx, cli, views, key)
			if !ok {
				unreadable[key] = true

				continue
			}

			out[key] = hostnames
		}
	}

	return out
}

func programmedListenerHostnames(
	ctx context.Context,
	cli client.Client,
	views *listenerViewCache,
	key string,
) ([]string, bool) {
	namespace, name, _ := strings.Cut(key, "/")

	var gateway gatewayv1.Gateway
	if err := cli.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &gateway); err != nil {
		logging.FromContext(ctx).Debug("Gateway unreadable, its listeners are not isolated in this config",
			"gateway", key, "error", err)

		return nil, false
	}

	view, err := views.orNew(cli).forGateway(ctx, &gateway)
	if err != nil {
		// Without the ListenerSet entries an entry's route would lose its hosts
		// to the Gateway's wildcard, so leave the Gateway unisolated instead.
		logging.FromContext(ctx).Debug("ListenerSets unreadable, Gateway's listeners are not isolated in this config",
			"gateway", key, "error", err)

		return nil, false
	}

	var hostnames []string

	for idx := range view.merged.Listeners {
		listener := &view.merged.Listeners[idx]
		value := listenerHostnameOf(listener.Hostname)

		if admitsRoutes(listener) && !slices.Contains(hostnames, value) {
			hostnames = append(hostnames, value)
		}
	}

	return hostnames, true
}

// admitsRoutes reports whether a merged listener is one routes can attach to,
// which is what makes it own its hosts. A conflicted listener, one whose
// protocol has no data plane here and one whose namespace selector does not
// parse are not Accepted and admit no route.
func admitsRoutes(listener *listenermerge.MergedListener) bool {
	return listener.ConflictReason == "" &&
		listenermerge.ServableProtocol(listener.Protocol) &&
		!routebinding.NamespaceSelectorInvalid(listener.AllowedRoutes)
}
