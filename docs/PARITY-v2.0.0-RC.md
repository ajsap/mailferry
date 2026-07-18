# MailFerry v2.0.0 RC — Python → Go Feature-Parity Matrix

Audit date: 2026-07-18 · Auditor: full source inspection (final Python
tree at `mailferry/`, Go tree at `go/`), not documentation.

Reference columns:

- **v1.0.0** — the public Python release (GitHub).
- **Python final** — the unreleased `1.2.0-dev` Python line (the
  behavioural specification for v2.0.0).
- **Go RC** — this release candidate, after the parity restoration pass.

Classification: `IMPLEMENTED` · `PARTIALLY` · `MISSING` ·
`INTENTIONALLY CHANGED` · `OBSOLETE IN GO` · `ADDED IN GO`.

## Configuration (mailferry.toml)

| Feature | v1.0.0 | Python final | Go RC |
| --- | --- | --- | --- |
| Auto-generate documented TOML on first run, from **any** command | — | ✓ | **IMPLEMENTED** (restored this pass: generation used to run only for `run`/`config`, and the note omitted the file path) |
| Creation note states the full path | — | ✓ | IMPLEMENTED (+ recorded in session.log and repeated after the TUI exits) |
| All sections `[migration] [retry] [recovery] [logging] [dashboard] [database]`, same keys | — | ✓ | IMPLEMENTED (byte-identical template) |
| Never fatal: missing/broken file → warning + defaults | — | ✓ | IMPLEMENTED (tested) |
| Never overwrite an existing file | — | ✓ | IMPLEMENTED (tested: byte-identical + mtime) |
| Unknown keys ignored with warning; invalid values → default with warning | — | ✓ | IMPLEMENTED |
| Future versions add options without destroying customisations | — | (tolerant reader only) | **INTENTIONALLY CHANGED (enhanced)**: new options are appended as fully commented, documented defaults — append-only, parse-safe for strict TOML, idempotent (tested) |
| Search order `--config` > `./mailferry.toml` > `~/.config/mailferry/` | — | ✓ | IMPLEMENTED |

## Commands

| Command | v1.0.0 | Python final | Go RC |
| --- | --- | --- | --- |
| run / resume | ✓ | ✓ | IMPLEMENTED |
| check / validate (preflight: connect, auth, list, estimate) | ✓ | ✓ | IMPLEMENTED (was a stub in the RC — real preflight restored) |
| doctor | ✓ | ✓ | IMPLEMENTED (Go flavour: runtime/platform/TTY/UTF-8/SQLite/CA store) |
| benchmark | ✓ (source tree only) | ✓ | INTENTIONALLY CHANGED: the loopback benchmark is compiled **into** the binary (in-process fake servers) instead of pointing at `tools/benchmark.py` |
| capabilities | ✓ | ✓ | IMPLEMENTED |
| verify | ✓ | ✓ | IMPLEMENTED |
| compact | ✓ | ✓ | IMPLEMENTED |
| import-state | ✓ | ✓ | IMPLEMENTED |
| init (+ `--init` muscle-memory alias) | ✓ | ✓ | IMPLEMENTED |
| changelog | ✓ | ✓ | IMPLEMENTED (CHANGELOG.md embedded at build; falls back to `[Unreleased]` for an RC version) |
| roadmap | ✓ | ✓ | IMPLEMENTED |
| failed (`--all --json --csv --mailbox --ignore`) | — | ✓ | IMPLEMENTED (`--ignore` restored this pass) |
| retry-failed (`--mailbox --folder --uid`) | — | ✓ | IMPLEMENTED |
| config (`--path`) | — | ✓ | IMPLEMENTED (`--path` restored) |
| status | — | — | **ADDED IN GO** (safe read-only inspection of a State Database) |

## Run flags

| Flag group | Python final | Go RC |
| --- | --- | --- |
| `--workers --logs-dir --db --ephemeral --force --skip-completed --retries --retry-delay --per-host-conns --timeout --tls-no-verify --no-tui --config` | ✓ | IMPLEMENTED (pre-existing) |
| `--no-retry --order csv\|size --max-conns-per-mailbox --lock-timeout --compress auto\|off --baseline --include --exclude --map --sync-flags --json-logs --json-progress --trace --debug --check/--dry-run` | ✓ | IMPLEMENTED (all restored this pass) |
| `--reset-stale-locks` | compatibility no-op | IMPLEMENTED as the same documented no-op |
| Obsolete wrapper flags rejected with guidance (`--imapsync-path` etc.) | ✓ | IMPLEMENTED |
| Flags accepted anywhere (`run file.csv --workers 6`) | ✓ (argparse) | IMPLEMENTED (`reorderArgs`) |
| Exit codes 0 / 1 / 130 | ✓ | IMPLEMENTED |
| Exit 141 on broken pipe | ✓ (explicit handler) | INTENTIONALLY CHANGED: the Go runtime's default SIGPIPE handling on stdout produces the same observable 141 |
| >20 workers throttling advisory | ✓ | IMPLEMENTED |

## Engine

| Behaviour | v1.0.0 | Python final | Go RC |
| --- | --- | --- | --- |
| Streaming FETCH→APPEND, bounded windows, backpressure | ✓ | ✓ | IMPLEMENTED |
| Per-message State Database commit protocol (WAL) | ✓ | ✓ | IMPLEMENTED (identical schema; cross-engine verified) |
| Fingerprint adoption, duplicate-free resume | ✓ | ✓ | IMPLEMENTED (byte-compatible `m:`/`h:` fingerprints, pinned tests) |
| In-flight APPEND reconciliation (never assume failure on connection loss) | — | ✓ | IMPLEMENTED (ACK-lost APPEND regression test) |
| COMPRESS=DEFLATE (auto when offered) | ✓ | ✓ | IMPLEMENTED this pass (RFC 4978; wire counters stay beneath the deflate layer; e2e tested) |
| Baseline RFC-3501-only mode | ✓ | ✓ | IMPLEMENTED this pass |
| Protocol trace with credential redaction | ✓ | ✓ | IMPLEMENTED this pass (LOGIN/AUTHENTICATE always `****`; tested) |
| Per-host connection budgets | ✓ | ✓ | IMPLEMENTED |
| Pause actually gates transfers (not display-only) | — | ✓ | IMPLEMENTED this pass (between messages and folders; paused time never counts as stalled) |
| `--order size` admission | ✓ | ✓ | IMPLEMENTED this pass |
| `--sync-flags` re-application (backup mode; deletions never propagated) | ✓ | ✓ | IMPLEMENTED this pass; INTENTIONALLY CHANGED detail: flag comparison is order-canonical (RFC 3501 flag lists are unordered — fixes a spurious-rewrite case) |
| Stale detection → Connection recovery → Recovery Mode (honest vocabulary) | — | ✓ | IMPLEMENTED |
| Progressive batch/message isolation (8 → 4+4 → 2+2 → 1) | — | ✓ | IMPLEMENTED |
| Persistent Failed Message Registry + statuses + RECOVERED transitions | — | ✓ | IMPLEMENTED |
| Known-bad skip on later runs / retry-failed re-queue | — | ✓ | IMPLEMENTED |
| COMPLETED WITH WARNINGS semantics (DB-authoritative on resume) | — | ✓ | IMPLEMENTED |
| Retry/backoff; auth failures never auto-retried | ✓ | ✓ | IMPLEMENTED |
| Log retention (`keep_days`) | ✓ | ✓ | IMPLEMENTED |
| NDJSON event log + progress snapshots | ✓ | ✓ | IMPLEMENTED this pass |
| Bounded shutdown escalation (6 s grace → force-close connections; 2nd Ctrl+C immediate) | — | ✓ | IMPLEMENTED this pass |

## Multi-instance / locks

| Behaviour | Python final | Go RC |
| --- | --- | --- |
| Worker registry, heartbeats, atomic lease claiming | ✓ | IMPLEMENTED |
| REMOTE mirroring of another worker's progress | ✓ | IMPLEMENTED |
| Offline-worker reclaim via CAS + respawn ("Worker takeover") | ✓ | IMPLEMENTED |
| Lease-lost self-stop after failover | ✓ | IMPLEMENTED |
| Stale lock auto-reset of verified-dead leases (+ history event) | ✓ | IMPLEMENTED (history event restored this pass) |
| Interactive stale-lock dialog `[R]/[D]/[C]` | present but **unreachable** (the engine stopped calling it once clustering landed) | **OBSOLETE IN GO** — superseded by the same automation that made it dead code in Python: verified-dead leases are reset automatically, live foreign leases become REMOTE mirrors, ambiguous ones resolve via worker-timeout takeover. A live instance can never be dispossessed. `--reset-stale-locks` remains as a compatibility no-op. |

## TUI

| Feature | Python final | Go RC |
| --- | --- | --- |
| Ten views on F1–F10, familiar dashboard, banner + slogan branding | ✓ | IMPLEMENTED (+ 1–0 digit aliases, mouse wheel: ADDED IN GO) |
| F5 History: navigable, follow, **Enter opens detail** | ✓ | IMPLEMENTED (Enter-detail popup restored this pass) |
| F3 Mailboxes: select, **Enter detail**, `r` retry, `s` sort, `f` filter | ✓ | IMPLEMENTED this pass |
| F6 Errors: select + Enter detail | ✓ | IMPLEMENTED this pass |
| F8 Logs: tail -f follow, scroll disengages, F/End re-follows | ✓ | IMPLEMENTED |
| `/` search (Workers, Mailboxes, History, Errors, Logs) | ✓ | IMPLEMENTED this pass |
| Global keys: `p` pause, `R` retry-all, `u` reload CSV, Space freeze, `^L` redraw, `?` help, `k/j` | ✓ | IMPLEMENTED this pass |
| Graceful shutdown dialog with live phase progression (same seven phases) | ✓ | IMPLEMENTED (live pending→active→done walk restored this pass) |
| View-render crash guard (a view bug never freezes navigation) | ✓ | IMPLEMENTED this pass |
| System monitor (CPU / load / memory / RSS) + 60 s sparklines in Performance | ✓ | IMPLEMENTED this pass — INTENTIONALLY CHANGED on macOS: process-CPU from rusage instead of host CPU via private ctypes calls (pure Go, no CGO); Linux reports host CPU from /proc exactly like Python |
| Real-time wire speed, ETA, aggregate footer | ✓ | IMPLEMENTED |
| Too-small-terminal guard, resize handling | ✓ | IMPLEMENTED |
| Search-as-footer, PAUSED/FROZEN badges | ✓ | IMPLEMENTED this pass |

## Reports / identity

| Feature | Python final | Go RC |
| --- | --- | --- |
| session.log, per-mailbox logs, results.csv (same columns), failed_messages.csv, JSON export, console summary | ✓ | IMPLEMENTED |
| Branding: product, title, slogan ("A High-Performance Native IMAP Migration Engine"), author, AGPL-3.0; single version source | ✓ | IMPLEMENTED (`internal/identity`) |
| Help/About content, keyboard reference | ✓ | IMPLEMENTED (expanded to document the restored keys) |

## Out of scope for parity (new v2.0.0 features, tracked for M3)

Dedup mode, date-range (`--from/--to`) migration, `mailferry attach` over
local IPC, and the published Python-vs-Go benchmark are **new** v2.0.0
capabilities from the rewrite plan — they exist in neither Python line and
are tracked separately. The RC gate does not treat them as regressions.

## Verification

- Go: `go test ./...` — 16 suites green (engine e2e, recovery/isolation,
  registry retry→RECOVERED, stale auto-resume, TOML lifecycle ×3,
  COMPRESS e2e, baseline, trace redaction, sync-flags idempotency,
  sysmon), `go vet` clean, `gofmt` clean.
- Python reference: 89 e2e + 138 TUI checks green after the
  privacy scrub (unchanged behaviour).
- Cross-engine: one `migration.db` written by Go, resumed by Python and
  vice versa with **zero** duplicate appends (M1 audit, schema unchanged).
