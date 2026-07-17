# Changelog

All notable changes to **MailFerry – IMAP Migration & Sync** are documented
in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.0.0] - 2026-07-17

First stable release of MailFerry — High-Performance Native IMAP Migration
Engine.

### Added
- Native asyncio IMAP protocol core: pipelined commands, streamed message bodies
  (constant memory), non-synchronising literals (`LITERAL+`), wire
  compression (`COMPRESS=DEFLATE`), STARTTLS and implicit TLS with
  certificate verification on by default, inactivity and byte-progress
  watchdogs, capability-driven optimisation with `--baseline` fallback.
- Migration, sync and backup in one command: fresh migration, fingerprint
  **adoption** of destinations pre-synced by other tools (duplicate-free,
  even after State Database loss), and incremental top-up runs; deletions
  are never propagated; optional `--sync-flags` re-applies flag changes.
- Per-message State Database (SQLite WAL): planned/inflight/done/skipped
  states, source→destination UID mapping via `APPENDUID`, UIDVALIDITY-aware
  re-verification, crash-window reconciliation, mailbox leases,
  `import-state` migration from the legacy wrapper, `compact` maintenance
  command.
- Scheduler: worker pools, per-host connection budgets, parallel folder
  pipelines inside large mailboxes, throttle-aware backoff; authentication
  failures are never auto-retried.
- Folder planner: NAMESPACE/delimiter translation, special-use role mapping
  (localisation-proof), Gmail virtual-folder policy, mUTF-7 Unicode names,
  include/exclude globs, explicit `--map` file.
- Live Dashboard: alternate-screen differential renderer, two-line MailFerry
  banner, global progress (messages/data %, throughput, ETA, duplicates
  prevented, reconnects, retries), per-mailbox operation verbs and detailed
  Source Server / Destination Server panels; non-TTY status lines;
  `--json-progress` snapshots.
- Reporting: session log, per-mailbox logs, `results.csv`, end-of-run
  summary, optional NDJSON event logs, `--trace` protocol logging with
  credential redaction.
- CLI: `run`, `check` (write-nothing preflight), `init`, `import-state`,
  `capabilities`, `verify`, `compact`; `--version`, `--about`, `--help`;
  exit codes 0/1/130/141.
- Tooling: single-source version and identity (`mailferry/__init__.py`),
  release builder with branding/header/changelog/license validation and
  SHA-256 checksums, AGPL source-header applier, end-to-end fake-IMAP test
  suite (34 checks).
- Packaging: standalone `mailferry.pyz` (zipapp), source archive, wheel.

[Unreleased]: https://github.com/ajsap/mailferry/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/ajsap/mailferry/releases/tag/v1.0.0
