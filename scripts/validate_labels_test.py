"""Unit tests for scripts/validate-labels.py.

Each rule has both a happy-path case (valid input → no errors) and at least
one failing case (invalid input → specific error message). Without these,
a regression that breaks one of the rules would silently pass the validate
job and let bad data through.

Run with: python3 -m pytest scripts/validate_labels_test.py
"""

from __future__ import annotations

import importlib.util
import pathlib

import pytest
import yaml

_VALIDATOR_PATH = pathlib.Path(__file__).parent / 'validate-labels.py'
_spec = importlib.util.spec_from_file_location('validate_labels', _VALIDATOR_PATH)
_module = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_module)
validate = _module.validate


def _label(name: str, **kwargs) -> dict:
    """Build a minimal valid label dict; tests override one field at a time."""
    return {
        'name': name,
        'color': 'aaaaaa',
        'description': 'short',
        **kwargs,
    }


def test_happy_path_no_errors():
    labels = [_label('kind/bug'), _label('priority/backlog')]
    assert validate(labels, existing_workflows=[]) == []


def test_rule1_description_too_long():
    long_desc = 'x' * 101
    labels = [_label('kind/bug', description=long_desc)]
    errs = validate(labels, existing_workflows=[])
    assert len(errs) == 1
    assert 'description 101 chars' in errs[0]


def test_rule1_description_exactly_100_ok():
    labels = [_label('kind/bug', description='x' * 100)]
    assert validate(labels, existing_workflows=[]) == []


def test_rule2_color_missing_hex_chars():
    labels = [_label('kind/bug', color='xyz123')]
    errs = validate(labels, existing_workflows=[])
    assert len(errs) == 1
    assert 'bad color' in errs[0]


def test_rule2_color_with_hash_prefix_rejected():
    labels = [_label('kind/bug', color='#aaaaaa')]
    errs = validate(labels, existing_workflows=[])
    assert len(errs) == 1


def test_rule3_duplicate_top_level_name():
    labels = [_label('kind/bug'), _label('kind/bug', color='ffffff')]
    errs = validate(labels, existing_workflows=[])
    # Note: rule 3 reports duplicate once, but rule 4/5 may also flag a
    # downstream effect; we only care that the duplicate is caught.
    assert any('duplicate name: kind/bug' in e for e in errs)


def test_rule4_alias_collides_with_top_level_name():
    labels = [
        _label('kind/bug', aliases=['kind/feature']),
        _label('kind/feature'),
    ]
    errs = validate(labels, existing_workflows=[])
    assert any('collides with a top-level name' in e for e in errs)


def test_rule5_alias_double_owned_across_labels():
    labels = [
        _label('triage/needs-information', aliases=['status/needs-info']),
        _label('triage/needs-triage', aliases=['status/needs-info']),
    ]
    errs = validate(labels, existing_workflows=[])
    assert any(
        "listed under both 'triage/needs-information'" in e
        and "'triage/needs-triage'" in e
        for e in errs
    )


def test_rule7_duplicate_alias_within_label():
    """Catches `aliases: ['x', 'x']` typos that rule 5 cannot see (the second
    entry silently overwrites in alias_owner)."""
    labels = [
        _label('kind/bug', aliases=['old-bug', 'old-bug']),
    ]
    errs = validate(labels, existing_workflows=[])
    assert any("alias 'old-bug' listed more than once" in e for e in errs)


def test_rule7_distinct_aliases_within_label_ok():
    labels = [
        _label('triage/needs-information', aliases=['status/needs-info', 'status/needs-design']),
    ]
    assert validate(labels, existing_workflows=[]) == []


def test_rule6_description_names_missing_workflow():
    labels = [
        _label(
            'kind/bug',
            description='auto-applied by .github/workflows/pr-size.yaml',
        ),
    ]
    errs = validate(labels, existing_workflows=['labels.yaml', 'pr.yaml'])
    assert len(errs) == 1
    assert 'references missing workflow .github/workflows/pr-size.yaml' in errs[0]


def test_rule6_description_names_existing_workflow_ok():
    labels = [
        _label(
            'Container Available',
            description=(
                'Container image built; auto-applied by '
                '.github/workflows/pr-privileged.yaml'
            ),
        ),
    ]
    assert validate(labels, existing_workflows=['pr-privileged.yaml']) == []


def test_rule6_description_with_yml_suffix_caught():
    """Tests both .yml and .yaml are recognised by the regex."""
    labels = [
        _label(
            'kind/bug',
            description='auto-applied by .github/workflows/ghost.yml',
        ),
    ]
    errs = validate(labels, existing_workflows=['real.yaml'])
    assert any('ghost.yml' in e for e in errs)


def test_multiple_rules_fire_simultaneously():
    """A single label may break multiple rules; all should surface."""
    labels = [
        _label('kind/bug', color='xyz', description='x' * 200),
    ]
    errs = validate(labels, existing_workflows=[])
    assert any('description 200 chars' in e for e in errs)
    assert any('bad color' in e for e in errs)


def test_empty_input_no_errors():
    assert validate([], existing_workflows=[]) == []


def test_real_labels_yml_consumers_reference_only_top_level_names():
    """Pins the migration's biggest hazard: every place outside labels.yml
    that names a label by string (issue templates, renovate.json, etc.)
    must reference a TOP-LEVEL label name. Referencing an alias would
    work today but break the moment the sync runs and the alias gets
    merged away. Referencing a name absent from labels.yml entirely is
    Renovate's special hazard -- it auto-creates the missing label with
    default color and no description, silently undoing the migration.

    Run from the repo root so the file paths resolve.
    """
    repo_root = pathlib.Path(__file__).parent.parent
    labels_file = repo_root / '.github' / 'labels.yml'

    with labels_file.open(encoding='utf-8') as handle:
        labels = yaml.safe_load(handle)

    top_level_names = {label['name'] for label in labels}

    referenced: list[tuple[str, str]] = []  # (source, label_name)

    # Issue templates: parse the YAML front-matter `labels: a, b` line.
    template_dir = repo_root / '.github' / 'ISSUE_TEMPLATE'
    if template_dir.is_dir():
        for template in sorted(template_dir.glob('*.md')):
            text = template.read_text(encoding='utf-8')
            for line in text.splitlines():
                if line.startswith('labels:'):
                    names = [n.strip() for n in line.split(':', 1)[1].split(',')]
                    for name in names:
                        if name:
                            referenced.append((str(template.relative_to(repo_root)), name))
                    break

    # renovate.json: top-level "labels": [...] array.
    renovate_path = repo_root / 'renovate.json'
    if renovate_path.exists():
        import json
        renovate_cfg = json.loads(renovate_path.read_text(encoding='utf-8'))
        for name in renovate_cfg.get('labels', []):
            referenced.append(('renovate.json', name))

    # GitHub Actions workflows: scan for `labels: ['x', 'y']` literals and
    # actions/github-script `labels:` keys (both wire the GitHub label
    # API). pr-privileged.yaml is the production case today (Container
    # Available), and any future workflow that auto-labels lands here
    # too.
    import re
    label_literal_re = re.compile(
        r"labels:\s*\[\s*(?P<names>[^\]]*)\s*\]",
        re.IGNORECASE,
    )
    workflows_dir = repo_root / '.github' / 'workflows'
    if workflows_dir.is_dir():
        for workflow in sorted(workflows_dir.glob('*.yaml')):
            text = workflow.read_text(encoding='utf-8')
            for match in label_literal_re.finditer(text):
                raw = match.group('names')
                # Items may be single- or double-quoted; strip both.
                for item in raw.split(','):
                    name = item.strip().strip("'").strip('"').strip()
                    if name:
                        referenced.append(
                            (str(workflow.relative_to(repo_root)), name),
                        )

    assert referenced, 'sanity: expected to find at least one external label reference'

    drift = [
        (src, name) for src, name in referenced if name not in top_level_names
    ]

    assert not drift, (
        'label references in consumer files do not match a top-level '
        f'label in .github/labels.yml: {drift}. Either rename the '
        'reference to a current top-level label, or add the missing '
        "label to labels.yml. Using an alias here is wrong because the "
        'sync will merge it away on the next reconcile.'
    )


if __name__ == '__main__':
    raise SystemExit(pytest.main([__file__, '-v']))
