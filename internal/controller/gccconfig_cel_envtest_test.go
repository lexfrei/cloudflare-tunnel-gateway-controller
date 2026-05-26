//go:build envtest

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// TestGatewayClassConfig_AccountIDCELValidation pins the CRD-level CEL
// rule that enforces the Cloudflare account-ID format (32-char
// lowercase hex) at admission time. Closes #110.
//
// CEL validations attach to the CRD's openAPIV3Schema and are
// evaluated by the kube-apiserver, so we exercise them through the
// real envtest control plane rather than the fake client (which does
// not run schema validation).
func TestGatewayClassConfig_AccountIDCELValidation(t *testing.T) {
	t.Parallel()

	require.NotNil(t, envK8sClient, "envtest must be wired up; see suite_envtest_test.go")

	cases := []struct {
		name      string
		accountID string
		wantErr   bool
		wantSub   string
	}{
		{
			name:      "empty accountID accepted (field is optional)",
			accountID: "",
			wantErr:   false,
		},
		{
			name:      "valid 32-char lowercase hex accepted",
			accountID: "0123456789abcdef0123456789abcdef",
			wantErr:   false,
		},
		{
			name:      "uppercase hex rejected (must be lowercase)",
			accountID: "0123456789ABCDEF0123456789ABCDEF",
			wantErr:   true,
			wantSub:   "lowercase hexadecimal",
		},
		{
			name:      "wrong length rejected (31 chars)",
			accountID: "0123456789abcdef0123456789abcde",
			wantErr:   true,
			wantSub:   "lowercase hexadecimal",
		},
		{
			name:      "wrong length rejected (33 chars)",
			accountID: "0123456789abcdef0123456789abcdef0",
			wantErr:   true,
			wantSub:   "lowercase hexadecimal",
		},
		{
			name:      "non-hex characters rejected",
			accountID: "ghijkl12345678901234567890123456",
			wantErr:   true,
			wantSub:   "lowercase hexadecimal",
		},
	}

	for idx, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gcc := &v1alpha1.GatewayClassConfig{
				ObjectMeta: metav1.ObjectMeta{
					// Unique per-case name so parallel subtests don't collide
					// in the shared envtest namespace.
					Name: stringHashSuffix("cel-test", idx, tc.name),
				},
				Spec: v1alpha1.GatewayClassConfigSpec{
					TunnelID: "12345678-1234-1234-1234-123456789012",
					CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
						Name: "cloudflare-credentials",
					},
					AccountID: tc.accountID,
				},
			}

			err := envK8sClient.Create(context.Background(), gcc)
			if tc.wantErr {
				require.Error(t, err, "invalid accountID %q must be rejected at admission", tc.accountID)
				assert.Contains(t, strings.ToLower(err.Error()), strings.ToLower(tc.wantSub),
					"rejection message must mention the rule's `message` text so users see what they got wrong")

				return
			}

			require.NoError(t, err, "valid accountID %q must pass admission", tc.accountID)
			// Best-effort cleanup; the envtest namespace is torn down per
			// suite anyway, so a failure here doesn't poison other cases.
			_ = envK8sClient.Delete(context.Background(), gcc)
		})
	}
}

// stringHashSuffix builds a deterministic resource name suffix from the
// case index + name so parallel subtests don't collide in the same
// cluster-scoped namespace. Kubernetes name shape (RFC 1123 subdomain)
// requires lowercase + hyphen; we sanitise the test name into that.
func stringHashSuffix(prefix string, idx int, name string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		case r >= '0' && r <= '9':
			return r
		case r == '-':
			return r
		default:
			return '-'
		}
	}, name)
	// Collapse repeats / trim leading and trailing dashes; clip to keep
	// within Kubernetes' 63-char limit.
	for strings.Contains(safe, "--") {
		safe = strings.ReplaceAll(safe, "--", "-")
	}

	safe = strings.Trim(safe, "-")
	if len(safe) > 40 {
		safe = safe[:40]
	}

	return prefix + "-" + safe + "-" + string(rune('a'+idx))
}
