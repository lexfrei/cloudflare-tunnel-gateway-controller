// pr-labels.js — Conventional-Commit-title-driven label derivation for PRs.
//
// Exported pure functions consumed by .github/workflows/pr-labels.yaml's
// actions/github-script step AND by pr-labels.test.js. No GitHub API or
// fs side effects live in this file — the workflow wires the inputs and
// applies the outputs. This split keeps the mapping unit-testable via
// stdlib `node --test` without spinning up a GitHub API mock.
//
// Design intent matches issue #274:
//   - kind/area/size labels are auto-applied on PR open/synchronize.
//   - Never removes labels a human applied — only adds the derived set.
//     The workflow uses github.rest.issues.addLabels, which is additive.
//   - Conventional Commits title format is the single source of truth for
//     kind + area. Diff stats are the source of truth for size.

'use strict';

// Conventional Commit type → kind/* label.
// Some types intentionally map to NO kind (`null`) because the matching
// area covers them: `test` → area/testing (testing is an area, not a kind),
// `ci` → area/ci. Leaving kind null means addLabels won't fight an
// already-applied manual kind/* label.
const TYPE_TO_KIND = Object.freeze({
  fix: 'kind/bug',
  feat: 'kind/feature',
  docs: 'kind/documentation',
  test: null,
  chore: 'kind/cleanup',
  refactor: 'kind/cleanup',
  ci: null,
  perf: 'kind/cleanup',
});

// Conventional Commit scope → area/* label.
// Must match the area/* set declared in .github/labels.yml; unmatched
// scopes fall through to area/uncategorized, which is the explicit
// fallback declared there.
const SCOPE_TO_AREA = Object.freeze({
  proxy: 'area/proxy',
  controller: 'area/controller',
  tunnel: 'area/tunnel',
  api: 'area/api',
  helm: 'area/helm',
  ci: 'area/ci',
  // ci-adjacent scope aliases -- a title like `ci(labels):` or
  // `ci(scripts):` is plumbing for the CI/automation surface, so
  // collapse them onto area/ci. Without this the autolabeler
  // misclassifies its OWN PR as area/uncategorized.
  labels: 'area/ci',
  scripts: 'area/ci',
  workflows: 'area/ci',
  workflow: 'area/ci',
  docs: 'area/docs',
  e2e: 'area/testing',
  test: 'area/testing',
  testing: 'area/testing',
  deps: 'area/dependencies',
  dependencies: 'area/dependencies',
});

// type → area fallback for cases where the scope is missing or
// unmapped but the type itself implies the area (`ci: foo` is plumbing,
// `test: foo` is testing infra, `docs: foo` is docs). Used only when
// SCOPE_TO_AREA didn't match -- explicit scope still wins.
const TYPE_FALLBACK_AREA = Object.freeze({
  ci: 'area/ci',
  test: 'area/testing',
  docs: 'area/docs',
});

// File paths excluded from size counting.
// vendor/ and *generated*.go are vendored / generated content — counting
// them would push every dep bump into size/XXL. *.svg / *.png skipped
// because binary diffs are not meaningful "lines changed".
// Patterns are anchored so a future pkg/regenerator/ doesn't silently
// disappear from size accounting. zz_generated_ catches kubebuilder
// deepcopy artifacts; the (^|/)generated/ form catches dedicated
// generated-content directories without matching "regenerator" or
// other prefixes that happen to contain the substring.
const SIZE_EXCLUDE_PATTERNS = Object.freeze([
  /^vendor\//,
  /(^|\/)generated\//,
  /zz_generated_/,
  /\.pb\.go$/,
  /\.svg$/,
  /\.png$/,
  /^go\.sum$/,
]);

// Size bucket thresholds mirror .github/labels.yml exactly.
const SIZE_BUCKETS = Object.freeze([
  { max: 9, label: 'size/XS' },
  { max: 29, label: 'size/S' },
  { max: 99, label: 'size/M' },
  { max: 499, label: 'size/L' },
  { max: 999, label: 'size/XL' },
  { max: Infinity, label: 'size/XXL' },
]);

// Title regex: type(scope)!: subject. Scope and `!` are optional. The
// breaking-change `!` is recognised but does NOT alter the label set —
// breaking-change semantics belong on the maintainer side.
const TITLE_RE = /^(?<type>[a-z]+)(?:\((?<scope>[^)]+)\))?!?:\s+/;

// GitHub's "Revert" button produces titles of the form `Revert "<orig>"`.
// Parse the inner title so the original kind/area is preserved -- a
// revert PR otherwise lands as area/uncategorized with no kind, which
// loses the routing signal for triage.
const REVERT_RE = /^Revert\s+"(.+)"\s*$/;

/**
 * Parse a Conventional Commit title and return derived kind/area labels.
 * @param {string} title
 * @returns {{kind: string|null, area: string}} Empty area falls back to
 *   area/uncategorized so every PR gets exactly one area/* label.
 */
function parseTitle(title) {
  const trimmed = (title || '').trim();
  const revertMatch = trimmed.match(REVERT_RE);
  const target = revertMatch ? revertMatch[1] : trimmed;

  // Lowercase the type token before regex match so common autocorrect
  // forms like `Fix:` / `Feat(proxy):` don't silently degrade to
  // kind:null + area/uncategorized. Scope is lowercased separately.
  // Only lowercase up to the first `:` to keep the body case-preserving
  // (not that we use it, but defensive).
  const colonIdx = target.indexOf(':');
  const head = colonIdx >= 0 ? target.slice(0, colonIdx).toLowerCase() : target.toLowerCase();
  const tail = colonIdx >= 0 ? target.slice(colonIdx) : '';
  const normalised = head + tail;

  const match = normalised.match(TITLE_RE);

  if (!match) {
    return { kind: null, area: 'area/uncategorized' };
  }

  const type = match.groups.type;
  // Multi-scope split: `feat(proxy,api):` and `feat(proxy/handler):`
  // are common; try each component left-to-right and take the first
  // that matches SCOPE_TO_AREA. If none match, fall through to the
  // type-fallback / uncategorized chain.
  const rawScope = (match.groups.scope || '').trim().toLowerCase();
  const scopeParts = rawScope ? rawScope.split(/[,/]/).map((s) => s.trim()).filter(Boolean) : [];

  const kind = TYPE_TO_KIND[type] ?? null;

  let area;
  for (const part of scopeParts) {
    if (SCOPE_TO_AREA[part]) {
      area = SCOPE_TO_AREA[part];
      break;
    }
  }
  if (!area) {
    area = TYPE_FALLBACK_AREA[type] || 'area/uncategorized';
  }

  return { kind, area };
}

/**
 * Decide whether a file path counts toward the size bucket.
 * @param {string} path
 * @returns {boolean}
 */
function shouldCountForSize(path) {
  return !SIZE_EXCLUDE_PATTERNS.some((re) => re.test(path));
}

/**
 * Sum added + deleted lines across files, ignoring excluded paths.
 * @param {Array<{filename: string, additions: number, deletions: number}>} files
 * @returns {number}
 */
function countLines(files) {
  return (files || [])
    .filter((f) => shouldCountForSize(f.filename))
    .reduce((sum, f) => sum + (f.additions || 0) + (f.deletions || 0), 0);
}

/**
 * Map a line count to the matching size/* label.
 * @param {number} lines
 * @returns {string}
 */
function sizeLabel(lines) {
  const bucket = SIZE_BUCKETS.find((b) => lines <= b.max);
  return bucket.label;
}

/**
 * Compose the full label set for a PR.
 * @param {string} title
 * @param {Array} files
 * @returns {string[]} Deduplicated. Order: kind (if present), area, size.
 *   Today's mapping can't collide across kind/area/size namespaces, but
 *   the dedup is explicit so a future taxonomy reshuffle (e.g. moving
 *   `area/testing` into a `kind/test` shape) doesn't double-list a
 *   label and then trigger a spurious GitHub API error or audit-log
 *   noise.
 */
function deriveLabels(title, files) {
  const { kind, area } = parseTitle(title);
  const size = sizeLabel(countLines(files));

  const labels = [];
  if (kind) labels.push(kind);
  labels.push(area);
  labels.push(size);

  return [...new Set(labels)];
}

module.exports = {
  parseTitle,
  shouldCountForSize,
  countLines,
  sizeLabel,
  deriveLabels,
  // Exposed for direct test assertion on the contents of the constants.
  TYPE_TO_KIND,
  SCOPE_TO_AREA,
  TYPE_FALLBACK_AREA,
  SIZE_BUCKETS,
};
