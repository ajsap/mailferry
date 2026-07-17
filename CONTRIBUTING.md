# Contributing to MailFerry

Thank you for considering a contribution to **MailFerry – IMAP Migration &
Sync**. Issues, feature requests and pull requests are all welcome at
<https://github.com/ajsap/mailferry>.

## Development setup

MailFerry is standard-library-only Python (3.9+, 3.12+ recommended). There
is nothing to install:

```bash
git clone https://github.com/ajsap/mailferry.git
cd mailferry
python3 -m mailferry --version
```

## Running the tests

The end-to-end suite runs an in-process fake IMAP server — no network, no
credentials:

```bash
python3 tests/test_e2e.py
```

All checks must pass before a pull request is merged. New behaviour needs
new checks — especially anything touching the idempotency invariants
(intent rows, APPENDUID handling, UIDVALIDITY resets, adoption).

## Ground rules

1. **Zero dependencies.** No third-party imports, ever. The release tooling
   fails the build if one appears.
2. **Never corrupt or duplicate mail.** Any change to the transfer path
   must preserve the invariants documented in `mailferry/engine/folder.py`.
3. **Branding is immutable** (changeable only in a major release):
   - Product: `MailFerry`
   - Title: `MailFerry – IMAP Migration & Sync`
   - Slogan: `High-Performance Native IMAP Migration Engine` — displayed
     exactly, never with added or removed adjectives.
   All identity values live in `mailferry/__init__.py` and must be imported
   from there — never hard-coded.
4. **Terminology** (user-facing text): *Migration* (not copy/transfer),
   *Mailbox*, *Worker*, *Dashboard*, *State Database*, *Source Server*,
   *Destination Server*.
5. **Source headers.** Every `.py` file carries the standard AGPL-3.0
   header; run `python3 tools/apply_headers.py` after adding files.
6. **Style:** type hints, docstrings on modules and non-trivial functions,
   ~100 column lines, f-strings, no dead code.

## Versioning & releases

MailFerry follows [Semantic Versioning](https://semver.org):
**PATCH** = fixes/refactors/docs · **MINOR** = backwards-compatible
features/options/capabilities · **MAJOR** = breaking changes (including
State Database schema changes requiring migration). Pre-releases use
`X.Y.Z-alpha.N` / `-beta.N` / `-rc.N`.

The version exists in exactly one place: `__version__` in
`mailferry/__init__.py`. To release:

1. Bump `__version__`.
2. Add a `## [X.Y.Z] - YYYY-MM-DD` section to `CHANGELOG.md`
   ([Keep a Changelog](https://keepachangelog.com) format).
3. `python3 tools/release.py` — validates branding, headers, license and
   changelog, runs nothing you didn't ask for, and builds stamped
   artefacts (`mailferry.pyz`, versioned `.pyz`, source archive, wheel,
   `SHA256SUMS`) into `dist/`.
4. Commit, tag `vX.Y.Z`, push, and attach the `dist/` artefacts to the
   GitHub release.

## Pull requests

- One logical change per PR; reference the issue it addresses.
- Include the reasoning, not just the diff — especially for protocol
  behaviour, where server quirks matter.
- `python3 tests/test_e2e.py` and `python3 tools/release.py --check-only`
  must both pass.

## Reporting bugs

Please include: MailFerry version (`mailferry --version`), Python version,
OS, both server types if known (`mailferry capabilities HOST PORT` output
helps enormously), the relevant per-mailbox log from `logs/`, and — for
protocol issues — a `--trace` excerpt with credentials redacted.
