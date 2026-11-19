package controller

import (
	"context"
	"sort"

	"github.com/cockroachdb/errors"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

// addLeftoverPlaneWarning registers a one-shot startup check for an
// installation that disabled per-Gateway data planes: a dedicated
// proxy Deployment rendered BEFORE the switch cannot be torn down now — the
// infra reconciler is off and the write RBAC is withdrawn — and its tunnel
// connector keeps serving the plane's last-pushed routes while the owning
// Gateway reports Accepted=False/PerGatewayDataPlanesDisabled. That standing
// contradiction is the operator's to resolve (delete the opted-in Gateways,
// or re-enable the feature and opt them out first), so every leftover is
// reported as an Error log at startup, where turning the toggle off is the
// action that gets reviewed.
//
// Detection is read-only (deployments get/list/watch stays cluster-wide) and
// counts only Deployments controlled by a Gateway whose GatewayClass THIS
// controller manages: the render label alone can be put on anything by anyone
// able to label a Deployment, and a SECOND installation's planes are its own,
// healthy and actively managed — reporting those would tell the operator to
// delete the neighbour's Gateways.
func addLeftoverPlaneWarning(mgr ctrl.Manager, controllerName string) error {
	err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		logger := log.FromContext(ctx).WithName("leftover-planes")

		leftovers, listErr := leftoverDedicatedPlanes(ctx, mgr.GetClient(), controllerName)
		if listErr != nil {
			// Reported, not fatal: failing the whole manager over a warning
			// scan would take the shared plane down with it.
			logger.Error(listErr, "could not check for leftover dedicated data planes")

			return nil
		}

		for _, plane := range leftovers {
			logger.Error(nil, "per-Gateway data planes are disabled but a previously rendered plane still exists; "+
				"it keeps serving its LAST-PUSHED routes and this controller holds no RBAC to remove it — "+
				"delete the opted-in Gateway, or re-enable perGatewayDataPlanes and opt it out first",
				"deployment", plane)
		}

		return nil
	}))

	return errors.Wrap(err, "failed to add leftover-plane warning")
}

// leftoverDedicatedPlanes lists the proxy Deployments a per-Gateway render of
// THIS controller left behind, as "namespace/name (Gateway <owner>)": each is
// controlled by a Gateway that still exists and whose GatewayClass carries
// controllerName. A Deployment whose owning Gateway is already deleted is
// skipped — ownerRef GC collects it, so there is nothing to act on.
func leftoverDedicatedPlanes(ctx context.Context, cli client.Client, controllerName string) ([]string, error) {
	var deployments appsv1.DeploymentList
	if err := cli.List(ctx, &deployments, client.HasLabels{render.GatewayLabel}); err != nil {
		return nil, errors.Wrap(err, "listing rendered proxy Deployments")
	}

	var leftovers []string

	for i := range deployments.Items {
		dep := &deployments.Items[i]
		if !controlledByGateway(dep) {
			continue
		}

		owner := metav1GetControllerName(dep)

		var gateway gatewayv1.Gateway
		if err := cli.Get(ctx, types.NamespacedName{Namespace: dep.Namespace, Name: owner}, &gateway); err != nil {
			if apierrors.IsNotFound(err) {
				continue // Gateway gone: ownerRef GC collects the plane.
			}

			return nil, errors.Wrapf(err, "reading the owning Gateway %s/%s", dep.Namespace, owner)
		}

		managed, err := gatewayIsManaged(ctx, cli, controllerName, &gateway)
		if err != nil {
			return nil, errors.Wrapf(err, "classifying the owning Gateway %s/%s", dep.Namespace, owner)
		}

		if !managed {
			continue // another installation's plane, its own controller's business
		}

		leftovers = append(leftovers, dep.Namespace+"/"+dep.Name+" (Gateway "+owner+")")
	}

	sort.Strings(leftovers)

	return leftovers, nil
}

// metav1GetControllerName names a Deployment's controlling owner; callers
// check controlledByGateway first, so the owner exists.
func metav1GetControllerName(dep *appsv1.Deployment) string {
	for i := range dep.OwnerReferences {
		ref := &dep.OwnerReferences[i]
		if ref.Controller != nil && *ref.Controller {
			return ref.Name
		}
	}

	return "unknown"
}
