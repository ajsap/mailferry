"""
MailFerry - IMAP Migration & Sync
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
at https://github.com/ajsap/mailferry

Access to bundled project metadata (the changelog) that must work both from
a source checkout and from the packaged mailferry.pyz. The release build
copies CHANGELOG.md into the package so it travels inside the zipapp.
"""
from __future__ import annotations

from pathlib import Path

from . import REPOSITORY


def read_changelog() -> str:
    """Return CHANGELOG.md text from the package (works inside the .pyz) or a
    source checkout; a friendly pointer if neither is present."""
    # 1) packaged copy (bundled into the zipapp at build time)
    try:
        import importlib.resources as ir

        res = ir.files("mailferry").joinpath("CHANGELOG.md")
        if res.is_file():
            return res.read_text(encoding="utf-8")
    except (ModuleNotFoundError, AttributeError, OSError, ValueError):
        pass
    # 2) source checkout: repo-root CHANGELOG.md next to the package
    for parent in Path(__file__).resolve().parents:
        candidate = parent / "CHANGELOG.md"
        if candidate.is_file():
            return candidate.read_text(encoding="utf-8")
    return (f"Changelog not bundled in this build.\n"
            f"See {REPOSITORY}/blob/main/CHANGELOG.md\n"
            f"or the Releases page: {REPOSITORY}/releases\n")


def changelog_latest(version: str) -> str:
    """Return just the section for `version` (for release notes / --about)."""
    import re

    text = read_changelog()
    m = re.search(rf"## \[{re.escape(version)}\].*?\n(.*?)(?=\n## \[|\n\[|\Z)", text, re.S)
    return (m.group(1).strip() + "\n") if m else text
