package hostnameownership_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/hostnameownership"
)

// TestNamespaceSelectorParity_MatchesLabelSelectorAsSelector pins the one
// parity axis the shared Vectors() table does not cover: namespace-selector
// scoping. The CEL ValidatingAdmissionPolicy binding consumes the raw
// metav1.LabelSelector (matchResources.namespaceSelector), while the controller
// consumes the kubectl-syntax string the chart's labelSelectorString helper
// renders, parsed by labels.Parse in hostnameownership.New. Those two paths
// must scope the SAME namespaces, or a namespace policed at admission could go
// unpoliced by the controller (or vice versa).
//
// The Helm helper is expected to emit the canonical kubectl form of the
// selector — exactly metav1.LabelSelectorAsSelector(ls).String() — which the
// helm-unittest in deployment_test.yaml pins as a literal string. This test
// owns the Go half: that canonical form, parsed by the controller's real path
// (New -> labels.Parse -> selector.Matches via Evaluate), scopes identically to
// metav1.LabelSelectorAsSelector. The literal string both sides agree on is the
// cross-language bridge.
func TestNamespaceSelectorParity_MatchesLabelSelectorAsSelector(t *testing.T) {
	t.Parallel()

	// A non-trivial selector exercising every matchExpression operator plus a
	// matchLabels term — the axes a naive string flattener gets wrong.
	selectors := []struct {
		name     string
		selector *metav1.LabelSelector
	}{
		{
			name:     "matchLabels only",
			selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tenancy": "enforced"}},
		},
		{
			name: "In",
			selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "team", Operator: metav1.LabelSelectorOpIn, Values: []string{"a", "b"}},
			}},
		},
		{
			name: "NotIn",
			selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "kubernetes.io/metadata.name", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"kube-system"}},
			}},
		},
		{
			name: "Exists",
			selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "tenancy", Operator: metav1.LabelSelectorOpExists},
			}},
		},
		{
			name: "DoesNotExist",
			selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "system", Operator: metav1.LabelSelectorOpDoesNotExist},
			}},
		},
		{
			name: "matchLabels and matchExpressions combined",
			selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"tenancy": "enforced"},
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "kubernetes.io/metadata.name", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"kube-system"}},
				},
			},
		},
	}

	// Candidate namespace label sets, deliberately WITHOUT the ownership label
	// so that "in selector scope" maps cleanly to "policed -> denied" and "out
	// of scope" to "not policed -> allowed", isolating the scoping decision.
	candidates := []labels.Set{
		{},
		{"tenancy": "enforced"},
		{"team": "a"},
		{"team": "c"},
		{"system": "true"},
		{"kubernetes.io/metadata.name": "kube-system"},
		{"kubernetes.io/metadata.name": "team-x", "tenancy": "enforced"},
	}

	for _, tc := range selectors {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			want, err := metav1.LabelSelectorAsSelector(tc.selector)
			require.NoError(t, err)

			policy, err := hostnameownership.New(celTestLabelKey, want.String())
			require.NoError(t, err)

			for _, set := range candidates {
				inScope := want.Matches(set)

				// A namespace without the ownership label: policed namespaces
				// (in scope) fail closed; unpoliced ones (out of scope) pass.
				verdict := policy.Evaluate(map[string]string(set), []gatewayv1.Hostname{"app.example.com"})

				require.Equal(t, inScope, !verdict.Allowed,
					"selector scope must match metav1.LabelSelectorAsSelector for labels %v", set)
			}
		})
	}
}
