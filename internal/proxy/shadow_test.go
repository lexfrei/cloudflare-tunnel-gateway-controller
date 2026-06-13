package proxy_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

var (
	shadowT0 = metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	shadowT1 = metav1.NewTime(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
)

// prov is a shorthand RuleProvenance constructor for the table tests.
func prov(kind, namespace, name string, ts metav1.Time, ruleIdx int) proxy.RuleProvenance {
	return proxy.RuleProvenance{
		Kind:              kind,
		Namespace:         namespace,
		Name:              name,
		CreationTimestamp: ts,
		RuleIndex:         ruleIdx,
	}
}

func pathPrefixMatch(value string) proxy.RouteMatch {
	return proxy.RouteMatch{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: value}}
}

func pathExactMatch(value string) proxy.RouteMatch {
	return proxy.RouteMatch{Path: &proxy.PathMatch{Type: proxy.PathMatchExact, Value: value}}
}

// TestDetectShadowedRules_ExactDuplicateAcrossRoutes pins the core contract:
// an identical (hostname, match) pair claimed by a lower-precedence route is
// flagged on the LOSER with the winner's identity and the precedence basis.
func TestDetectShadowedRules_ExactDuplicateAcrossRoutes(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{Hostnames: []string{"app.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
			{Hostnames: []string{"app.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
		},
		Provenance: []proxy.RuleProvenance{
			prov("HTTPRoute", "team-a", "app", shadowT0, 0),
			prov("HTTPRoute", "team-b", "intruder", shadowT1, 0),
		},
	}

	diags := proxy.DetectShadowedRules(cfg)
	require.Len(t, diags, 1)

	diag := diags[0]
	assert.Equal(t, proxy.DiagnosticShadowed, diag.Target)
	assert.Equal(t, "team-b", diag.Namespace, "the diagnostic must land on the losing route")
	assert.Equal(t, "intruder", diag.Name)
	assert.Equal(t, 0, diag.RuleIndex)
	assert.False(t, diag.WholeRule)
	assert.Contains(t, diag.Message, "HTTPRoute team-a/app")
	assert.Contains(t, diag.Message, "older creationTimestamp")
	assert.Contains(t, diag.Message, "app.example.com")
}

// TestDetectShadowedRules_TimestampTieUsesNamespaceName pins the second
// precedence criterion: equal creationTimestamps fall back to alphabetical
// {namespace}/{name}.
func TestDetectShadowedRules_TimestampTieUsesNamespaceName(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{Hostnames: []string{"app.example.com"}, Matches: []proxy.RouteMatch{pathExactMatch("/v1")}},
			{Hostnames: []string{"app.example.com"}, Matches: []proxy.RouteMatch{pathExactMatch("/v1")}},
		},
		Provenance: []proxy.RuleProvenance{
			prov("HTTPRoute", "team-a", "app", shadowT0, 0),
			prov("HTTPRoute", "team-b", "app", shadowT0, 0),
		},
	}

	diags := proxy.DetectShadowedRules(cfg)
	require.Len(t, diags, 1)
	assert.Equal(t, "team-b", diags[0].Namespace)
	assert.Contains(t, diags[0].Message, "alphabetical {namespace}/{name}")
}

// TestDetectShadowedRules_NoFalsePositives pins the deliberately-out-of-scope
// cases: same-route duplicates (within-route first-wins is spec'd separately),
// different hostnames, prefix-vs-exact on the same path value, and
// wildcard-vs-exact hostnames. None of these is the deterministic zero-traffic
// collision the condition exists for.
func TestDetectShadowedRules_NoFalsePositives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *proxy.Config
	}{
		{
			name: "same route duplicate pair",
			cfg: &proxy.Config{
				Rules: []proxy.RouteRule{
					{Hostnames: []string{"a.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
					{Hostnames: []string{"a.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
				},
				Provenance: []proxy.RuleProvenance{
					prov("HTTPRoute", "ns", "same", shadowT0, 0),
					prov("HTTPRoute", "ns", "same", shadowT0, 1),
				},
			},
		},
		{
			name: "different hostnames",
			cfg: &proxy.Config{
				Rules: []proxy.RouteRule{
					{Hostnames: []string{"a.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
					{Hostnames: []string{"b.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
				},
				Provenance: []proxy.RuleProvenance{
					prov("HTTPRoute", "ns", "one", shadowT0, 0),
					prov("HTTPRoute", "ns", "two", shadowT1, 0),
				},
			},
		},
		{
			name: "prefix versus exact on the same path",
			cfg: &proxy.Config{
				Rules: []proxy.RouteRule{
					{Hostnames: []string{"a.example.com"}, Matches: []proxy.RouteMatch{pathExactMatch("/v1")}},
					{Hostnames: []string{"a.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/v1")}},
				},
				Provenance: []proxy.RuleProvenance{
					prov("HTTPRoute", "ns", "one", shadowT0, 0),
					prov("HTTPRoute", "ns", "two", shadowT1, 0),
				},
			},
		},
		{
			name: "wildcard versus exact hostname",
			cfg: &proxy.Config{
				Rules: []proxy.RouteRule{
					{Hostnames: []string{"*.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
					{Hostnames: []string{"app.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
				},
				Provenance: []proxy.RuleProvenance{
					prov("HTTPRoute", "ns", "wild", shadowT0, 0),
					prov("HTTPRoute", "ns", "exact", shadowT1, 0),
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Empty(t, proxy.DetectShadowedRules(tt.cfg))
		})
	}
}

// TestDetectShadowedRules_CatchAllPair pins that two rules with NO hostnames
// and NO matches (the catch-all bucket) collide on the empty key — the second
// one deterministically receives zero traffic.
func TestDetectShadowedRules_CatchAllPair(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{},
			{},
		},
		Provenance: []proxy.RuleProvenance{
			prov("HTTPRoute", "ns", "first", shadowT0, 0),
			prov("HTTPRoute", "ns", "second", shadowT1, 0),
		},
	}

	diags := proxy.DetectShadowedRules(cfg)
	require.Len(t, diags, 1)
	assert.Equal(t, "second", diags[0].Name)
}

// TestDetectShadowedRules_PartialShadowEmitsPerPair pins granularity: a rule
// with two ORed matches where only one collides yields exactly one diagnostic
// (for the shadowed pair), because the other match still serves traffic.
func TestDetectShadowedRules_PartialShadowEmitsPerPair(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{Hostnames: []string{"a.example.com"}, Matches: []proxy.RouteMatch{pathExactMatch("/v1")}},
			{Hostnames: []string{"a.example.com"}, Matches: []proxy.RouteMatch{pathExactMatch("/v1"), pathExactMatch("/v2")}},
		},
		Provenance: []proxy.RuleProvenance{
			prov("HTTPRoute", "ns", "winner", shadowT0, 0),
			prov("HTTPRoute", "ns", "loser", shadowT1, 0),
		},
	}

	diags := proxy.DetectShadowedRules(cfg)
	require.Len(t, diags, 1)
	assert.Contains(t, diags[0].Message, "/v1")
	assert.NotContains(t, diags[0].Message, "/v2")
}

// TestDetectShadowedRules_HigherPriorityLaterRuleWins pins winner attribution
// against the ROUTER's actual ordering, not flattening order. The router ranks
// a rule by the MAX priority across its ORed matches and serves a request with
// the first rule (priority desc) whose ANY match hits. So an older route A
// [prefix /] loses its traffic to a newer route B [prefix /, exact /admin]:
// B's exact match lifts the whole rule above A, and B's "prefix /" arm
// swallows every request A would have served. The diagnostic must land on A
// (the starved route), naming B as the winner — flagging B would invert
// winner and loser exactly when the observability matters.
func TestDetectShadowedRules_HigherPriorityLaterRuleWins(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{Hostnames: []string{"app.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
			{Hostnames: []string{"app.example.com"}, Matches: []proxy.RouteMatch{
				pathPrefixMatch("/"), pathExactMatch("/admin"),
			}},
		},
		Provenance: []proxy.RuleProvenance{
			prov("HTTPRoute", "team-a", "older-starved", shadowT0, 0),
			prov("HTTPRoute", "team-b", "newer-winner", shadowT1, 0),
		},
	}

	diags := proxy.DetectShadowedRules(cfg)
	require.Len(t, diags, 1)
	assert.Equal(t, "older-starved", diags[0].Name,
		"the route the router actually starves must carry the diagnostic")
	assert.Equal(t, "team-a", diags[0].Namespace)
	assert.Contains(t, diags[0].Message, "HTTPRoute team-b/newer-winner",
		"the winner named in the message must be the route the router serves")
}

// TestDetectShadowedRules_ThreeWayCollisionNamesTheFinalWinner pins winner
// attribution under ≥3 claimants on one pair: every loser's message must name
// the route the router ACTUALLY serves ("matching requests are served by that
// route"), which is only known after all claimants are seen. With claimants
// low → mid → high (by priority, in flattening order), the low-priority
// route's diagnostic must name the high-priority route — not the mid one it
// happened to lose to first, which itself serves zero traffic on the pair.
func TestDetectShadowedRules_ThreeWayCollisionNamesTheFinalWinner(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{Hostnames: []string{"app.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
			{Hostnames: []string{"app.example.com"}, Matches: []proxy.RouteMatch{
				pathPrefixMatch("/"), pathPrefixMatch("/mid-priority-arm"),
			}},
			{Hostnames: []string{"app.example.com"}, Matches: []proxy.RouteMatch{
				pathPrefixMatch("/"), pathExactMatch("/high-priority-arm"),
			}},
		},
		Provenance: []proxy.RuleProvenance{
			prov("HTTPRoute", "ns", "low", shadowT0, 0),
			prov("HTTPRoute", "ns", "mid", shadowT1, 0),
			prov("HTTPRoute", "ns", "high", shadowT1, 0),
		},
	}

	diags := proxy.DetectShadowedRules(cfg)

	byLoser := make(map[string]string)
	for _, diag := range diags {
		byLoser[diag.Name] = diag.Message
	}

	require.Contains(t, byLoser, "low")
	require.Contains(t, byLoser, "mid")
	assert.NotContains(t, byLoser, "high", "the final winner must not be flagged")

	assert.Contains(t, byLoser["low"], "HTTPRoute ns/high",
		"the loser's message must name the route that actually serves the pair")
	assert.NotContains(t, byLoser["low"], "HTTPRoute ns/mid",
		"naming an intermediate claimant that serves zero traffic misleads the operator")
	assert.Contains(t, byLoser["mid"], "HTTPRoute ns/high")
}

// TestDetectShadowedRules_MultiHostnameCrossProduct pins the (hostname × match)
// cross-product in ruleShadowKeys: a rule listing two hostnames collides with a
// lower-precedence rule on EACH shared hostname independently. Two hostnames
// both shadowed → two diagnostics.
func TestDetectShadowedRules_MultiHostnameCrossProduct(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{Hostnames: []string{"a.example.com", "b.example.com"}, Matches: []proxy.RouteMatch{pathExactMatch("/v1")}},
			{Hostnames: []string{"a.example.com", "b.example.com"}, Matches: []proxy.RouteMatch{pathExactMatch("/v1")}},
		},
		Provenance: []proxy.RuleProvenance{
			prov("HTTPRoute", "ns", "winner", shadowT0, 0),
			prov("HTTPRoute", "ns", "loser", shadowT1, 0),
		},
	}

	diags := proxy.DetectShadowedRules(cfg)
	require.Len(t, diags, 2, "each shadowed hostname yields its own diagnostic")

	joined := diags[0].Message + " " + diags[1].Message
	assert.Contains(t, joined, "a.example.com")
	assert.Contains(t, joined, "b.example.com")
}

// TestDetectShadowedRules_WildcardVsWildcardCollides pins a positive
// wildcard-vs-wildcard case: two rules claiming the SAME wildcard pattern
// collide (the pattern is the bucket key), unlike wildcard-vs-exact which does
// not.
func TestDetectShadowedRules_WildcardVsWildcardCollides(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{Hostnames: []string{"*.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
			{Hostnames: []string{"*.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
		},
		Provenance: []proxy.RuleProvenance{
			prov("HTTPRoute", "ns", "winner", shadowT0, 0),
			prov("HTTPRoute", "ns", "loser", shadowT1, 0),
		},
	}

	diags := proxy.DetectShadowedRules(cfg)
	require.Len(t, diags, 1)
	assert.Equal(t, "loser", diags[0].Name)
}

// TestDetectShadowedRules_HeaderOrderNormalized pins the canonical match key:
// the same header set listed in a different order is the SAME match and must
// collide.
func TestDetectShadowedRules_HeaderOrderNormalized(t *testing.T) {
	t.Parallel()

	matchAB := proxy.RouteMatch{
		Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"},
		Headers: []proxy.HeaderMatch{
			{Type: proxy.HeaderMatchExact, Name: "X-A", Value: "1"},
			{Type: proxy.HeaderMatchExact, Name: "X-B", Value: "2"},
		},
	}
	matchBA := proxy.RouteMatch{
		Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"},
		Headers: []proxy.HeaderMatch{
			{Type: proxy.HeaderMatchExact, Name: "x-b", Value: "2"},
			{Type: proxy.HeaderMatchExact, Name: "x-a", Value: "1"},
		},
	}

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{Hostnames: []string{"a.example.com"}, Matches: []proxy.RouteMatch{matchAB}},
			{Hostnames: []string{"a.example.com"}, Matches: []proxy.RouteMatch{matchBA}},
		},
		Provenance: []proxy.RuleProvenance{
			prov("HTTPRoute", "ns", "winner", shadowT0, 0),
			prov("HTTPRoute", "ns", "loser", shadowT1, 0),
		},
	}

	diags := proxy.DetectShadowedRules(cfg)
	assert.Len(t, diags, 1, "header order and name case must not defeat the collision detection")
}

// TestDetectShadowedRules_CrossKindBasis pins the honest basis label for the
// only remaining tie: an HTTPRoute rule beats an equal-precedence GRPCRoute
// rule purely because HTTP rules precede gRPC rules in the generated config.
func TestDetectShadowedRules_CrossKindBasis(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{Hostnames: []string{"grpc.example.com"}, Matches: []proxy.RouteMatch{pathExactMatch("/pkg.Svc/Method")}},
			{Hostnames: []string{"grpc.example.com"}, Matches: []proxy.RouteMatch{pathExactMatch("/pkg.Svc/Method")}},
		},
		Provenance: []proxy.RuleProvenance{
			// Same timestamp; the HTTPRoute sorts alphabetically AFTER the
			// GRPCRoute, so neither timestamp nor name explains the win — only
			// the HTTP-before-gRPC flattening order does.
			prov("HTTPRoute", "ns", "zz-http", shadowT0, 0),
			prov("GRPCRoute", "ns", "aa-grpc", shadowT0, 0),
		},
	}

	diags := proxy.DetectShadowedRules(cfg)
	require.Len(t, diags, 1)
	assert.Equal(t, "aa-grpc", diags[0].Name)
	assert.Contains(t, diags[0].Message, "HTTPRoute rules precede GRPCRoute rules")
}

// TestDetectShadowedRules_UnavailableRuleStillClaims pins that a fail-closed
// rule (UnavailableStatus != 0) still claims its keys: it matches requests and
// answers them (with an error), so a later identical rule is still shadowed.
func TestDetectShadowedRules_UnavailableRuleStillClaims(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{Hostnames: []string{"a.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}, UnavailableStatus: 500},
			{Hostnames: []string{"a.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
		},
		Provenance: []proxy.RuleProvenance{
			prov("HTTPRoute", "ns", "broken-winner", shadowT0, 0),
			prov("HTTPRoute", "ns", "loser", shadowT1, 0),
		},
	}

	diags := proxy.DetectShadowedRules(cfg)
	require.Len(t, diags, 1)
	assert.Equal(t, "loser", diags[0].Name)
}

// TestDetectShadowedRules_NoProvenanceNoPanic pins defensive behaviour: a
// config without provenance (e.g. hand-built in tests) yields no diagnostics
// instead of panicking on the parallel-slice access.
func TestDetectShadowedRules_NoProvenanceNoPanic(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{Hostnames: []string{"a.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
			{Hostnames: []string{"a.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
		},
	}

	assert.Empty(t, proxy.DetectShadowedRules(cfg))
}
