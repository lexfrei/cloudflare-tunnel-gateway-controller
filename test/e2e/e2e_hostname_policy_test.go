//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/hostnameownership"
)

// hostnamePolicyMarkerLabel scopes the e2e policy binding so the rest of the
// suite's namespaces are untouched.
const hostnamePolicyMarkerLabel = "cf-e2e-hostname-policy"

// hostnamePolicySuffixLabel must match the chart default labelKey.
const hostnamePolicySuffixLabel = "cf.k8s.lex.la/hostname-suffix"

// TestHostnameOwnershipPolicyEndToEnd applies the CHART-RENDERED hostname
// ownership ValidatingAdmissionPolicy (helm template output, not hand-copied
// YAML — chart-vs-test drift would defeat the point) scoped to marker-labelled
// namespaces, then drives the SHARED semantic vectors through real admission.
// The same vector table is executed against the controller-side enforcement
// layer by the internal/hostnameownership unit tests; running both keeps the
// two implementations of the rule from drifting.
func TestHostnameOwnershipPolicyEndToEnd(t *testing.T) {
	cfg := loadTestConfig(t)
	k8sClient := newK8sClient(t, cfg.KubeContext)
	ctx := context.Background()

	applyHostnamePolicyFromChart(t, k8sClient)

	policedNamespace := func(suffix string) *corev1.Namespace {
		labels := map[string]string{hostnamePolicyMarkerLabel: "enforced"}
		if suffix != "" {
			labels[hostnamePolicySuffixLabel] = suffix
		}

		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-hostname-policy-",
			Labels:       labels,
		}}
	}

	// VAP propagation is eventually consistent: poll a known-bad write in a
	// labelled namespace until the policy starts denying, before running the
	// vector table.
	warmup := policedNamespace("team-a.example.com")
	require.NoError(t, k8sClient.Create(ctx, warmup))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), warmup) })

	waitForHostnamePolicyEnforcement(ctx, t, k8sClient, cfg, warmup.Name)

	for _, vector := range hostnameownership.Vectors() {
		t.Run(vector.Name, func(t *testing.T) {
			namespace := policedNamespace(vector.Suffix)
			require.NoError(t, k8sClient.Create(ctx, namespace))

			//nolint:contextcheck // cleanup runs after the test context may be done
			t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), namespace) })

			route := hostnamePolicyRoute(namespace.Name, cfg, vector.Hostnames)
			err := k8sClient.Create(ctx, route)

			if vector.WantAllowed {
				require.NoError(t, err, "vector %q must be admitted", vector.Name)
				_ = k8sClient.Delete(ctx, route)

				return
			}

			require.Error(t, err, "vector %q must be denied at admission", vector.Name)
			assert.True(t, apierrors.IsInvalid(err) || apierrors.IsForbidden(err),
				"denial must come from admission, got: %v", err)
		})
	}
}

// applyHostnamePolicyFromChart renders the policy + binding via helm template
// with the binding scoped to the e2e marker label, applies both, and removes
// them on cleanup (policy LAST so no Fail-closed window opens).
func applyHostnamePolicyFromChart(t *testing.T, k8sClient client.Client) {
	t.Helper()

	ctx := context.Background()

	helmBin, err := exec.LookPath("helm")
	require.NoError(t, err, "helm is required for the hostname-policy e2e (renders the shipped artifact)")

	selector := fmt.Sprintf(`{"matchLabels":{"%s":"enforced"}}`, hostnamePolicyMarkerLabel)

	out, err := exec.CommandContext(ctx, helmBin, "template", "e2e-hostname",
		"../../charts/cloudflare-tunnel-gateway-controller",
		"--set", "proxy.tunnelTokenSecretRef.name=unused",
		"--set", "hostnameOwnershipPolicy.enabled=true",
		"--set-json", "hostnameOwnershipPolicy.namespaceSelector="+selector,
		"--show-only", "templates/validatingadmissionpolicy-hostname-ownership.yaml",
	).Output()
	require.NoError(t, err, "helm template failed")

	var (
		policy  *admissionregistrationv1.ValidatingAdmissionPolicy
		binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
	)

	for _, doc := range strings.Split(string(out), "\n---") {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}

		if strings.Contains(doc, "kind: ValidatingAdmissionPolicyBinding") {
			binding = &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
			require.NoError(t, yaml.Unmarshal([]byte(doc), binding))

			continue
		}

		if strings.Contains(doc, "kind: ValidatingAdmissionPolicy") {
			policy = &admissionregistrationv1.ValidatingAdmissionPolicy{}
			require.NoError(t, yaml.Unmarshal([]byte(doc), policy))
		}
	}

	require.NotNil(t, policy, "rendered output must contain the policy")
	require.NotNil(t, binding, "rendered output must contain the binding")

	applyObject(ctx, t, k8sClient, policy)
	applyObject(ctx, t, k8sClient, binding)

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_ = k8sClient.Delete(cleanupCtx, binding)
		_ = k8sClient.Delete(cleanupCtx, policy)
	})
}

// waitForHostnamePolicyEnforcement polls a deliberately-violating create
// until the apiserver starts denying it (VAP compilation/propagation lag).
func waitForHostnamePolicyEnforcement(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	cfg testConfig,
	namespace string,
) {
	t.Helper()

	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 90*time.Second, true,
		func(pollCtx context.Context) (bool, error) {
			probe := hostnamePolicyRoute(namespace, cfg, []string{"outside.example.net"})

			createErr := k8sClient.Create(pollCtx, probe)
			if createErr != nil {
				//nolint:nilerr // a denied create IS the success signal: the policy went live
				return true, nil
			}

			_ = k8sClient.Delete(pollCtx, probe)

			return false, nil
		},
	)
	require.NoError(t, err, "hostname-ownership policy never started enforcing")
}

// hostnamePolicyRoute builds a minimal HTTPRoute carrying the vector's
// hostnames. Admission does not resolve parents, so the parentRef just points
// at the shared e2e Gateway.
func hostnamePolicyRoute(namespace string, cfg testConfig, hostnames []string) *gatewayv1.HTTPRoute {
	gatewayNS := gatewayv1.Namespace(cfg.Namespace)

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "hostname-policy-", Namespace: namespace},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(cfg.GatewayName), Namespace: &gatewayNS},
				},
			},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/hostname-policy")}},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						backendRef("echo-v1", 80, nil),
					},
				},
			},
		},
	}

	for _, hostname := range hostnames {
		route.Spec.Hostnames = append(route.Spec.Hostnames, gatewayv1.Hostname(hostname))
	}

	return route
}
