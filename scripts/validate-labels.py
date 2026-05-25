#!/usr/bin/env python3
"""Validate .github/labels.yml schema for the labels sync workflow.

Rules enforced (all hard failures via exit 1):

1. description <= 100 chars (GitHub REST API limit)
2. color is a 6-char hex string without leading #
3. unique top-level names
4. aliases do not collide with any top-level name
5. aliases do not collide across labels (same legacy name cannot belong to two
   different canonical owners; EndBug/label-sync's "last one wins" behaviour
   is not a contract we want to ship)
6. any description that names a .github/workflows/X.yaml file must point at
   one that actually exists; catches the "auto-applied by X" class of doc
   drift where a label outlives its backing workflow.
7. within a single label's aliases list, no duplicate entries
   (`aliases: ['x', 'x']` would silently overwrite each other in the
   cross-label collision check at rule 5).

Used by .github/workflows/labels.yaml's validate job. Extracted out of an
inline heredoc so it can be covered by unit tests
(scripts/validate_labels_test.py).
"""

from __future__ import annotations

import argparse
import os
import re
import sys
from typing import Iterable

import yaml

_WORKFLOW_REF_RE = re.compile(r'\.github/workflows/([\w.-]+\.ya?ml)')
_HEX6_RE = re.compile(r'^[0-9A-Fa-f]{6}$')


def validate(labels: list[dict], existing_workflows: Iterable[str]) -> list[str]:
    """Apply all schema rules and return the list of error messages.

    An empty list means the input is valid. Caller decides how to surface
    errors (sys.exit, GitHub Actions ::error::, pytest assertion, etc.).
    """
    errors: list[str] = []
    existing = set(existing_workflows)

    # Rule 1: description length.
    for label in labels:
        desc = label.get('description', '') or ''
        if len(desc) > 100:
            errors.append(
                f"{label['name']}: description {len(desc)} chars (max 100)"
            )

    # Rule 2: color format.
    for label in labels:
        color = label.get('color', '') or ''
        if not _HEX6_RE.match(color):
            errors.append(
                f"{label['name']}: bad color {color!r} (must be 6-char hex without #)"
            )

    # Rule 3: unique top-level names.
    names = [label['name'] for label in labels]
    for dup in sorted({n for n in names if names.count(n) > 1}):
        errors.append(f"duplicate name: {dup}")

    # Rule 4: aliases do not collide with any top-level name.
    name_set = set(names)
    for label in labels:
        for alias in label.get('aliases') or []:
            if alias in name_set:
                errors.append(
                    f"alias {alias!r} (under {label['name']!r}) collides with a top-level name"
                )

    # Rule 5: aliases do not collide across labels.
    alias_owner: dict[str, str] = {}
    for label in labels:
        for alias in label.get('aliases') or []:
            if alias in alias_owner:
                errors.append(
                    f"alias {alias!r} listed under both {alias_owner[alias]!r} "
                    f"and {label['name']!r} -- pick one canonical owner"
                )
            else:
                alias_owner[alias] = label['name']

    # Rule 7: no duplicate aliases inside a single label's list. Catches
    # `aliases: ['x', 'x']` typos -- the second entry silently overwrites
    # the first in alias_owner at rule 5, so rule 5 alone cannot see it.
    for label in labels:
        aliases = label.get('aliases') or []
        if len(aliases) != len(set(aliases)):
            seen: set[str] = set()
            for alias in aliases:
                if alias in seen:
                    errors.append(
                        f"{label['name']}: alias {alias!r} listed more than once"
                    )
                seen.add(alias)

    # Rule 6: descriptions cannot reference workflow files that do not
    # exist. Order in this function is "rule 7 first (works on aliases),
    # then rule 6 (works on descriptions)" because the alias-cleanliness
    # checks group naturally together; the docstring lists rules in
    # numeric order, which matches the validate-time evaluation only
    # incidentally. Keeping numeric and execution orders independent so
    # rules can be reordered without renumbering test names.
    for label in labels:
        desc = label.get('description', '') or ''
        for referenced in _WORKFLOW_REF_RE.findall(desc):
            if referenced not in existing:
                errors.append(
                    f"{label['name']}: description references missing workflow "
                    f".github/workflows/{referenced}"
                )

    return errors


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        '--labels-file',
        default='.github/labels.yml',
        help='Path to labels.yml',
    )
    parser.add_argument(
        '--workflow-dir',
        default='.github/workflows',
        help='Directory checked by rule 6 for referenced workflow files',
    )
    args = parser.parse_args()

    with open(args.labels_file, encoding='utf-8') as handle:
        labels = yaml.safe_load(handle)

    existing_workflows: list[str] = []
    if os.path.isdir(args.workflow_dir):
        existing_workflows = os.listdir(args.workflow_dir)

    errors = validate(labels, existing_workflows)

    if errors:
        for err in errors:
            print(f"::error::{err}")
        return 1

    aliases_total = sum(len(label.get('aliases') or []) for label in labels)
    print(f"labels.yml schema OK ({len(labels)} labels, {aliases_total} aliases)")

    return 0


if __name__ == '__main__':
    sys.exit(main())
