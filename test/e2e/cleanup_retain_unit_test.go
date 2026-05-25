//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestShouldSkipCleanupOnFailure pins the truth table of the cleanup-skip
// predicate. The two-input AND is small enough that the temptation is to
// inline it; this test exists so that flipping the polarity (e.g. "skip
// on success" by accident, or "skip whenever the env is set even on
// green runs") gets caught immediately. CI accumulating dangling
// HTTPRoutes across runs would be a silent multi-hour debugging trap;
// the fast feedback here is cheap insurance.
func TestShouldSkipCleanupOnFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		failed   bool
		envValue string
		want     bool
	}{
		{
			name:     "green run with env unset cleans up",
			failed:   false,
			envValue: "",
			want:     false,
		},
		{
			name:     "green run with env set still cleans up",
			failed:   false,
			envValue: "1",
			want:     false,
		},
		{
			name:     "red run with env unset cleans up (CI default)",
			failed:   true,
			envValue: "",
			want:     false,
		},
		{
			name:     "red run with env set retains for post-mortem",
			failed:   true,
			envValue: "1",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := shouldSkipCleanupOnFailure(tt.failed, tt.envValue)
			if got != tt.want {
				t.Fatalf("shouldSkipCleanupOnFailure(failed=%v, env=%q) = %v, want %v",
					tt.failed, tt.envValue, got, tt.want)
			}
		})
	}
}

// TestEnvVarDocumented pins the cross-link between the constant and
// docs/development/testing.md. A maintainer who hits a flaky run looks
// at the testing-doc env-var table first; if the knob isn't listed
// there it might as well not exist for the operator. Renaming or
// dropping the constant without also updating the doc would silently
// break that contract.
//
// Asserts two things: (a) the file contains an `### E2E Environment
// Variables` heading, (b) the env var literal appears as the first
// cell of a markdown table row beneath that heading (`| NAME |`
// shape). A stray prose mention of the env var name elsewhere in the
// doc would not satisfy the second check -- that's intentional: a
// loose substring test would silently let the table row disappear.
//
// The path is resolved relative to this test file's package
// (test/e2e), which always sits two levels below the docs directory.
func TestEnvVarDocumented(t *testing.T) {
	t.Parallel()

	const (
		docsPath    = "../../docs/development/testing.md"
		sectionHead = "### E2E Environment Variables"
	)

	data, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatalf("read %s: %v", docsPath, err)
	}

	content := string(data)

	sectionIdx := strings.Index(content, sectionHead)
	if sectionIdx < 0 {
		t.Fatalf("%q heading not found in %s; the E2E env-var table moved or was deleted",
			sectionHead, docsPath)
	}

	// Bound the search to the section that follows the heading -- the
	// next heading at the same or higher level ends the slice.
	tail := content[sectionIdx+len(sectionHead):]
	if end := strings.Index(tail, "\n### "); end >= 0 {
		tail = tail[:end]
	}

	cellMarker := "| `" + skipCleanupEnvVar + "` |"
	if !strings.Contains(tail, cellMarker) {
		t.Fatalf("env var %q is not present as a table row (looked for %q) under %q in %s",
			skipCleanupEnvVar, cellMarker, sectionHead, docsPath)
	}

	// The per-subtest predicate gates a namespace-wide wipe, so a
	// passing sibling running AFTER the failing subtest deletes the
	// retained state. The doc row must call this out -- otherwise an
	// operator running the full suite will think the env knob is
	// broken when state silently disappears between failure and
	// inspection. Pin the `-run` recommendation literally so a
	// rewrite that drops it gets caught.
	const isolationHint = "`-run TestName/SubtestName` to isolate"
	if !strings.Contains(tail, isolationHint) {
		t.Fatalf("docs row for %q must mention %q so operators know to isolate failing subtests; %s missing the caveat",
			skipCleanupEnvVar, isolationHint, docsPath)
	}
}

// deferObservationHelperEnv directs the test binary into the
// subprocess-helper branch of TestSkipCleanupOnFailure_DeferredObservation.
// The helper deliberately marks a child *testing.T as failed and writes
// its observations to stdout, so the parent can pin the contract without
// the parent process itself reporting a failed test.
const deferObservationHelperEnv = "E2E_DEFER_OBSERVATION_HELPER"

// TestSkipCleanupOnFailure_DeferredObservation pins the production
// contract that `defer skipCleanupOnFailure(t)` placed in a real
// subtest correctly observes the sticky failure bit. The pure-predicate
// test above exercises the AND, but the real-world surface is "Go
// runtime + *testing.T + defer + require/t.Fail" -- a refactor that,
// say, evaluates t.Failed() at registration time instead of at the
// defer's fire moment would silently break the suite without this
// test.
//
// Uses the subprocess re-entry pattern documented in CLAUDE.md: the
// child process runs the same test name with deferObservationHelperEnv
// set, executes the helper branch, and exits with whatever code the
// deliberately-failing subtest produced. The parent ignores that exit
// code and asserts only on the captured stdout markers, so this test
// is itself a clean pass even though it contains a forced failure.
// tparallel is intentionally disabled for this test: the subtests
// `passing-child` and `failing-child` must run sequentially. Capturing
// passing-child's observation before failing-child marks the parent
// failed is what proves the env-gated retain isn't accidentally
// triggered on a green subtest -- making them parallel would race the
// observation and break the contract this test pins.
//
//nolint:tparallel // sequential execution is load-bearing; see above
func TestSkipCleanupOnFailure_DeferredObservation(t *testing.T) {
	t.Parallel()

	if os.Getenv(deferObservationHelperEnv) != "" {
		runDeferObservationHelper(t)

		return
	}

	// 60s gives a cold-start CI runner room while still bounding a
	// genuine hang. Locally the subprocess finishes in ~10ms.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=TestSkipCleanupOnFailure_DeferredObservation/",
		"-test.v")
	cmd.Env = append(os.Environ(),
		deferObservationHelperEnv+"=1",
		skipCleanupEnvVar+"=1")

	// CombinedOutput returns a non-nil error because the child's
	// failing-child subtest deliberately fails, producing a non-zero
	// exit code. The exit code is the whole point of this test, so
	// we ignore the error and assert only on the captured markers.
	out, _ := cmd.CombinedOutput()
	output := string(out)

	if !strings.Contains(output, "DEFER_OBSERVED_PASSING=false") {
		t.Fatalf("expected deferred cleanup on a passing subtest NOT to skip "+
			"(env set but t.Failed()==false); helper output:\n%s", output)
	}

	if !strings.Contains(output, "DEFER_OBSERVED_FAILING=true") {
		t.Fatalf("expected deferred cleanup on a failing subtest to skip "+
			"(env set AND t.Failed()==true); helper output:\n%s", output)
	}

	// Regression guard for the no-sibling-cascade invariant: each
	// t.Run gets a fresh *testing.T whose Failed() reports only that
	// subtest's failures, never an earlier sibling's. A refactor that
	// flipped the predicate to walk to the parent would make this
	// observation flip to true and break the documented "retention is
	// per-failing-subtest" contract without any other test catching
	// it.
	if !strings.Contains(output, "DEFER_OBSERVED_AFTER_FAILING_SIBLING=false") {
		t.Fatalf("expected a passing subtest running AFTER a failing sibling to NOT skip "+
			"cleanup (siblings don't inherit failure); helper output:\n%s", output)
	}
}

// runDeferObservationHelper is the child-process branch. It runs
// three sibling subtests in order:
//
//  1. passing-child -- proves a green subtest's defer does NOT skip
//     even with the env set.
//  2. failing-child -- proves a red subtest's defer DOES skip with
//     the env set.
//  3. passing-after-failing-sibling -- proves siblings don't
//     inherit failure: even though (2) failed the parent, a fresh
//     sibling t.Run gets t.Failed() == false in its own *T and
//     cleans up normally.
//
// Each defer captures its observation via skipCleanupOnFailure into
// a closure variable, then the helper writes a "KEY=value" marker
// to stdout for the parent to grep. Output is centralised in
// writeObservation so the print form is in one place.
func runDeferObservationHelper(t *testing.T) {
	t.Helper()

	var observedPassing bool

	t.Run("passing-child", func(t *testing.T) {
		defer func() { observedPassing = skipCleanupOnFailure(t) }()
		// Intentionally do nothing; the subtest passes.
	})

	writeObservation("DEFER_OBSERVED_PASSING", observedPassing)

	var observedFailing bool

	t.Run("failing-child", func(t *testing.T) {
		defer func() { observedFailing = skipCleanupOnFailure(t) }()
		// t.Fail (not t.Fatal) marks failure without aborting the
		// closure, so the defer still runs against the failed bit.
		t.Fail()
	})

	writeObservation("DEFER_OBSERVED_FAILING", observedFailing)

	var observedAfterFailingSibling bool

	t.Run("passing-after-failing-sibling", func(t *testing.T) {
		defer func() { observedAfterFailingSibling = skipCleanupOnFailure(t) }()
		// Intentionally do nothing; siblings don't inherit failure.
	})

	writeObservation("DEFER_OBSERVED_AFTER_FAILING_SIBLING", observedAfterFailingSibling)
}

// writeObservation writes a single `KEY=value` marker line to the
// child process's stdout. Centralised so the marker print form
// lives in one place and the call sites stay short.
func writeObservation(key string, value bool) {
	_, _ = fmt.Fprintf(os.Stdout, "%s=%v\n", key, value)
}
