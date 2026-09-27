package ingress

import (
	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/zero_trust"
)

// Rule represents a simplified ingress rule for comparison.
type Rule struct {
	Hostname string
	Path     string
	Service  string
}

// RuleFromUpdate converts an update params ingress rule to a Rule for comparison.
// Returns empty Rule if r is nil.
func RuleFromUpdate(r *zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress) Rule {
	if r == nil {
		return Rule{}
	}

	return Rule{
		Hostname: r.Hostname.Value,
		Path:     r.Path.Value,
		Service:  r.Service.Value,
	}
}

// RuleFromGet converts a get response ingress rule to a Rule for comparison.
// Returns empty Rule if r is nil.
func RuleFromGet(r *zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress) Rule {
	if r == nil {
		return Rule{}
	}

	return Rule{
		Hostname: r.Hostname,
		Path:     r.Path,
		Service:  r.Service,
	}
}

// RulesEqual compares two rules for equality.
func RulesEqual(a, b Rule) bool {
	return a.Hostname == b.Hostname &&
		a.Path == b.Path &&
		a.Service == b.Service
}

// IsCatchAll returns true if the rule is a catch-all rule (no hostname and catch-all service).
// Wildcard routes (no hostname but with a real backend) are NOT catch-all.
func IsCatchAll(r Rule) bool {
	return r.Hostname == "" && r.Service == CatchAllService
}

// DiffRules computes the difference between current and desired rules as
// multisets: each desired copy of a rule is matched against at most one
// deployed copy. Returns the desired copies with no deployed counterpart
// (toAdd) and the deployed copies with no desired counterpart (toRemove), so
// applying both leaves the document holding every rule exactly as many times
// as it is desired. Catch-all rules are excluded from comparison.
func DiffRules(
	current []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress,
	desired []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
) ([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, []Rule) {
	unmatched := make(map[Rule]int, len(current))

	for idx := range current {
		if rule := RuleFromGet(&current[idx]); !IsCatchAll(rule) {
			unmatched[rule]++
		}
	}

	var toAdd []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress

	for idx := range desired {
		rule := RuleFromUpdate(&desired[idx])
		if IsCatchAll(rule) {
			continue
		}

		if unmatched[rule] > 0 {
			unmatched[rule]--

			continue
		}

		toAdd = append(toAdd, desired[idx])
	}

	var toRemove []Rule

	for idx := range current {
		if rule := RuleFromGet(&current[idx]); unmatched[rule] > 0 && !IsCatchAll(rule) {
			unmatched[rule]--
			toRemove = append(toRemove, rule)
		}
	}

	return toAdd, toRemove
}

// ApplyDiff applies the diff to current rules, returning the final rule set.
// Removes one deployed copy per toRemove entry, keeps the rest, adds toAdd.
func ApplyDiff(
	current []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress,
	toAdd []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
	toRemove []Rule,
) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
	result := make([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, 0, len(current)+len(toAdd))

	// Each toRemove entry drops one deployed copy, so a rule deployed more
	// often than it is desired shrinks to its desired count.
	pending := make(map[Rule]int, len(toRemove))
	for _, rule := range toRemove {
		pending[rule]++
	}

	for idx := range current {
		rule := RuleFromGet(&current[idx])

		// Skip catch-all, will be handled separately
		if IsCatchAll(rule) {
			continue
		}

		if pending[rule] > 0 {
			pending[rule]--

			continue
		}

		result = append(result, convertGetToUpdate(&current[idx]))
	}

	// Add new rules
	result = append(result, toAdd...)

	return result
}

// EnsureCatchAll ensures a catch-all rule exists at the end of the rules.
func EnsureCatchAll(
	rules []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
	// Check if catch-all already exists and filter it out
	filtered := make([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, 0, len(rules))

	for idx := range rules {
		rule := RuleFromUpdate(&rules[idx])
		if !IsCatchAll(rule) {
			filtered = append(filtered, rules[idx])
		}
	}

	// Add catch-all at the end
	filtered = append(filtered, zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
		Service: cloudflare.F(CatchAllService),
	})

	return filtered
}

// convertGetToUpdate converts a get response ingress rule to update params format.
func convertGetToUpdate(
	r *zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress,
) zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
	result := zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
		Service: cloudflare.F(r.Service),
	}

	if r.Hostname != "" {
		result.Hostname = cloudflare.F(r.Hostname)
	}

	if r.Path != "" {
		result.Path = cloudflare.F(r.Path)
	}

	return result
}

// RulesUnchanged reports whether the desired ingress document is identical to
// the currently-deployed one. The comparison is order-sensitive — cloudflared
// ingress rules are first-match — and covers the full document including the
// catch-all. Used to skip the Cloudflare configuration write entirely on
// steady-state syncs: the configurations endpoint is a whole-document update,
// so the only way to reduce API traffic is to not write at all.
func RulesUnchanged(
	current []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress,
	desired []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
) bool {
	if len(current) != len(desired) {
		return false
	}

	for idx := range current {
		if !RulesEqual(RuleFromGet(&current[idx]), RuleFromUpdate(&desired[idx])) {
			return false
		}
	}

	return true
}
