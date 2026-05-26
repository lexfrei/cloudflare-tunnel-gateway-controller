//go:build e2e

package e2e

import (
	"os"
	"testing"
)

// skipCleanupEnvVar is the opt-in env knob: when set to any non-empty
// value AND the test failed, cleanup helpers in this package return
// early instead of deleting the test resources. Lets a maintainer keep
// the cluster state around for post-mortem inspection (`kubectl get
// httproute -A`, proxy log inspection, etc.) without manually fishing
// the right `--skip-cleanup` flag into every test.
//
// Scope of retention -- per-subtest predicate AND per-subtest
// resource isolation. Each `t.Run` receives a fresh `*testing.T`
// whose `Failed()` reports only that subtest's own failures, never
// a sibling's (pinned by runDeferObservationHelper); so the
// PREDICATE fires per-failing-subtest. The CLEANUP it gates --
// deleteAllRoutes -- now also filters by ownerLabelKey ==
// subtestLabelValue(t.Name()) so it only deletes the routes the
// current subtest created. Issue #265 closed the regression where
// a passing sibling's defer would wipe a failing subtest's
// retained routes.
//
// Operational consequence in a full-suite run:
//
//  1. Subtest A fails. A's defer skips (predicate=true). A's
//     routes stay in the namespace with owner=hash(A).
//  2. Subtest B runs next, creates its own routes with
//     owner=hash(B), passes.
//  3. B's defer runs (predicate=false because B passed) and
//     deleteAllRoutes filters by owner=hash(B); A's routes survive.
//
// So `E2E_SKIP_CLEANUP_ON_FAILURE` now preserves the failing
// subtest's state across the rest of the run without the operator
// having to pair it with `-run TestName/SubtestName`. Pairing still
// works (and is recommended if the failing subtest itself created
// many routes you want to triage one at a time), but is no longer
// load-bearing.
//
// Cross-invocation. Retention is scoped to a single `go test`
// process. The next invocation's clean-slate `wipeAllRoutesInNamespace`
// at the top of `TestHTTPRouteConformance` runs with the parent's
// `t.Failed() == false` and wipes anything left over regardless of
// owner -- intentional, so a follow-up run starts from a known
// state once the operator has finished inspecting. `kubectl`
// against the retained state must happen between the failing run
// and the next `go test` invocation.
//
// Documented in docs/development/testing.md under "E2E Environment
// Variables"; that doc row is asserted by TestEnvVarDocumented in
// this file so the knob can't silently drop out of the docs.
const skipCleanupEnvVar = "E2E_SKIP_CLEANUP_ON_FAILURE"

// shouldSkipCleanupOnFailure is the pure predicate behind
// skipCleanupOnFailure. Extracted so it can be unit-tested without
// having to artificially fail the surrounding test (which would also
// taint the harness state). Returns true only when BOTH conditions
// hold: the test recorded a failure AND the opt-in env var is set.
//
// Defaulting cleanup to "always run unless explicitly opted out"
// keeps CI from accumulating dangling cluster resources on flaky
// runs while still letting a developer who's actively debugging
// preserve the failed state.
func shouldSkipCleanupOnFailure(failed bool, skipFailEnv string) bool {
	return failed && skipFailEnv != ""
}

// skipCleanupOnFailure inspects the current test's failure status and
// the opt-in env var and reports whether the caller should retain
// resources for post-mortem. Logs the decision when retaining so the
// reason shows up in the test output next to the failure itself.
func skipCleanupOnFailure(t *testing.T) bool {
	t.Helper()

	if !shouldSkipCleanupOnFailure(t.Failed(), os.Getenv(skipCleanupEnvVar)) {
		return false
	}

	t.Logf("test failed and %s is set -- retaining resources for post-mortem inspection", skipCleanupEnvVar)

	return true
}
