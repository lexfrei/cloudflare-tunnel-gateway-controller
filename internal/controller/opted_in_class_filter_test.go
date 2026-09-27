package controller

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// foreignOptedInGateway is a Gateway that asks for a dedicated data plane but
// belongs to a class another controller owns.
func foreignOptedInGateway(namespace string) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-foreign", Namespace: namespace},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "someone-elses-class",
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: &gatewayv1.LocalParametersReference{
					Group: config.ParametersRefGroup, Kind: config.GatewayParametersRefKind, Name: "foreign-config",
				},
			},
		},
	}
}

func requestKeys(requests []reconcile.Request) []string {
	keys := make([]string, 0, len(requests))
	for _, request := range requests {
		keys = append(keys, request.Namespace+"/"+request.Name)
	}

	return keys
}

// TestNamespaceSiblingMappersSkipForeignGateways pins that a namespace-scoped
// event wakes only opted-in Gateways of this controller's classes, on both
// Gateway-typed controllers. A Gateway another controller owns may carry a
// parametersRef too, and reconciling it here is work with no outcome.
func TestNamespaceSiblingMappersSkipForeignGateways(t *testing.T) {
	t.Parallel()

	t.Run("GatewayReconciler", func(t *testing.T) {
		t.Parallel()

		fakeClient := quotaFixtures(t, new(int32(2)))
		require.NoError(t, fakeClient.Create(context.Background(), foreignOptedInGateway("tenant")))

		reconciler := &GatewayReconciler{
			Client:         fakeClient,
			Scheme:         fakeClient.Scheme(),
			ControllerName: "test-controller",
			ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
		}

		keys := requestKeys(reconciler.namespaceDataPlaneSiblings(context.Background(),
			&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw-old", Namespace: "tenant"}}))

		assert.Contains(t, keys, "tenant/gw-new", "a managed opted-in sibling is still woken")
		assert.NotContains(t, keys, "tenant/gw-foreign", "another controller's Gateway is not woken")
	})

	t.Run("GatewayInfraReconciler", func(t *testing.T) {
		t.Parallel()

		reconciler := newInfraReconciler(t, infraQuotaFixtures(t, new(int32(1)))...)
		require.NoError(t, reconciler.Create(context.Background(), foreignOptedInGateway(infraNamespace)))

		keys := requestKeys(reconciler.namespaceInfraGateways(context.Background(),
			&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: infraNamespace}}))

		assert.Contains(t, keys, infraNamespace+"/edge", "a managed opted-in Gateway is still woken")
		assert.NotContains(t, keys, infraNamespace+"/gw-foreign", "another controller's Gateway is not woken")
	})
}

// TestClassConfigWatchesAreGenerationGated pins the wiring of both
// GatewayClassConfig watches to the shared predicate set, the same way
// TestGatewayWatchIsRegisteredOnBothControllers pins the sibling watch: a
// predicate dropped from a Watches block leaves every behavioural test green.
func TestClassConfigWatchesAreGenerationGated(t *testing.T) {
	t.Parallel()

	for file, mapper := range map[string]string{
		"gateway_controller.go":       "mapper.MapConfigToRequests(r.getAllManagedGateways)",
		"gateway_infra_reconciler.go": "r.classConfigInfraGateways",
	} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()

			source, err := os.ReadFile(file)
			require.NoError(t, err)

			packed := strings.Join(strings.Fields(string(source)), "")

			assert.Contains(t, packed,
				"Watches(&v1alpha1.GatewayClassConfig{},handler.EnqueueRequestsFromMapFunc("+mapper+
					"),builder.WithPredicates(classConfigWatchPredicates()...),",
				"%s must gate its GatewayClassConfig watch, or every status write on a class "+
					"config wakes every Gateway it maps to", file)
		})
	}
}
