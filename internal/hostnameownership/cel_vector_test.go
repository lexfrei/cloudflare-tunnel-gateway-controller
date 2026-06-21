package hostnameownership_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/hostnameownership"
)

// celTestLabelKey is substituted for the chart's templated labelKey when the
// rendered CEL is extracted, and is the key the test namespace objects carry.
const celTestLabelKey = "cf.k8s.lex.la/hostname-suffix"

// vapTemplatePath is the rendered admission-policy template relative to this
// package.
var vapTemplatePath = filepath.Join("..", "..", "charts", "cloudflare-tunnel-gateway-controller",
	"templates", "validatingadmissionpolicy-hostname-ownership.yaml")

// TestCELPolicyMatchesVectors closes the drift hole the two-layer design rests
// on: the controller-side Policy.Evaluate runs in `go test`, but the CEL
// ValidatingAdmissionPolicy only runs in the maintainer-only e2e against a live
// apiserver. This compiles and EVALUATES the rendered CEL expressions against
// the SAME shared Vectors() the Go layer uses, so a CEL-side typo — dropping
// the "." subdomain boundary, or substring(2)->substring(1) on the wildcard
// strip — fails here in CI instead of silently only in e2e.
func TestCELPolicyMatchesVectors(t *testing.T) {
	t.Parallel()

	suffixExpr, validations := extractCELExpressions(t)

	env := newCELTestEnv(t)

	suffixPrg := compileCEL(t, env, suffixExpr)

	validationPrgs := make([]cel.Program, len(validations))
	for i, expr := range validations {
		validationPrgs[i] = compileCEL(t, env, expr)
	}

	for _, vector := range hostnameownership.Vectors() {
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()

			namespaceObject := buildNamespaceObject(vector.Suffix)

			suffixOut, _, err := suffixPrg.Eval(map[string]any{"namespaceObject": namespaceObject})
			require.NoError(t, err)

			suffixVal, ok := suffixOut.Value().(string)
			require.True(t, ok, "the suffix variable must evaluate to a string")

			input := map[string]any{
				"object":          buildRouteObject(vector.Hostnames),
				"namespaceObject": namespaceObject,
				"variables":       map[string]any{"suffix": suffixVal},
			}

			allowed := true

			for _, prg := range validationPrgs {
				out, _, err := prg.Eval(input)
				require.NoError(t, err)

				pass, ok := out.Value().(bool)
				require.True(t, ok, "each validation must evaluate to a bool")

				if !pass {
					allowed = false
				}
			}

			require.Equal(t, vector.WantAllowed, allowed,
				"the rendered CEL must reach the SAME allow/deny verdict as the Go Policy.Evaluate")
		})
	}
}

func compileCEL(t *testing.T, env *cel.Env, expr string) cel.Program {
	t.Helper()

	ast, issues := env.Compile(expr)
	require.NoError(t, issues.Err(), "rendered CEL must compile: %s", expr)

	prg, err := env.Program(ast)
	require.NoError(t, err)

	return prg
}

// buildNamespaceObject models a policed namespace: an empty suffix means the
// ownership label is absent (labels present but without the key), matching the
// Go layer's nsLabels[key]=="" treatment.
func buildNamespaceObject(suffix string) map[string]any {
	labels := map[string]any{}
	if suffix != "" {
		labels[celTestLabelKey] = suffix
	}

	return map[string]any{"metadata": map[string]any{"labels": labels}}
}

// buildRouteObject models the route: nil hostnames omit the field entirely
// (has() false), an empty slice is a present-but-empty list (has() true,
// size 0), mirroring the nil-vs-empty wire distinction the vectors pin.
func buildRouteObject(hostnames []string) map[string]any {
	spec := map[string]any{}

	if hostnames != nil {
		list := make([]any, len(hostnames))
		for i, hostname := range hostnames {
			list[i] = hostname
		}

		spec["hostnames"] = list
	}

	return map[string]any{"spec": spec}
}

// newCELTestEnv builds the CEL environment the rendered policy expressions are
// compiled against: the same variables and string extensions the apiserver
// exposes to a ValidatingAdmissionPolicy.
func newCELTestEnv(t *testing.T) *cel.Env {
	t.Helper()

	env, err := cel.NewEnv(
		cel.Variable("object", cel.DynType),
		cel.Variable("namespaceObject", cel.DynType),
		cel.Variable("variables", cel.DynType),
		ext.Strings(),
	)
	require.NoError(t, err)

	return env
}

// vapPolicyDoc is the subset of the rendered ValidatingAdmissionPolicy the test
// reads.
type vapPolicyDoc struct {
	Spec struct {
		Variables []struct {
			Name       string `yaml:"name"`
			Expression string `yaml:"expression"`
		} `yaml:"variables"`
		Validations []struct {
			Expression        string `yaml:"expression"`
			MessageExpression string `yaml:"messageExpression"`
		} `yaml:"validations"`
	} `yaml:"spec"`
}

// loadPolicyDoc reads the chart template, renders away the single Helm
// directive the expressions carry (the label key), drops the remaining control
// directives, and unmarshals the first YAML document (the policy; the binding
// follows after "---") into vapPolicyDoc.
func loadPolicyDoc(t *testing.T) vapPolicyDoc {
	t.Helper()

	raw, err := os.ReadFile(vapTemplatePath)
	require.NoError(t, err)

	src := strings.SplitN(string(raw), "\n---", 2)[0]
	src = strings.ReplaceAll(src, `{{ .Values.hostnameOwnershipPolicy.labelKey | quote }}`, `"`+celTestLabelKey+`"`)
	src = strings.ReplaceAll(src, `{{ .Values.hostnameOwnershipPolicy.labelKey }}`, celTestLabelKey)
	src = strings.ReplaceAll(src, `{{ include "cf-tunnel-gw-ctrl.fullname" . }}`, "test")

	// Drop every remaining templated line (the {{- if guard and the labels
	// include) so the policy document parses as plain YAML.
	var kept []string

	for line := range strings.SplitSeq(src, "\n") {
		if strings.Contains(line, "{{") {
			continue
		}

		kept = append(kept, line)
	}

	var policy vapPolicyDoc
	require.NoError(t, yaml.Unmarshal([]byte(strings.Join(kept, "\n")), &policy))

	return policy
}

// TestCELMessageExpressionsCompile pins the denial-message expressions. The
// verdict expressions are exercised by TestCELPolicyMatchesVectors, but a typo
// in a messageExpression would surface only at live admission (it is the
// human-facing reason string, not the allow/deny decision). Compile each
// non-empty messageExpression in the same environment and assert it evaluates
// to a string so a malformed one fails in CI instead.
func TestCELMessageExpressionsCompile(t *testing.T) {
	t.Parallel()

	policy := loadPolicyDoc(t)
	env := newCELTestEnv(t)

	input := map[string]any{
		"object":          buildRouteObject([]string{"app.example.com"}),
		"namespaceObject": buildNamespaceObject("example.com"),
		"variables":       map[string]any{"suffix": "example.com"},
	}

	compiled := 0

	for _, validation := range policy.Spec.Validations {
		if validation.MessageExpression == "" {
			continue
		}

		prg := compileCEL(t, env, validation.MessageExpression)

		out, _, err := prg.Eval(input)
		require.NoError(t, err)

		_, ok := out.Value().(string)
		require.True(t, ok, "a messageExpression must evaluate to a string: %s", validation.MessageExpression)

		compiled++
	}

	require.Positive(t, compiled, "the policy must ship at least one messageExpression to compile")
}

// extractCELExpressions returns the suffix-variable expression plus the ordered
// validation expressions — the EXACT strings the chart ships.
func extractCELExpressions(t *testing.T) (string, []string) {
	t.Helper()

	policy := loadPolicyDoc(t)

	var suffix string

	for _, variable := range policy.Spec.Variables {
		if variable.Name == "suffix" {
			suffix = variable.Expression
		}
	}

	require.NotEmpty(t, suffix, "the suffix variable expression must be extracted")

	validations := make([]string, 0, len(policy.Spec.Validations))
	for _, validation := range policy.Spec.Validations {
		validations = append(validations, validation.Expression)
	}

	require.Len(t, validations, 3, "the policy must ship exactly three validations")

	return suffix, validations
}
