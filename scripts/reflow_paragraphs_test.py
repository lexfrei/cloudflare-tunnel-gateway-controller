"""Unit tests for scripts/reflow-paragraphs.py.

Pins the reflow rules + the list-item-continuation regression: the first
docs/development/ reflow pass stripped the indent off shallow-indented
list-item continuation lines, producing mid-bullet wraps that violate
the project's prose-wrap rule. This test fixture exercises the exact
shape that broke so a future edit to the reflow logic that re-introduces
the bug fails here.

Run with: python3 -m pytest scripts/reflow_paragraphs_test.py
"""

from __future__ import annotations

import importlib.util
import pathlib

import pytest

_SCRIPT_PATH = pathlib.Path(__file__).parent / 'reflow-paragraphs.py'
_spec = importlib.util.spec_from_file_location('reflow_paragraphs', _SCRIPT_PATH)
_module = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_module)
reflow = _module.reflow


def test_single_paragraph_collapses_to_one_line():
    src = 'This is one paragraph\nspread across\nthree lines.\n'
    assert reflow(src) == 'This is one paragraph spread across three lines.\n'


def test_two_paragraphs_separated_by_blank_line():
    src = 'First paragraph\nline two.\n\nSecond paragraph\nline two.\n'
    expected = 'First paragraph line two.\n\nSecond paragraph line two.\n'
    assert reflow(src) == expected


def test_headings_are_paragraph_breaks():
    src = '# Heading\nFirst line\nsecond line.\n'
    expected = '# Heading\nFirst line second line.\n'
    assert reflow(src) == expected


def test_fenced_code_block_is_preserved_verbatim():
    src = (
        'Some prose\nwrapped.\n\n'
        '```go\nfunc Foo() {\n    return\n}\n```\n\n'
        'More prose\nwrapped.\n'
    )
    expected = (
        'Some prose wrapped.\n\n'
        '```go\nfunc Foo() {\n    return\n}\n```\n\n'
        'More prose wrapped.\n'
    )
    assert reflow(src) == expected


def test_indented_code_block_is_preserved():
    # 4-space indent = CommonMark indented code; must NOT be reflowed.
    src = 'Intro paragraph\nwrapped.\n\n    code line one\n    code line two\n'
    expected = 'Intro paragraph wrapped.\n\n    code line one\n    code line two\n'
    assert reflow(src) == expected


def test_list_items_stay_separate():
    src = '- first bullet\n- second bullet\n- third bullet\n'
    # Each bullet is its own list paragraph -> stays on its own line.
    assert reflow(src) == src


def test_list_item_continuation_joins_onto_bullet_line():
    """The regression that motivated the script fix.

    Original docs had list items with shallow-indented (2-space)
    continuation lines:

        - **Mode**: Set X on Y to
          do something with Z

    The first reflow pass stripped the 2-space indent and emitted:

        - **Mode**: Set X on Y to
        do something with Z

    Which is the forbidden mid-bullet wrap form. The correct behaviour
    flattens the continuation onto the bullet line.
    """
    src = (
        '- **Mode**: Set X on Y to\n'
        '  do something with Z\n'
        '- **Other**: shorter bullet\n'
    )
    expected = (
        '- **Mode**: Set X on Y to do something with Z\n'
        '- **Other**: shorter bullet\n'
    )
    assert reflow(src) == expected


def test_list_item_with_multiple_continuation_lines():
    src = (
        '- **Mode**: line one\n'
        '  line two\n'
        '  line three\n'
    )
    expected = '- **Mode**: line one line two line three\n'
    assert reflow(src) == expected


def test_numbered_list_continuation_also_joins():
    src = (
        '1. **First**: line one\n'
        '   line two\n'
        '2. **Second**: short\n'
    )
    expected = (
        '1. **First**: line one line two\n'
        '2. **Second**: short\n'
    )
    assert reflow(src) == expected


def test_blank_line_after_list_item_closes_the_list():
    src = (
        '- bullet text\n'
        '  continuation\n'
        '\n'
        'New paragraph\n'
        'wrapped.\n'
    )
    expected = (
        '- bullet text continuation\n'
        '\n'
        'New paragraph wrapped.\n'
    )
    assert reflow(src) == expected


def test_table_rows_preserved_verbatim():
    src = (
        'intro\nwrapped.\n\n'
        '| col1 | col2 |\n'
        '| --- | --- |\n'
        '| a | b |\n'
    )
    expected = (
        'intro wrapped.\n\n'
        '| col1 | col2 |\n'
        '| --- | --- |\n'
        '| a | b |\n'
    )
    assert reflow(src) == expected


def test_blockquote_preserved():
    src = '> quoted line one\n> quoted line two\n\nprose\nwrapped.\n'
    expected = '> quoted line one\n> quoted line two\n\nprose wrapped.\n'
    assert reflow(src) == expected


def test_empty_input_returns_empty():
    assert reflow('') == ''


def test_nested_list_preserves_indent():
    """Nested bullets must keep their leading indent so the renderer
    keeps the nesting. An earlier reflow pass .strip()-ed the bullet
    line as well as continuations, collapsing nested bullets to col 0
    and breaking the nesting (markdownlint flagged it as MD032 because
    the dedented inner list lost its blank-line separator).
    """
    src = (
        '1. Outer item:\n'
        '   - inner bullet a\n'
        '   - inner bullet b\n'
        '2. Next outer item:\n'
        '   - inner of second\n'
    )
    # Inner bullets keep their 3-space indent; outer numbered list items
    # stay flush left. Continuation joining within inner bullets is
    # NOT exercised here -- this test is about the structural preserve.
    assert reflow(src) == src


def test_nested_list_with_continuations():
    """Nested bullet with a continuation line gets joined ONTO the
    nested bullet (preserving the bullet's indent), not flattened to
    the outer list's indent.
    """
    src = (
        '1. Outer:\n'
        '   - inner bullet starts here\n'
        '     and continues on the next line\n'
    )
    expected = (
        '1. Outer:\n'
        '   - inner bullet starts here and continues on the next line\n'
    )
    assert reflow(src) == expected


def test_idempotent_on_already_reflowed_text():
    """Running reflow on its own output must not change anything."""
    src = '# Heading\n\nFirst paragraph already on one line.\n\n- bullet one\n- bullet two\n'
    assert reflow(reflow(src)) == reflow(src)


if __name__ == '__main__':
    raise SystemExit(pytest.main([__file__, '-v']))
