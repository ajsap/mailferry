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
"""

# ---------------------------------------------------------------------------
# Single source of truth for MailFerry's identity. Every surface — dashboard,
# startup banner, --version, --about, help output, log headers, package
# metadata, build artefacts — derives from these constants. Never duplicate
# the version string anywhere else in the project (SemVer: MAJOR.MINOR.PATCH,
# pre-releases like 2.0.0-rc.1 are supported).
# ---------------------------------------------------------------------------

__version__ = "1.0.0"

PRODUCT = "MailFerry"
TITLE = "MailFerry – IMAP Migration & Sync"
SLOGAN = "High-Performance Native IMAP Migration Engine"
AUTHOR = "Andy Saputra"
AUTHOR_EMAIL = "andy@saputra.org"
COPYRIGHT = "Copyright (C) 2026 Andy Saputra <andy@saputra.org>"
REPOSITORY = "https://github.com/ajsap/mailferry"
PROJECT_URL = "https://saputra.org"
SUPPORT_URL = "https://github.com/ajsap/mailferry/issues"
LICENSE_NAME = "GNU Affero General Public License v3.0 (AGPL-3.0)"


def banner_line() -> str:
    """The official one-line application title with version."""
    return f"{PRODUCT} v{__version__} — IMAP Migration & Sync"


def version_text() -> str:
    """Two-line output for --version (title + official slogan)."""
    return f"{banner_line()}\n{SLOGAN}"


def about_text() -> str:
    """Output for --about."""
    return f"""{banner_line()}
{SLOGAN}

Author:
  {AUTHOR} <{AUTHOR_EMAIL}>
  {PROJECT_URL}

Repository:
  {REPOSITORY}

Support / Issues:
  {SUPPORT_URL}

License:
  {LICENSE_NAME}

MailFerry is a native, high-performance IMAP migration and synchronisation
engine designed for reliable, resumable, large-scale mailbox migrations.
MailFerry speaks the IMAP protocol directly — no imapsync, no external
tools, no third-party dependencies — and migrates, synchronises and backs
up mailboxes with per-message state in its State Database, duplicate
prevention, and a live Dashboard covering both the Source Server and the
Destination Server.

Contributions are welcome.
Fork the project, report issues, or submit pull requests on GitHub.
"""
