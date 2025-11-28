package ingress_test

import (
	"testing"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/zero_trust"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
)

func TestRulesEqual(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ruleA    ingress.Rule
		ruleB    ingress.Rule
		expected bool
	}{
		{
			name:     "identical rules",
			ruleA:    ingress.Rule{Hostname: "app.example.com", Path: "/api", Service: "http://svc:8080"},
			ruleB:    ingress.Rule{Hostname: "app.example.com", Path: "/api", Service: "http://svc:8080"},
			expected: true,
		},
		{
			name:     "different hostname",
			ruleA:    ingress.Rule{Hostname: "app1.example.com", Path: "/api", Service: "http://svc:8080"},
			ruleB:    ingress.Rule{Hostname: "app2.example.com", Path: "/api", Service: "http://svc:8080"},
			expected: false,
		},
		{
			name:     "different path",
			ruleA:    ingress.Rule{Hostname: "app.example.com", Path: "/api", Service: "http://svc:8080"},
			ruleB:    ingress.Rule{Hostname: "app.example.com", Path: "/web", Service: "http://svc:8080"},
			expected: false,
		},
		{
			name:     "different service",
			ruleA:    ingress.Rule{Hostname: "app.example.com", Path: "/api", Service: "http://svc1:8080"},
			ruleB:    ingress.Rule{Hostname: "app.example.com", Path: "/api", Service: "http://svc2:8080"},
			expected: false,
		},
		{
			name:     "empty rules",
			ruleA:    ingress.Rule{},
			ruleB:    ingress.Rule{},
			expected: true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			result := ingress.RulesEqual(testCase.ruleA, testCase.ruleB)
			if result != testCase.expected {
				t.Errorf("RulesEqual() = %v, want %v", result, testCase.expected)
			}
		})
	}
}

func TestIsCatchAll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rule     ingress.Rule
		expected bool
	}{
		{
			name:     "catch-all rule (empty hostname)",
			rule:     ingress.Rule{Hostname: "", Service: "http_status:404"},
			expected: true,
		},
		{
			name:     "regular rule with hostname",
			rule:     ingress.Rule{Hostname: "app.example.com", Service: "http://svc:8080"},
			expected: false,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			result := ingress.IsCatchAll(testCase.rule)
			if result != testCase.expected {
				t.Errorf("IsCatchAll() = %v, want %v", result, testCase.expected)
			}
		})
	}
}

func TestDiffRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		current        []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress
		desired        []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress
		expectedToAdd  int
		expectedRemove int
	}{
		{
			name:    "empty current, add all desired",
			current: []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress{},
			desired: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{Hostname: cloudflare.F("app.example.com"), Service: cloudflare.F("http://svc:8080")},
			},
			expectedToAdd:  1,
			expectedRemove: 0,
		},
		{
			name: "empty desired, remove all current",
			current: []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress{
				{Hostname: "app.example.com", Service: "http://svc:8080"},
			},
			desired:        []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{},
			expectedToAdd:  0,
			expectedRemove: 1,
		},
		{
			name: "no changes needed",
			current: []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress{
				{Hostname: "app.example.com", Service: "http://svc:8080"},
			},
			desired: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{Hostname: cloudflare.F("app.example.com"), Service: cloudflare.F("http://svc:8080")},
			},
			expectedToAdd:  0,
			expectedRemove: 0,
		},
		{
			name: "add one, remove one",
			current: []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress{
				{Hostname: "old.example.com", Service: "http://old:8080"},
			},
			desired: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{Hostname: cloudflare.F("new.example.com"), Service: cloudflare.F("http://new:8080")},
			},
			expectedToAdd:  1,
			expectedRemove: 1,
		},
		{
			name: "catch-all in current is ignored",
			current: []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress{
				{Hostname: "app.example.com", Service: "http://svc:8080"},
				{Hostname: "", Service: "http_status:404"}, // catch-all
			},
			desired: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{Hostname: cloudflare.F("app.example.com"), Service: cloudflare.F("http://svc:8080")},
			},
			expectedToAdd:  0,
			expectedRemove: 0,
		},
		{
			name: "catch-all in desired is ignored",
			current: []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress{
				{Hostname: "app.example.com", Service: "http://svc:8080"},
			},
			desired: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{Hostname: cloudflare.F("app.example.com"), Service: cloudflare.F("http://svc:8080")},
				{Service: cloudflare.F("http_status:404")}, // catch-all
			},
			expectedToAdd:  0,
			expectedRemove: 0,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			toAdd, toRemove := ingress.DiffRules(testCase.current, testCase.desired)

			if len(toAdd) != testCase.expectedToAdd {
				t.Errorf("toAdd count = %d, want %d", len(toAdd), testCase.expectedToAdd)
			}

			if len(toRemove) != testCase.expectedRemove {
				t.Errorf("toRemove count = %d, want %d", len(toRemove), testCase.expectedRemove)
			}
		})
	}
}

func TestApplyDiff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		current       []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress
		toAdd         []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress
		toRemove      []ingress.Rule
		expectedCount int
	}{
		{
			name:          "empty everything",
			current:       []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress{},
			toAdd:         []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{},
			toRemove:      []ingress.Rule{},
			expectedCount: 0,
		},
		{
			name: "keep existing rule",
			current: []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress{
				{Hostname: "app.example.com", Service: "http://svc:8080"},
			},
			toAdd:         []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{},
			toRemove:      []ingress.Rule{},
			expectedCount: 1,
		},
		{
			name:    "add new rule to empty",
			current: []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress{},
			toAdd: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{Hostname: cloudflare.F("app.example.com"), Service: cloudflare.F("http://svc:8080")},
			},
			toRemove:      []ingress.Rule{},
			expectedCount: 1,
		},
		{
			name: "remove orphaned rule",
			current: []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress{
				{Hostname: "orphan.example.com", Service: "http://orphan:8080"},
			},
			toAdd: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{},
			toRemove: []ingress.Rule{
				{Hostname: "orphan.example.com", Service: "http://orphan:8080"},
			},
			expectedCount: 0,
		},
		{
			name: "skip catch-all in current",
			current: []zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress{
				{Hostname: "app.example.com", Service: "http://svc:8080"},
				{Hostname: "", Service: "http_status:404"},
			},
			toAdd:         []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{},
			toRemove:      []ingress.Rule{},
			expectedCount: 1, // catch-all is skipped
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			result := ingress.ApplyDiff(testCase.current, testCase.toAdd, testCase.toRemove)

			if len(result) != testCase.expectedCount {
				t.Errorf("result count = %d, want %d", len(result), testCase.expectedCount)
			}
		})
	}
}

func TestEnsureCatchAll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		rules               []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress
		expectedCount       int
		expectedLastService string
	}{
		{
			name:                "empty rules gets catch-all",
			rules:               []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{},
			expectedCount:       1,
			expectedLastService: ingress.CatchAllService,
		},
		{
			name: "rules without catch-all get one added",
			rules: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{Hostname: cloudflare.F("app.example.com"), Service: cloudflare.F("http://svc:8080")},
			},
			expectedCount:       2,
			expectedLastService: ingress.CatchAllService,
		},
		{
			name: "existing catch-all is moved to end",
			rules: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{Service: cloudflare.F("http_status:404")}, // catch-all in wrong position
				{Hostname: cloudflare.F("app.example.com"), Service: cloudflare.F("http://svc:8080")},
			},
			expectedCount:       2,
			expectedLastService: ingress.CatchAllService,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			result := ingress.EnsureCatchAll(testCase.rules)

			if len(result) != testCase.expectedCount {
				t.Errorf("result count = %d, want %d", len(result), testCase.expectedCount)
			}

			if len(result) > 0 {
				lastRule := result[len(result)-1]
				if lastRule.Service.Value != testCase.expectedLastService {
					t.Errorf("last service = %s, want %s", lastRule.Service.Value, testCase.expectedLastService)
				}
			}
		})
	}
}
