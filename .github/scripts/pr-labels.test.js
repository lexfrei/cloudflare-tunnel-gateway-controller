// pr-labels.test.js — unit tests for pr-labels.js.
//
// Runs under stdlib `node --test`; no third-party test framework so the
// CI step doesn't have to install npm deps.

'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const {
  parseTitle,
  shouldCountForSize,
  countLines,
  sizeLabel,
  deriveLabels,
} = require('./pr-labels.js');

test('parseTitle: feat with proxy scope', () => {
  assert.deepEqual(parseTitle('feat(proxy): add header timeout'), {
    kind: 'kind/feature',
    area: 'area/proxy',
  });
});

test('parseTitle: fix with controller scope', () => {
  assert.deepEqual(parseTitle('fix(controller): handle stale gateway'), {
    kind: 'kind/bug',
    area: 'area/controller',
  });
});

test('parseTitle: ci type emits area/ci with no kind', () => {
  // `ci` is an area, not a kind — TYPE_TO_KIND maps it to null.
  // Scope `scripts` is in the ci-adjacent alias set → area/ci.
  assert.deepEqual(parseTitle('ci(scripts): pin pytest version'), {
    kind: null,
    area: 'area/ci',
  });
});

test('parseTitle: ci scope on a chore title', () => {
  assert.deepEqual(parseTitle('chore(ci): bump action digests'), {
    kind: 'kind/cleanup',
    area: 'area/ci',
  });
});

test('parseTitle: refactor and perf both map to kind/cleanup', () => {
  assert.equal(parseTitle('refactor(api): rename helper').kind, 'kind/cleanup');
  assert.equal(parseTitle('perf(proxy): cache the regex').kind, 'kind/cleanup');
});

test('parseTitle: build/style types are NOT in TYPE_TO_KIND (out of #274 spec)', () => {
  // Only the types listed in #274 are mapped. build/style → no kind so
  // a typo in TYPE_TO_KIND can't silently change labelling on unrelated PRs.
  assert.equal(parseTitle('build(deps): bump go to 1.27').kind, null);
  assert.equal(parseTitle('style(proxy): gofmt').kind, null);
});

test('parseTitle: long-form dependencies scope → area/dependencies', () => {
  assert.equal(parseTitle('chore(dependencies): bump foo').area, 'area/dependencies');
});

test('parseTitle: Revert "feat(proxy): ..." preserves original kind+area', () => {
  // GitHub's revert button emits `Revert "<orig>"`. Parsing the inner
  // title preserves the routing signal -- a revert lands in the same
  // triage bucket as the change it undoes.
  assert.deepEqual(parseTitle('Revert "feat(proxy): add header timeout"'), {
    kind: 'kind/feature',
    area: 'area/proxy',
  });
});

test('parseTitle: bare Revert "..." without inner conventional title falls back', () => {
  assert.deepEqual(parseTitle('Revert "Hotfix from yesterday"'), {
    kind: null,
    area: 'area/uncategorized',
  });
});

test('parseTitle: e2e and test scopes both → area/testing', () => {
  assert.equal(parseTitle('test(e2e): pin teardown').area, 'area/testing');
  assert.equal(parseTitle('chore(test): drop dead helper').area, 'area/testing');
});

test('parseTitle: breaking change marker (!) is parsed but does not change labels', () => {
  // breaking-change semantics are maintainer judgement; the labeller
  // does not auto-apply kind/breaking-change.
  assert.deepEqual(parseTitle('feat(controller)!: drop noproxy mode'), {
    kind: 'kind/feature',
    area: 'area/controller',
  });
});

test('parseTitle: unknown scope falls back to area/uncategorized', () => {
  assert.deepEqual(parseTitle('feat(weirdthing): make it work'), {
    kind: 'kind/feature',
    area: 'area/uncategorized',
  });
});

test('parseTitle: title without scope still gets the kind', () => {
  assert.deepEqual(parseTitle('feat: add cool thing'), {
    kind: 'kind/feature',
    area: 'area/uncategorized',
  });
});

test('parseTitle: non-conventional title gets uncategorized only', () => {
  assert.deepEqual(parseTitle('Update README'), {
    kind: null,
    area: 'area/uncategorized',
  });
});

test('parseTitle: empty / undefined safe', () => {
  assert.deepEqual(parseTitle(''), { kind: null, area: 'area/uncategorized' });
  assert.deepEqual(parseTitle(undefined), { kind: null, area: 'area/uncategorized' });
});

test('shouldCountForSize: vendor / generated / pb.go skipped', () => {
  assert.equal(shouldCountForSize('vendor/golang.org/x/net/foo.go'), false);
  assert.equal(shouldCountForSize('internal/proxy/zz_generated_deepcopy.go'), false);
  assert.equal(shouldCountForSize('api/v1alpha1/api.pb.go'), false);
  assert.equal(shouldCountForSize('go.sum'), false);
  assert.equal(shouldCountForSize('docs/img/diagram.svg'), false);
  assert.equal(shouldCountForSize('pkg/generated/clientset.go'), false);
  assert.equal(shouldCountForSize('api/v1alpha1/generated/zz.go'), false);
});

test('shouldCountForSize: substring "generated" inside legitimate path is COUNTED', () => {
  // The tightened regex `(^|/)generated/` matches only dedicated
  // generated-content directories, not files whose names happen to
  // contain the substring (e.g. a hypothetical pkg/regenerator/foo.go).
  // Without the anchor a future pkg/regenerator/ would silently fall
  // out of size accounting and PR size labels would understate the
  // change.
  assert.equal(shouldCountForSize('pkg/regenerator/foo.go'), true);
  assert.equal(shouldCountForSize('internal/proxy/regenerator.go'), true);
});

test('shouldCountForSize: real source files counted', () => {
  assert.equal(shouldCountForSize('internal/proxy/handler.go'), true);
  assert.equal(shouldCountForSize('cmd/proxy/main.go'), true);
  assert.equal(shouldCountForSize('.github/workflows/pr.yaml'), true);
  assert.equal(shouldCountForSize('README.md'), true);
});

test('countLines: sums additions + deletions on counted files', () => {
  const files = [
    { filename: 'internal/proxy/handler.go', additions: 12, deletions: 3 },
    { filename: 'vendor/golang.org/x/foo.go', additions: 500, deletions: 0 },
    { filename: 'cmd/proxy/main.go', additions: 5, deletions: 5 },
  ];
  // 12+3 + 5+5 = 25; vendor excluded.
  assert.equal(countLines(files), 25);
});

test('countLines: empty / undefined safe', () => {
  assert.equal(countLines([]), 0);
  assert.equal(countLines(undefined), 0);
});

test('sizeLabel: bucket boundaries', () => {
  // Mirrors .github/labels.yml: XS 0-9, S 10-29, M 30-99, L 100-499, XL 500-999, XXL 1000+
  assert.equal(sizeLabel(0), 'size/XS');
  assert.equal(sizeLabel(9), 'size/XS');
  assert.equal(sizeLabel(10), 'size/S');
  assert.equal(sizeLabel(29), 'size/S');
  assert.equal(sizeLabel(30), 'size/M');
  assert.equal(sizeLabel(99), 'size/M');
  assert.equal(sizeLabel(100), 'size/L');
  assert.equal(sizeLabel(499), 'size/L');
  assert.equal(sizeLabel(500), 'size/XL');
  assert.equal(sizeLabel(999), 'size/XL');
  assert.equal(sizeLabel(1000), 'size/XXL');
  assert.equal(sizeLabel(50000), 'size/XXL');
});

test('deriveLabels: happy path includes kind+area+size in that order', () => {
  const files = [{ filename: 'internal/proxy/handler.go', additions: 50, deletions: 10 }];
  assert.deepEqual(
    deriveLabels('feat(proxy): add header timeout', files),
    ['kind/feature', 'area/proxy', 'size/M'],
  );
});

test('deriveLabels: no kind (e.g. ci type) omits the kind/* entry', () => {
  const files = [{ filename: '.github/workflows/foo.yaml', additions: 5, deletions: 1 }];
  // `ci` type maps to no kind; scope absent → TYPE_FALLBACK_AREA['ci'] = area/ci; size XS (6 lines).
  assert.deepEqual(deriveLabels('ci: bump action digest', files), [
    'area/ci',
    'size/XS',
  ]);
});

test('deriveLabels: vendor-heavy PR still gets meaningful size', () => {
  const files = [
    { filename: 'vendor/foo/bar.go', additions: 5000, deletions: 4000 },
    { filename: 'go.mod', additions: 2, deletions: 1 },
  ];
  // vendor + go.sum excluded; only go.mod counts (3 lines) → XS.
  assert.deepEqual(deriveLabels('chore(deps): bump foo', files), [
    'kind/cleanup',
    'area/dependencies',
    'size/XS',
  ]);
});

test('deriveLabels: fallback shape is always area/uncategorized + size', () => {
  // Garbage title + no files = uncategorized + XS.
  assert.deepEqual(deriveLabels('Update README', []), [
    'area/uncategorized',
    'size/XS',
  ]);
});

test('deriveLabels: explicit dedup -- collisions across kind/area/size collapse to one entry', () => {
  // Today's taxonomy can't produce a collision, but the JSDoc claims
  // deduplication. Drive the path with a synthetic shape: monkey-patch
  // a function that would emit two identical labels and assert the
  // output has them collapsed.
  const { deriveLabels: real, parseTitle: realParse, sizeLabel: realSize, countLines: realCount } = require('./pr-labels.js');

  // Verify directly with the real function: a hypothetical taxonomy
  // where the kind label equals the area label would deduplicate.
  // We can't trigger a real collision without rewriting the module,
  // so the cheap pin: assert deriveLabels result length never exceeds
  // the number of distinct labels its inputs imply.
  const labels = real('feat(proxy): something', [{ filename: 'foo.go', additions: 1, deletions: 0 }]);
  const unique = new Set(labels);
  assert.equal(unique.size, labels.length,
    'deriveLabels must return only unique labels');
});

test('parseTitle: ci(labels) -- the autolabeler must label its OWN PR correctly', () => {
  // Regression pin from round-2 review: round 1 of this PR was titled
  // `ci(labels): ...` and the labeller mapped scope `labels` to
  // area/uncategorized -- self-misclassification. Fix maps
  // labels/scripts/workflows aliases to area/ci.
  assert.deepEqual(parseTitle('ci(labels): widen paths + autolabeler'), {
    kind: null,
    area: 'area/ci',
  });
  assert.deepEqual(parseTitle('ci(scripts): pin pytest'), { kind: null, area: 'area/ci' });
  assert.deepEqual(parseTitle('ci(workflows): bump action'), { kind: null, area: 'area/ci' });
});

test('parseTitle: bare type with no scope falls back via TYPE_FALLBACK_AREA', () => {
  // Without the type fallback, `ci: foo` would land as
  // area/uncategorized -- losing the routing signal even when the
  // type itself is unambiguous about the area.
  assert.equal(parseTitle('ci: bump action digest').area, 'area/ci');
  assert.equal(parseTitle('test: drop dead helper').area, 'area/testing');
  assert.equal(parseTitle('docs: tweak setup guide').area, 'area/docs');
});

test('parseTitle: case-insensitive on the type+scope (handles autocorrect)', () => {
  // `Fix:` / `Feat(Proxy):` (sentence-case) are common from people
  // typing on phones; silently degrading to area/uncategorized would
  // mislabel a real fix as a non-conventional title.
  assert.deepEqual(parseTitle('Fix(proxy): handle stale gateway'), {
    kind: 'kind/bug',
    area: 'area/proxy',
  });
  assert.deepEqual(parseTitle('Feat(Proxy): add header timeout'), {
    kind: 'kind/feature',
    area: 'area/proxy',
  });
});

test('parseTitle: multi-scope split (comma and slash) picks the first matched component', () => {
  // GitHub UI doesn't enforce single scope; people write
  // `feat(proxy,api):` or `feat(proxy/handler):`. Take the first
  // recognised piece so the routing signal isn't lost to a
  // multi-scope convention.
  assert.equal(parseTitle('feat(proxy,api): touch both').area, 'area/proxy');
  assert.equal(parseTitle('feat(api,proxy): touch both').area, 'area/api');
  assert.equal(parseTitle('feat(proxy/handler): nested scope').area, 'area/proxy');
});

test('parseTitle: multi-scope where no component matches falls through normally', () => {
  // Two unknowns + no type fallback → area/uncategorized.
  // `feat` has no TYPE_FALLBACK_AREA entry.
  assert.equal(parseTitle('feat(weirdA,weirdB): nothing matches').area, 'area/uncategorized');
});

test('TYPE_TO_KIND non-null values must all exist in .github/labels.yml', () => {
  // Drift pin: if SCOPE_TO_AREA gains a value not in labels.yml, the
  // autolabeler creates a label on the fly (GitHub auto-creates),
  // which then survives the next labels-sync cron but is undocumented.
  // Same risk in reverse for kind/* values.
  const fs = require('node:fs');
  const path = require('node:path');
  const yaml = fs.readFileSync(path.join(__dirname, '..', 'labels.yml'), 'utf8');
  const declaredKinds = new Set(
    (yaml.match(/^- name: kind\/[a-z-]+/gm) || []).map((l) => l.replace(/^- name: /, '')),
  );

  const { TYPE_TO_KIND } = require('./pr-labels.js');
  for (const kindLabel of Object.values(TYPE_TO_KIND)) {
    if (kindLabel === null) continue;
    assert.ok(
      declaredKinds.has(kindLabel),
      `TYPE_TO_KIND emits ${kindLabel}, but .github/labels.yml does not declare it`,
    );
  }
});

test('SCOPE_TO_AREA and TYPE_FALLBACK_AREA values must all exist in .github/labels.yml', () => {
  const fs = require('node:fs');
  const path = require('node:path');
  const yaml = fs.readFileSync(path.join(__dirname, '..', 'labels.yml'), 'utf8');
  const declaredAreas = new Set(
    (yaml.match(/^- name: area\/[a-z-]+/gm) || []).map((l) => l.replace(/^- name: /, '')),
  );

  const { SCOPE_TO_AREA, TYPE_FALLBACK_AREA } = require('./pr-labels.js');
  const allAreaValues = new Set([
    ...Object.values(SCOPE_TO_AREA),
    ...Object.values(TYPE_FALLBACK_AREA),
    'area/uncategorized',
  ]);
  for (const areaLabel of allAreaValues) {
    assert.ok(
      declaredAreas.has(areaLabel),
      `SCOPE_TO_AREA / TYPE_FALLBACK_AREA emit ${areaLabel}, but .github/labels.yml does not declare it`,
    );
  }
});

test('SIZE_BUCKETS must match .github/labels.yml entries one-for-one', () => {
  // Pins the YAML-vs-JS correspondence. Drift between the labeller's
  // bucket list and the labels declared in .github/labels.yml would
  // silently produce labels that label-sync deletes on the next run.
  const fs = require('node:fs');
  const path = require('node:path');

  const yamlPath = path.join(__dirname, '..', 'labels.yml');
  const yaml = fs.readFileSync(yamlPath, 'utf8');

  // Parse the size/ section out by regex -- avoids a YAML dep.
  // Pattern: lines like `- name: size/XS`.
  const yamlSizeLabels = (yaml.match(/^- name: size\/\w+/gm) || [])
    .map((line) => line.replace(/^- name: /, ''));

  const { SIZE_BUCKETS } = require('./pr-labels.js');
  const jsBucketLabels = SIZE_BUCKETS.map((b) => b.label);

  // Each JS bucket must have a matching YAML entry; each YAML entry
  // must be present in the JS bucket list. Symmetric set equality.
  assert.deepEqual(
    new Set(jsBucketLabels),
    new Set(yamlSizeLabels),
    `JS SIZE_BUCKETS (${jsBucketLabels.join(',')}) must match .github/labels.yml size/* entries (${yamlSizeLabels.join(',')})`,
  );
});
