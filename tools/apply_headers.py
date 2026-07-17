#!/usr/bin/env python3
"""Apply the standard MailFerry AGPL-3.0 source header to every Python file.

Idempotent: files already carrying the copyright line are left untouched.
Existing module docstrings are preserved below the header inside the same
docstring block.
"""
from __future__ import annotations

import sys
from pathlib import Path

HEADER = """MailFerry - IMAP Migration & Sync
High-Performance Native IMAP Migration Engine

Copyright (C) 2026 Andy Saputra <andy@saputra.org>

https://saputra.org
https://github.com/ajsap/mailferry

Licensed under the GNU Affero General Public License v3.0 (AGPL-3.0).
This program is free software: you can redistribute it and/or modify it
under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or (at
your option) any later version.

Contributions welcome: submit issues, feature requests and pull requests
at https://github.com/ajsap/mailferry"""

MARK = "Copyright (C) 2026 Andy Saputra"


def apply(path: Path) -> bool:
    text = path.read_text(encoding="utf-8")
    if MARK in text[:2000]:
        return False
    shebang = ""
    if text.startswith("#!"):
        nl = text.find("\n")
        shebang, text = text[:nl + 1], text[nl + 1:]
    stripped = text.lstrip()
    lead = text[: len(text) - len(stripped)]
    if stripped.startswith('"""'):
        close = stripped.find('"""', 3)
        if close < 0:
            raise SystemExit(f"{path}: unterminated module docstring")
        inner = stripped[3:close].strip("\n").rstrip()
        rest = stripped[close + 3:]
        doc = '"""\n' + HEADER + "\n"
        if inner:
            doc += "\n" + inner + "\n"
        doc += '"""'
        new = shebang + lead + doc + rest
    else:
        new = shebang + '"""\n' + HEADER + '\n"""\n\n' + text
    path.write_text(new, encoding="utf-8")
    return True


def main() -> int:
    root = Path(__file__).resolve().parents[1]
    changed = 0
    for sub in ("mailferry", "tests", "tools"):
        for p in sorted((root / sub).rglob("*.py")):
            if "__pycache__" in p.parts:
                continue
            if apply(p):
                changed += 1
                print(f"header applied: {p.relative_to(root)}")
    print(f"{changed} file(s) updated.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
