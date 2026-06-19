package hostnameownership

// Shared fixture names for the vector table (hoisted for goconst; RFC 2606
// documentation domains).
const (
	vectorSuffix      = "team-a.example.com"
	vectorInsideHost  = "app.team-a.example.com"
	vectorForeignHost = "app.team-b.example.com"
)

// Vector is one shared semantic test case for the hostname-ownership rule.
// The SAME table is executed against both enforcement layers:
//
//   - the controller-side Policy.Evaluate (package unit tests), and
//   - the CEL ValidatingAdmissionPolicy rendered by the Helm chart (e2e suite,
//     which creates a labelled namespace per vector and asserts admission).
//
// Suffix is the value of the ownership label on the (policed) namespace; an
// empty Suffix means the label is absent. Keep every vector expressible in
// both layers — no controller-only concepts here.
type Vector struct {
	Name        string
	Suffix      string
	Hostnames   []string
	WantAllowed bool
}

// Vectors returns the shared semantic contract between the CEL admission
// layer and the controller enforcement layer. Extend HERE first; both layers'
// tests pick the change up automatically.
func Vectors() []Vector {
	return append(allowedVectors(), deniedVectors()...)
}

func allowedVectors() []Vector {
	return []Vector{
		{
			Name:        "exact suffix match allowed",
			Suffix:      vectorSuffix,
			Hostnames:   []string{vectorSuffix},
			WantAllowed: true,
		},
		{
			Name:        "subdomain allowed",
			Suffix:      vectorSuffix,
			Hostnames:   []string{vectorInsideHost},
			WantAllowed: true,
		},
		{
			Name:        "deep subdomain allowed",
			Suffix:      vectorSuffix,
			Hostnames:   []string{"x.y.team-a.example.com"},
			WantAllowed: true,
		},
		{
			Name:        "wildcard within suffix allowed",
			Suffix:      vectorSuffix,
			Hostnames:   []string{"*.team-a.example.com"},
			WantAllowed: true,
		},
		{
			Name:        "multiple in-suffix hostnames allowed",
			Suffix:      vectorSuffix,
			Hostnames:   []string{"a.team-a.example.com", "b.team-a.example.com"},
			WantAllowed: true,
		},
		{
			// Label VALUES may carry uppercase; DNS comparison is
			// case-insensitive. Both layers normalize the suffix AND the route
			// hostname (CEL lowerAscii, Go ToLower) — the CRD schema already
			// guarantees lowercase route hostnames, but neither layer depends
			// on that. An uppercase ROUTE hostname can't be a shared vector
			// (the CRD rejects it before admission), so the controller-side
			// case-insensitivity is pinned by TestPolicy_UppercaseHostnameNormalised.
			Name:        "uppercase suffix label normalized",
			Suffix:      "Team-A.Example.COM",
			Hostnames:   []string{vectorInsideHost},
			WantAllowed: true,
		},
	}
}

func deniedVectors() []Vector {
	return []Vector{
		{
			Name:      "foreign hostname denied",
			Suffix:    vectorSuffix,
			Hostnames: []string{vectorForeignHost},
		},
		{
			Name:      "suffix-string trick denied",
			Suffix:    vectorSuffix,
			Hostnames: []string{"evilteam-a.example.com"},
		},
		{
			// The classic suffix-as-a-label-of-a-foreign-zone confusion: the
			// allowed suffix appears verbatim but as a prefix of an attacker's
			// domain. The "."-boundary suffix match denies it in both layers.
			Name:      "suffix as prefix of foreign zone denied",
			Suffix:    vectorSuffix,
			Hostnames: []string{"team-a.example.com.evil.net"},
		},
		{
			Name:      "wildcard escaping the suffix denied",
			Suffix:    vectorSuffix,
			Hostnames: []string{"*.example.com"},
		},
		{
			Name:      "parent zone denied",
			Suffix:    vectorSuffix,
			Hostnames: []string{"example.com"},
		},
		{
			Name:      "mixed in and out denied",
			Suffix:    vectorSuffix,
			Hostnames: []string{"ok.team-a.example.com", vectorForeignHost},
		},
		{
			Name:      "no hostnames denied",
			Suffix:    vectorSuffix,
			Hostnames: nil,
		},
		{
			// Distinct from nil ON THE WIRE: `hostnames: []` is a present
			// field, and CEL's has() semantics for present-but-empty lists is
			// exactly the trap this table exists to pin (the e2e sends the
			// empty list explicitly via unstructured — typed clients drop it
			// through omitempty).
			Name:      "present-but-empty hostnames denied",
			Suffix:    vectorSuffix,
			Hostnames: []string{},
		},
		{
			Name:      "missing ownership label denied",
			Suffix:    "",
			Hostnames: []string{vectorInsideHost},
		},
	}
}
