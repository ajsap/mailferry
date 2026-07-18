# MailFerry — IMAP Migration & Sync

**A High-Performance Native IMAP Migration Engine**

[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/ajsap/mailferry?include_prereleases&sort=semver)](https://github.com/ajsap/mailferry/releases)
[![Dependencies: none](https://img.shields.io/badge/runtime%20dependencies-none-brightgreen.svg)](#installation)

MailFerry migrates, synchronises and backs up IMAP mailboxes natively —
no imapsync, no external tools, no runtime dependencies. One static
binary speaks IMAP directly to both servers, streams messages
source-to-destination with bounded memory, and records per-message state
in an SQLite database so every run is resumable and duplicate-free.

> **Release status: `v2.0.0-rc.1` (Release Candidate — pre-release).**
> v1.0.0 was the original Python implementation. v2.0.0 is a complete
> native Go rewrite; `v2.0.0-rc.1` makes it publicly available for
> real-world testing before v2.0.0 is declared production-ready.
> Test carefully and keep your source mailboxes until you have verified
> the results. MailFerry never expunges or deletes mail on either server,
> in any mode.

- **Author:** Andy Saputra <andy@saputra.org>
- **Repository:** https://github.com/ajsap/mailferry
- **Support:** https://github.com/ajsap/mailferry/issues
- **Licence:** GNU AGPL v3.0

![MailFerry TUI dashboard](docs/img/tui-dashboard.png)

## Why the Go rewrite

| | v1.0.0 (Python) | v2.0.0 (Go) |
| --- | --- | --- |
| Distribution | Python 3.9+ required | **single static binary**, zero dependencies |
| Concurrency | asyncio | goroutines + bounded pipelines |
| Platforms | anywhere with Python | macOS (arm64/amd64), Linux (amd64/arm64), Windows (amd64/arm64) |
| TUI | hand-rolled ANSI | Bubble Tea + Lip Gloss, compiled in |
| State | SQLite | SQLite (pure Go driver, **no CGO**) — same schema, byte-compatible fingerprints |

The final (unreleased) Python development line is preserved in-tree under
`mailferry/` as the behavioural reference; the Go engine reached feature
parity with it before this RC (see `docs/PARITY-v2.0.0-RC.md`).

## Feature status in v2.0.0-rc.1

**Implemented** (covered by the automated suite):

- Native IMAP engine: pipelined FETCH→APPEND streaming, LITERAL+,
  STARTTLS/SSL, inactivity watchdogs, per-host connection budgets,
  COMPRESS=DEFLATE (auto when the server offers it)
- Per-message SQLite State Database: resume, incremental top-up,
  duplicate-free adoption of pre-existing destination mail, ack-lost
  APPEND reconciliation (a connection loss never double-copies a message)
- Self-healing: stall detection → connection recovery → **Recovery
  Mode** → progressive failed-message isolation (batch → halves → single)
- **Failed Message Registry**: persistent record of messages a server
  refuses (`mailferry failed`, `retry-failed`, `--ignore`), skipped on
  future runs, **COMPLETED WITH WARNINGS** instead of endless retry loops
- Interactive TUI (ten F1–F10 views incl. live dashboard, History,
  follow-mode Logs), automatic TTY detection, `--no-tui` headless mode,
  `status` read-only inspection, graceful shutdown with bounded escalation
- Multi-instance clustering: several MailFerry processes share one
  migration.db, mailboxes are claimed atomically, offline workers are
  reclaimed automatically *(implemented + tested; still under real-world
  validation at RC stage — see Known limitations)*
- mailferry.toml configuration (auto-generated, documented, never fatal)
- Wrapper-compatible CSV input, folder mapping/include/exclude, Gmail
  virtual-folder handling, `--sync-flags` backup mode, `--order size`,
  NDJSON logs, protocol trace with credential redaction

**Planned** (not in this RC — these commands do not exist yet):

- Destination **deduplication mode** (conservative, dry-run by default,
  full audit trail)
- **Date-range migration** (`--from` / `--to`, INTERNALDATE-authoritative)
- `mailferry attach` (connect a TUI to a running headless migration over
  local IPC)
- OAuth 2.0 (XOAUTH2/OAUTHBEARER), MULTIAPPEND, QRESYNC (v2.1.0)

## Installation

Download the binary for your platform from the
[Releases page](https://github.com/ajsap/mailferry/releases), verify the
checksum, make it executable, run it. There is nothing else to install.

```sh
shasum -a 256 -c SHA256SUMS          # verify (macOS: shasum, Linux: sha256sum)
chmod +x mailferry-v2.0.0-rc.1-darwin-arm64
./mailferry-v2.0.0-rc.1-darwin-arm64 version
```

Targets: `darwin-arm64` (all Apple Silicon), `darwin-amd64` (Intel Macs),
`linux-amd64`, `linux-arm64`, `windows-amd64.exe`, `windows-arm64.exe`.

> **macOS Gatekeeper (RC):** the RC binaries are **not Developer-ID
> signed and not notarised** (the arm64 build carries only the Go
> linker's ad-hoc signature). A browser-downloaded copy will therefore
> be blocked until you approve it under **System Settings → Privacy &
> Security → Open Anyway**. That is Gatekeeper working as designed for
> unsigned downloads; the production signing/notarisation pipeline for
> final releases is documented in `docs/RELEASING-MACOS.md`. Never
> disable Gatekeeper for MailFerry or anything else.

Building from source instead: Go 1.22+, `cd go && ./build.sh` (builds all
six targets reproducibly: `CGO_ENABLED=0 -trimpath`).

## Quick start

```sh
mailferry init mailboxes.csv        # write a template
$EDITOR mailboxes.csv               # fill in your mailboxes
mailferry check mailboxes.csv       # preflight: connect, auth, estimate — no writes
mailferry mailboxes.csv             # migrate (same as: mailferry run mailboxes.csv)
```

Re-running the same command is always safe: MailFerry resumes from its
State Database, verifies, and copies only what is missing.

### CSV format (fictional example — use your own servers)

```csv
oldhost,oldport,oldsecurity,olduser,oldpassword,newhost,newport,newsecurity,newuser,newpassword
imap.example.com,993,ssl,jane@example.com,SourcePassword,imap.example.org,993,ssl,jane@example.org,DestinationPassword
```

`*security` is `ssl`, `tls` (STARTTLS) or `none`. The CSV holds plaintext
credentials — protect the file accordingly; MailFerry itself never writes
passwords into its State Database, logs or reports.

### Frequently used flags

```
--workers N            concurrent mailboxes (default 10)
--db PATH              State Database (default ./migration.db)
--logs-dir DIR         logs (default ./logs)
--skip-completed       skip mailboxes already recorded SUCCESS
--include/--exclude G  folder filters (repeatable), --map FILE renames
--sync-flags           re-apply changed flags to already-synced mail
--order csv|size       admission order
--compress auto|off    COMPRESS=DEFLATE (default auto)
--no-tui               headless output   --trace  redacted protocol trace
```

Run `mailferry run -h` for the full list, `mailferry --help` for all
commands (`status`, `failed`, `retry-failed`, `verify`, `doctor`,
`capabilities`, `benchmark`, `compact`, `import-state`, `config`,
`changelog`, `roadmap`, …).

## Configuration (mailferry.toml)

MailFerry works with no configuration at all. On first run it generates a
fully documented `mailferry.toml` (and tells you exactly where —
`~/.config/mailferry/mailferry.toml` unless a `./mailferry.toml` exists
or `--config PATH` is given). Every option ships at its built-in default;
CLI flags always override the file; a missing or broken file can never
stop MailFerry. Your file is never overwritten — when a future version
introduces new options, they are appended as commented, documented
defaults with your settings untouched. `mailferry config` shows the
active path and search order.

## The TUI

An interactive terminal is detected automatically: you get the live
dashboard (per-mailbox progress, wire-speed, ETA, warnings) plus nine
more views on **F1–F10** — Workers (cluster), Mailboxes, Queue,
History/Activity, Errors, Performance, Logs, Settings, Help. Digits
**1–0** mirror the F-keys for terminals that intercept them. `Enter`
opens details, `/` searches, `p` pauses, `r`/`R` retries failed
mailboxes, `u` picks up rows newly added to the CSV, `Space` freezes the
display, `F8` logs follow like `tail -f` (scroll to browse, `F` to
re-follow). `Ctrl+C` opens the graceful-shutdown dialog; a second
`Ctrl+C` forces an immediate exit with state intact.

![History / Activity view](docs/img/tui-history.png)

The TUI is a pure consumer of the engine's event bus: a rendering
problem, terminal resize or lost SSH session can never corrupt migration
state.

## SSH, tmux and headless operation

- **SSH:** run MailFerry normally inside the SSH session; the TUI works
  over SSH from macOS, Linux and Windows clients. If the connection
  drops, the process receives SIGHUP and shuts down gracefully — per-UID
  state is committed, so re-running resumes exactly where it stopped, and
  worker leases mean an abandoned run's mailboxes are reclaimed
  automatically. No mailbox is ever left permanently locked.
- **tmux/screen (recommended for long remote migrations):** start inside
  `tmux new -s mailferry` (or `screen`), detach with `Ctrl+B D`, and the
  migration continues with the TUI intact; reattach any time with
  `tmux attach -t mailferry`.
- **Headless:** `--no-tui` (or any non-interactive stdin/stdout, e.g.
  cron, CI, `nohup`, pipes — detected automatically) prints clean status
  lines instead. `mailferry status --db ./migration.db` inspects a
  running or finished migration read-only from another shell, and
  `--json-logs` / `--json-progress` emit NDJSON for machine consumption.

## Resume, recovery and the Failed Message Registry

- **Resume:** every message is committed to the State Database only after
  the destination confirms it. Interrupt anything — Ctrl+C, crash, power
  loss — and the next run continues from the last confirmed message.
  After an interrupted APPEND, MailFerry *reconciles* with the
  destination rather than assuming failure, so ambiguous windows cannot
  produce duplicates.
- **Stall recovery:** a mailbox with no measurable progress for
  `stale_timeout_seconds` (default 5 min) is verified, its connections
  recycled, and the transfer resumes from checkpoint — automatically.
- **Recovery Mode:** when the same batch keeps breaking the connection,
  MailFerry isolates it progressively (8 → 4+4 → 2+2 → 1) until the
  poisonous message is identified, records it in the **Failed Message
  Registry** with full metadata and failure type, skips it on future
  runs, and finishes the mailbox as **COMPLETED WITH WARNINGS** — a few
  bad messages never hold the other thousands hostage.
- Inspect and manage: `mailferry failed` (list/export `--json`/`--csv`,
  silence with `--ignore`), `mailferry retry-failed` re-queues entries;
  successes become RECOVERED.

## Known RC limitations

- Deduplication mode, date-range migration and `mailferry attach` are
  **not implemented** in this RC (planned for final v2.0.0).
- macOS binaries are unsigned / not notarised (see Installation).
- Clustering and COMPRESS=DEFLATE are implemented and tested against the
  automated suite but still under real-world validation; `--compress off`
  and single-instance operation are the conservative fallbacks.
- Windows console: built and cross-compiled, limited interactive testing.
- Report issues: https://github.com/ajsap/mailferry/issues — a
  `mailferry doctor` output and the relevant `logs/*.log` lines (they
  contain no credentials) make reports actionable.

## Repository layout

```
go/          the Go engine (v2.0.0-rc.1) — cd go && ./build.sh
mailferry/   final Python reference line (1.2.0-dev, unreleased)
tests/       Python reference test suites
docs/        documentation, parity audit, macOS release pipeline
```

## Licence

GNU Affero General Public License v3.0 — see `LICENSE`.
Copyright (C) 2026 Andy Saputra <andy@saputra.org>.
Contributions welcome: issues, feature requests and pull requests at
https://github.com/ajsap/mailferry.
