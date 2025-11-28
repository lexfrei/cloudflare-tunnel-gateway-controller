package ingress

import (
	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/zero_trust"
)

// Rule represents a simplified ingress rule for comparison.
type Rule struct {
	Hostname string
	Path     string
	Service  string
}

// RuleFromUpdate converts an update params ingress rule to a Rule for comparison.
func RuleFromUpdate(r *zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress) Rule {
	return Rule{
		Hostname: r.Hostname.Value,
		Path:     r.Path.Value,
		Service:  r.Service.Value,
	}
}

// RuleFromGet converts a get response ingress rule to a Rule for comparison.
func RuleFromGet(r *zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress) Rule {
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

// IsCatchAll returns true if the rule is a catch-all rule (no hostname).
func IsCatchAll(r Rule) bool {
	return r.Hostname == ""
}

// DiffRules computes the difference between current and desired rules.
// Returns rules to add (in desired but not in current) and rules to remove (in current but not in desired).
// Catch-all rules are excluded from comparison.
func DiffRules(
	current []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress,
	desired []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
) (toAdd []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, toRemove []Rule) {
	currentRules := make([]Rule, 0, len(current))

	for idx := range current {
		rule := RuleFromGet(&current[idx])
		if !IsCatchAll(rule) {
			currentRules = append(currentRules, rule)
		}
	}

	desiredRules := make([]Rule, 0, len(desired))
	desiredMap := make(map[int]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress)

	for idx := range desired {
		rule := RuleFromUpdate(&desired[idx])
		if !IsCatchAll(rule) {
			desiredRules = append(desiredRules, rule)
			desiredMap[len(desiredRules)-1] = desired[idx]
		}
	}

	// Find rules to add (in desired but not in current)
	for idx, desiredRule := range desiredRules {
		found := false

		for _, currentRule := range currentRules {
			if RulesEqual(desiredRule, currentRule) {
				found = true

				break
			}
		}

		if !found {
			toAdd = append(toAdd, desiredMap[idx])
		}
	}

	// Find rules to remove (in current but not in desired)
	for _, currentRule := range currentRules {
		found := false

		for _, desiredRule := range desiredRules {
			if RulesEqual(currentRule, desiredRule) {
				found = true

				break
			}
		}

		if !found {
			toRemove = append(toRemove, currentRule)
		}
	}

	return toAdd, toRemove
}

// ApplyDiff applies the diff to current rules, returning the final rule set.
// Removes orphaned rules, keeps existing rules, adds new rules.
func ApplyDiff(
	current []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress,
	toAdd []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
	toRemove []Rule,
) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
	result := make([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, 0, len(current)+len(toAdd))

	// Keep current rules that are not in toRemove (and not catch-all)
	for idx := range current {
		rule := RuleFromGet(&current[idx])

		// Skip catch-all, will be handled separately
		if IsCatchAll(rule) {
			continue
		}

		shouldRemove := false

		for _, removeRule := range toRemove {
			if RulesEqual(rule, removeRule) {
				shouldRemove = true

				break
			}
		}

		if !shouldRemove {
			result = append(result, convertGetToUpdate(&current[idx]))
		}
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
