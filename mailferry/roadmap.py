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

Single source of truth for the public project roadmap. The README roadmap
section and the `mailferry roadmap` command both derive from ROADMAP.
Aspirational, not a fixed commitment — it evolves as the project grows.
"""
from __future__ import annotations

from typing import List, Tuple

# (version, status, summary). status: "done" | "next" | "planned"
ROADMAP: List[Tuple[str, str, str]] = [
    ("v1.0.0", "done",
     "Initial public release — native asyncio IMAP protocol core, per-message "
     "State Database, duplicate-free adoption, live two-sided Dashboard, release tooling."),
    ("v1.2-dev", "done",
     "Unreleased Python reference line: full TUI (ten views), self-healing "
     "stall recovery, failed-message isolation with a persistent registry and "
     "COMPLETED WITH WARNINGS, multi-instance clustering with failover, live "
     "wire-speed metering, mailferry.toml."),
    ("v2.0.0", "next",
     "Complete architectural rewrite in Go: single static cross-platform "
     "binary (macOS/Linux/Windows, arm64+amd64), goroutine-based concurrent "
     "engine, plus destination deduplication and date-range migration modes. "
     "Released only after full feature parity with the Python reference."),
    ("v2.1.0", "planned",
     "Performance: MULTIAPPEND batching and QRESYNC/CONDSTORE delta sync; "
     "OAuth 2.0 (XOAUTH2 / OAUTHBEARER); Prometheus-style metrics."),
]

_MARK = {"done": "✓", "next": "▸", "planned": "·"}


def roadmap_lines() -> List[str]:
    """Plain, terminal-friendly roadmap lines."""
    out = []
    for version, status, summary in ROADMAP:
        label = {"done": "released", "next": "in progress", "planned": "planned"}[status]
        out.append(f"  {_MARK[status]} {version:<9} ({label}) — {summary}")
    return out


def roadmap_markdown() -> str:
    """README roadmap section body (kept in sync with the command output)."""
    rows = []
    for version, status, summary in ROADMAP:
        box = {"done": "[x]", "next": "[ ]", "planned": "[ ]"}[status]
        rows.append(f"- {box} **{version}** — {summary}")
    return "\n".join(rows)
