package hostnameownership_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVendoredHostnamePatternStaysASCII is a vendor-drift canary for the
// gateway-api Hostname CRD validation pattern. The entire CEL-vs-Go layer
// parity rests on hostnames being ASCII-lowercase-only: Go's strings.ToLower
// folds non-ASCII (e.g. the Kelvin sign K → "k") while CEL's lowerAscii
// does not, so a non-ASCII hostname would make the two layers disagree and
// reopen a homograph bypass. The Hostname pattern forbids non-ASCII today; a
// re-vendor that relaxes it must fail HERE first, not silently in production.
// (Mirrors internal/tunnel/origin_vendor_drift_test.go for the fork.)
func TestVendoredHostnamePatternStaysASCII(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../vendor/sigs.k8s.io/gateway-api/apis/v1/shared_types.go")
	require.NoError(t, err, "reading vendored gateway-api shared_types.go")

	source := string(raw)
	require.Contains(t, source, "type Hostname string", "the Hostname type moved — re-point this canary")

	// The ASCII-lowercase-only pattern the parity argument depends on (optional
	// `*.` wildcard prefix, then lowercase-alphanumeric/dash labels joined by
	// dots). A raw literal: the kubebuilder marker stores it verbatim.
	const asciiHostnamePattern = `^(\*\.)?[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`

	assert.Contains(t, source, asciiHostnamePattern,
		"the gateway-api Hostname pattern must stay ASCII-lowercase-only — a relaxation reopens the "+
			"homograph axis that strings.ToLower (Unicode) and CEL lowerAscii diverge on")
}
