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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
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

			route := hostnamePolicyVectorObject(t, namespace.Name, cfg, vector.Hostnames)
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

	// The policy matches grpcroutes too — pin that resource kind through real
	// admission as well (one denied, one allowed), or a matchConstraints typo
	// would silently exempt gRPC tenants.
	t.Run("grpcroute foreign hostname denied", func(t *testing.T) {
		namespace := policedNamespace("team-a.example.com")
		require.NoError(t, k8sClient.Create(ctx, namespace))

		//nolint:contextcheck // cleanup runs after the test context may be done
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), namespace) })

		route := hostnamePolicyGRPCRoute(namespace.Name, cfg, "app.team-b.example.com")
		err := k8sClient.Create(ctx, route)
		require.Error(t, err, "a GRPCRoute with a foreign hostname must be denied")
		assert.True(t, apierrors.IsInvalid(err) || apierrors.IsForbidden(err),
			"denial must come from admission, got: %v", err)
	})

	t.Run("grpcroute owned hostname admitted", func(t *testing.T) {
		namespace := policedNamespace("team-a.example.com")
		require.NoError(t, k8sClient.Create(ctx, namespace))

		//nolint:contextcheck // cleanup runs after the test context may be done
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), namespace) })

		route := hostnamePolicyGRPCRoute(namespace.Name, cfg, "app.team-a.example.com")
		require.NoError(t, k8sClient.Create(ctx, route), "an owned-hostname GRPCRoute must be admitted")
		_ = k8sClient.Delete(ctx, route)
	})
}

// hostnamePolicyGRPCRoute builds a minimal GRPCRoute for the admission-layer
// checks (admission does not resolve parents or backends).
func hostnamePolicyGRPCRoute(namespace string, cfg testConfig, hostname string) *gatewayv1.GRPCRoute {
	gatewayNS := gatewayv1.Namespace(cfg.Namespace)
	port := gatewayv1.PortNumber(8080)

	return &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "hostname-policy-grpc-", Namespace: namespace},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(cfg.GatewayName), Namespace: &gatewayNS},
				},
			},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(hostname)},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "echo-v1", Port: &port,
							},
						}},
					},
				},
			},
		},
	}
}

// TestHostnameOwnershipRelabelReconverges drives the CONTROLLER-side
// enforcement layer end to end through a namespace relabel — the path
// admission cannot cover (the VAP only gates route writes; relabelling the
// namespace rewrites nothing on the route). Revoking the namespace's allowed
// suffix must flip an already-programmed route to
// Accepted=False/HostnameNotPermitted, and granting it back must clear the
// rejection. Requires the controller deployed with hostnameOwnershipPolicy
// enabled and scoped to the e2e marker label (conformance-setup.sh does
// this).
func TestHostnameOwnershipRelabelReconverges(t *testing.T) {
	cfg := loadTestConfig(t)
	k8sClient := newK8sClient(t, cfg.KubeContext)
	ctx := context.Background()

	requireControllerOwnershipEnforcement(ctx, t, k8sClient, cfg)

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: "e2e-hostname-relabel-",
		Labels: map[string]string{
			hostnamePolicyMarkerLabel: "enforced",
			hostnamePolicySuffixLabel: "team-a.example.com",
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, namespace))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), namespace) })

	route := hostnamePolicyRoute(namespace.Name, cfg, []string{"app.team-a.example.com"})
	require.NoError(t, k8sClient.Create(ctx, route), "an owned hostname must program normally")
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), route) })

	waitForRouteAcceptedReason(t, k8sClient, route, metav1.ConditionTrue, "",
		"the owned hostname must be accepted before the revocation")

	// Revoke: rebind the namespace to a different suffix. The programmed
	// route now violates ownership and the controller must reject it without
	// any route write happening.
	relabelNamespace(ctx, t, k8sClient, namespace.Name, "team-b.example.com")
	waitForRouteAcceptedReason(t, k8sClient, route, metav1.ConditionFalse, "HostnameNotPermitted",
		"revoking the namespace suffix must reject the programmed route")

	// Grant back: the rejection must clear the same way.
	relabelNamespace(ctx, t, k8sClient, namespace.Name, "team-a.example.com")
	waitForRouteAcceptedReason(t, k8sClient, route, metav1.ConditionTrue, "",
		"restoring the namespace suffix must re-accept the route")
}

// requireControllerOwnershipEnforcement fails fast (loudly, not a skip) when
// the deployed controller does not carry the ownership flags — a silent skip
// would dilute the pre-merge gate the moment someone redeploys without the
// policy.
func requireControllerOwnershipEnforcement(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	cfg testConfig,
) {
	t.Helper()

	var deployments appsv1.DeploymentList
	require.NoError(t, k8sClient.List(ctx, &deployments, client.InNamespace(cfg.Namespace)))

	for i := range deployments.Items {
		for _, container := range deployments.Items[i].Spec.Template.Spec.Containers {
			for _, arg := range container.Args {
				if strings.Contains(arg, "--hostname-ownership-enforce") {
					return
				}
			}
		}
	}

	t.Fatalf("controller in %s is not deployed with --hostname-ownership-enforce; "+
		"redeploy via hack/conformance-setup.sh (it enables hostnameOwnershipPolicy scoped to the e2e marker)",
		cfg.Namespace)
}

// relabelNamespace updates the namespace's allowed-suffix label with
// conflict retry (the e2e cluster is shared).
func relabelNamespace(ctx context.Context, t *testing.T, k8sClient client.Client, name, suffix string) {
	t.Helper()

	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var namespace corev1.Namespace

		err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &namespace)
		if err != nil {
			return fmt.Errorf("get namespace %s: %w", name, err)
		}

		namespace.Labels[hostnamePolicySuffixLabel] = suffix

		return k8sClient.Update(ctx, &namespace)
	}))
}

// waitForRouteAcceptedReason polls the route's Accepted condition on the e2e
// Gateway parent until it reaches the wanted status (and reason, when given).
func waitForRouteAcceptedReason(
	t *testing.T,
	k8sClient client.Client,
	route *gatewayv1.HTTPRoute,
	wantStatus metav1.ConditionStatus,
	wantReason string,
	explanation string,
) {
	t.Helper()

	ctx := context.Background()

	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 90*time.Second, true,
		func(pollCtx context.Context) (bool, error) {
			var current gatewayv1.HTTPRoute

			getErr := k8sClient.Get(pollCtx,
				types.NamespacedName{Namespace: route.Namespace, Name: route.Name}, &current)
			if getErr != nil {
				return false, nil //nolint:nilerr // transient read failures just re-poll
			}

			for _, parent := range current.Status.Parents {
				for _, cond := range parent.Conditions {
					if cond.Type != string(gatewayv1.RouteConditionAccepted) || cond.Status != wantStatus {
						continue
					}

					if wantReason == "" || cond.Reason == wantReason {
						return true, nil
					}
				}
			}

			return false, nil
		},
	)
	require.NoError(t, err, explanation)
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
				// Only an ADMISSION denial proves the policy went live; a
				// transient apiserver error must keep polling or the vector
				// table runs before enforcement starts and flakes.
				return apierrors.IsInvalid(createErr) || apierrors.IsForbidden(createErr), nil
			}

			_ = k8sClient.Delete(pollCtx, probe)

			return false, nil
		},
	)
	require.NoError(t, err, "hostname-ownership policy never started enforcing")
}

// hostnamePolicyVectorObject builds the create payload for one vector. The
// nil-vs-empty hostnames distinction must survive to the wire: typed clients
// drop an empty slice through omitempty, so a present-but-empty vector
// (`hostnames: []` — the CEL has() trap) is sent as unstructured with the
// field explicitly set.
func hostnamePolicyVectorObject(
	t *testing.T,
	namespace string,
	cfg testConfig,
	hostnames []string,
) client.Object {
	t.Helper()

	route := hostnamePolicyRoute(namespace, cfg, hostnames)
	if hostnames == nil || len(hostnames) > 0 {
		return route
	}

	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(route)
	require.NoError(t, err)

	raw := &unstructured.Unstructured{Object: content}
	raw.SetAPIVersion(gatewayv1.GroupVersion.String())
	raw.SetKind("HTTPRoute")
	require.NoError(t, unstructured.SetNestedSlice(raw.Object, []any{}, "spec", "hostnames"))

	return raw
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
