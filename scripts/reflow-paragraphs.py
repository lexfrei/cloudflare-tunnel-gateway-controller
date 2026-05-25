#!/usr/bin/env python3
"""Reflow hard-wrapped paragraphs in markdown to one line per paragraph.

Skips: fenced code blocks, indented code blocks (4+ space indent), headings,
blockquotes, HTML tags, table rows, horizontal rules.

List items are treated specially: a bullet plus any subsequent
shallow-indented (1-3 space) non-blank lines collapse onto ONE line so the
list-item continuation does not lose its association with the bullet.

A "paragraph" is a run of consecutive prose lines bracketed by blank lines (or
structural elements above). Within a paragraph, adjacent lines are joined with
a single space.
"""

from __future__ import annotations

import re
import sys

FENCE = re.compile(r'^\s*```')
HEADING = re.compile(r'^#{1,6}\s')
LIST_BULLET = re.compile(r'^\s*[-*+]\s')
LIST_NUMBERED = re.compile(r'^\s*\d+\.\s')
BLOCKQUOTE = re.compile(r'^\s*>')
HR = re.compile(r'^\s*(?:-{3,}|\*{3,}|_{3,})\s*$')
TABLE_ROW = re.compile(r'^\s*\|')
HTML_BLOCK = re.compile(r'^\s*<(?:!--|/?[a-zA-Z])')
INDENTED_CODE = re.compile(r'^    ')  # 4-space indent => code per CommonMark
SHALLOW_INDENT = re.compile(r'^[ ]{1,3}\S')  # 1-3 space indent => list continuation


def is_list_item(line: str) -> bool:
    return bool(LIST_BULLET.match(line) or LIST_NUMBERED.match(line))


def is_structural(line: str) -> bool:
    """Lines that must stay as-is and break a paragraph."""
    if not line.strip():
        return True
    if HEADING.match(line):
        return True
    if BLOCKQUOTE.match(line):
        return True
    if HR.match(line):
        return True
    if TABLE_ROW.match(line):
        return True
    if HTML_BLOCK.match(line):
        return True
    if INDENTED_CODE.match(line):
        return True
    return False


def reflow(text: str) -> str:
    out: list[str] = []
    paragraph: list[str] = []
    list_buffer: list[str] = []
    in_fence = False

    def flush_paragraph():
        if paragraph:
            out.append(' '.join(line.strip() for line in paragraph))
            paragraph.clear()

    def flush_list():
        if list_buffer:
            # Preserve the bullet line's leading indent so nested bullets
            # stay nested. Only continuation lines are .strip()-ed and
            # joined onto the bullet's tail.
            first = list_buffer[0].rstrip()
            rest = [line.strip() for line in list_buffer[1:]]
            if rest:
                out.append(first + ' ' + ' '.join(rest))
            else:
                out.append(first)
            list_buffer.clear()

    for line in text.splitlines():
        if FENCE.match(line):
            flush_paragraph()
            flush_list()
            out.append(line)
            in_fence = not in_fence
            continue
        if in_fence:
            out.append(line)
            continue
        if is_list_item(line):
            flush_paragraph()
            flush_list()
            list_buffer.append(line)
            continue
        # While a list item is open, ANY indented non-blank line is a
        # continuation of THAT bullet -- even if the indent is 4+ spaces
        # (which is_structural would otherwise classify as code). This
        # is what lets nested-bullet continuations aligned to the inner
        # bullet text (typically column 5 or deeper) stay attached.
        if list_buffer and line and line[0] == ' ' and line.strip():
            list_buffer.append(line)
            continue
        # Shallow-indented (1-3 space) line outside a list: continuation
        # of the current paragraph.
        if SHALLOW_INDENT.match(line):
            paragraph.append(line)
            continue
        if is_structural(line):
            flush_paragraph()
            flush_list()
            out.append(line)
            continue
        # Plain prose line at column 0: continues the current paragraph but
        # closes any open list block.
        flush_list()
        paragraph.append(line)

    flush_paragraph()
    flush_list()

    result = '\n'.join(out)
    if text.endswith('\n'):
        result += '\n'
    return result


def main() -> int:
    for path in sys.argv[1:]:
        with open(path, encoding='utf-8') as fh:
            original = fh.read()
        reflowed = reflow(original)
        if reflowed != original:
            with open(path, 'w', encoding='utf-8') as fh:
                fh.write(reflowed)
            print(f'reflowed: {path}')
        else:
            print(f'unchanged: {path}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
