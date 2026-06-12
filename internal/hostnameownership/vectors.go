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
			Name:        "uppercase hostname normalised",
			Suffix:      vectorSuffix,
			Hostnames:   []string{"APP.Team-A.Example.Com"},
			WantAllowed: true,
		},
		{
			Name:        "multiple in-suffix hostnames allowed",
			Suffix:      vectorSuffix,
			Hostnames:   []string{"a.team-a.example.com", "b.team-a.example.com"},
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
			Name:      "missing ownership label denied",
			Suffix:    "",
			Hostnames: []string{vectorInsideHost},
		},
	}
}
