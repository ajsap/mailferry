# Changelog

All notable changes to **MailFerry – IMAP Migration & Sync** are documented
in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

- macOS Developer ID signing + notarisation pipeline
  (docs/RELEASING-MACOS.md) — v2.0.0 binaries are not yet signed or
  notarised.
- OAuth 2.0, MULTIAPPEND, QRESYNC, quarantine purge (v2.1.0).

## [2.0.0] - 2026-07-19

**Stable release** of the complete native Go rewrite — the culmination
of rc.2/rc.3 plus the final feature set, published only after the full
internal release gate passed.

### Added

- **Whole-file CSV validation**: every detectable error reported in a
  single pass (headers, ports 1–65535, security none/ssl/starttls,
  empty values, column counts, quoting), passwords never echoed;
  migration never starts on invalid input.
- **`--dry-run`**: strict read-only runs — mutating IMAP verbs (APPEND,
  STORE, CREATE, …) are blocked at one choke point inside the client
  before any socket write (proven by fake-server mutation counters:
  zero), state kept in memory only, DRY RUN plan summary printed.
- **ISO 8601 date-range migration**: `--from`/`--to`, inclusive bounds,
  explicit offsets and `Z` honoured, local timezone otherwise; IMAP
  INTERNALDATE is the authoritative timestamp; the resolved window is
  persisted in the State Database and a stored window wins on resume
  (deterministic); no `present`/`now` keywords — an omitted `--to`
  simply means no upper bound.
- **`mailferry dedup`**: explicit, destination-only deduplication.
  Analysis (default and `--dry-run`) mutates nothing and writes a
  dedup_report.csv; `--execute` relocates duplicates reversibly (UID
  MOVE to MailFerry-Quarantine when the server offers MOVE, otherwise
  COPY + `\Deleted` flag — **EXPUNGE is never issued**; permanent
  deletion is deliberately not implemented). Duplicates require
  matching normalised Message-ID AND size AND header fingerprint;
  uncertain matches are always retained; keeper is the lowest UID;
  interruption-safe and resumable via the dedup_state table; actively
  leased mailboxes are skipped with a clear notice.
- **`mailferry attach`**: read-only live monitor for running headless
  migrations — 1 Hz read-only State-Database snapshots plus a session
  log tail; takes no leases, writes nothing; attach/detach/re-attach
  freely without ever disturbing workers; non-TTY invocations print a
  one-shot status snapshot.
- **`--portable`**: self-contained mode rooted at the executable's
  directory (mailferry.toml, mailferry.db, logs/, cache/); precedence
  CLI flags → portable → TOML → native; informational commands remain
  zero-side-effect; read-only roots produce clear actionable errors.

### Changed

- Version identity to stable v2.0.0 across the application,
  documentation and release artefacts.

## [2.0.0-rc.3] - 2026-07-19

Final planned Release Candidate before stable v2.0.0. Multi-process
coordination hardening, honest coordination reporting, terminal-safety
fixes and the canonical src/dst CSV format. Published as a GitHub
**pre-release**.

### Fixed

- **Multi-instance visibility**: a MailFerry process that finds its
  mailboxes actively owned by other healthy workers now says so plainly
  on stdout — `Mailbox already active: <mailbox> — owned by worker
  <id> (heartbeat …, run <run-id>)` — and stands by as a hot failover,
  taking the mailbox over automatically when it is released or its
  owner dies. Headless runs stream all notable coordination events
  (takeovers, stale-lock auto-resets, completed-by-another-worker).
  Nothing exits silently: runs that copy nothing explain why
  ("Nothing new to copy — every message was already on the Destination
  Server"), and the startup banner prints the unique
  `Run <run-id> · worker <worker-id>` identity.
- **Terminal restoration**: a second Ctrl+C in the TUI no longer
  hard-exits underneath Bubble Tea — the engine is aborted, the TUI
  quits and restores the terminal, then the process ends (bounded 2 s
  wait). An engine panic is recovered and reported instead of killing
  the process mid-alt-screen. No exit path leaves the terminal in raw
  or alternate-screen state.
- **SQLite multi-process hardening**: WAL / busy-timeout / synchronous
  pragmas now travel in the connection string so every future
  connection gets them, and Run IDs carry a random suffix so processes
  launched in the same second can never collide.

### Changed

- **Canonical CSV format is `src`/`dst`**:
  `srchost,srcport,srcsecurity,srcuser,srcpassword,dsthost,dstport,dstsecurity,dstuser,dstpassword`.
  `mailferry init`, the docs and all fixtures use it; the obsolete v1
  `old*`/`new*` header is rejected with an actionable rename hint —
  columns are never silently misinterpreted.

### Added

- **Real multi-process concurrency tests**: the suite builds the actual
  binary and launches genuine OS processes against one shared
  `mailferry.db`, verifying concurrent independent runs, mixed-CSV
  behaviour (available mailboxes proceed; held ones are reported and
  topped up after release), exactly-once delivery under contention,
  kill‑9 stale-worker reclaim across processes, unique run/worker
  identities, and cross-process resume-to-zero idempotency.

## [2.0.0-rc.2] - 2026-07-18

Release-candidate refinement pass: native operating-system paths, lazy
state initialisation and canonical identity. Published as a GitHub
**pre-release**; not the production release.

### Changed

- **Canonical slogan corrected** everywhere to
  **"High-Performance Native IMAP Migration Engine"** (no leading "A").
  Identity has a single authoritative source (`internal/identity`) and
  is now enforced by an automated test.
- **Native per-OS application paths** (new `internal/paths`, one place
  for every location; resolving a path never creates it):
  - macOS: `~/Library/Application Support/MailFerry/` (configuration +
    State Database), `~/Library/Logs/MailFerry/`,
    `~/Library/Caches/MailFerry/`
  - Linux: XDG Base Directories (`$XDG_CONFIG_HOME/mailferry/`,
    `$XDG_STATE_HOME/mailferry/` for the database and logs,
    `$XDG_CACHE_HOME/mailferry/`), with standard fallbacks
  - Windows: `%APPDATA%\MailFerry\` (configuration),
    `%LOCALAPPDATA%\MailFerry\` (database, `Logs\`, `Cache\`)
  - Precedence: CLI flags → `mailferry.toml` (new `database.path` /
    `logging.directory` keys) → native default. A `./mailferry.toml`
    in the working directory is still honoured; a full `--portable`
    mode is planned but not yet implemented.
- **`mailferry.db` is the canonical State Database** (replacing the
  `./migration.db` working-directory default): one authoritative
  per-user database regardless of the directory MailFerry is run from.
  An existing development `./migration.db` is detected and reported
  with explicit choices — it is never adopted, moved, overwritten or
  silently duplicated. `--db PATH` continues to be honoured verbatim.
- **Zero-side-effect informational commands**: `--help`, `-h`,
  `version`, `--version`, `about`, `changelog`, `roadmap` and
  `config paths` no longer create configuration, directories, logs,
  caches or databases — resolution is separated from creation, and
  regression tests enforce it. Configuration is generated on the
  first *operational* run (`run`/`resume`) or explicitly via
  `mailferry config`; read-only State Database commands (`status`,
  `failed`, `retry-failed`, `verify`, `compact`) now refuse to create
  an empty database as a side effect.
- New **`mailferry config paths`** shows every canonical location
  (with creation status) without creating anything.
- **Restrictive permissions** on MailFerry-generated files: 0700
  application directories; 0600 configuration, State Database
  (including WAL/SHM) and logs, where the platform supports POSIX
  permissions.
- Documentation: README rewritten around the native-path/lazy-creation
  model; **CONTRIBUTING.md rewritten for the Go project** (developer
  workflow, dependency policy, engineering principles, bug-report
  guidance); new end-user **docs/INSTALLATION-MACOS.md** (architecture
  choice, Gatekeeper "Open Anyway" walk-through specific to this RC,
  checksum verification vs notarisation).

### Fixed

- Read-only inspection commands could previously create an empty
  State Database when pointed at a missing path.

## [2.0.0-rc.1] - 2026-07-18 (superseded by rc.2 before public release)

First public **Release Candidate** of MailFerry v2.0.0 — the complete
native Go rewrite. Published as a GitHub **pre-release** for real-world
testing; it is not the production release. Test carefully and keep your
source mailboxes until you have verified results. MailFerry never
expunges or deletes mail on either server.

### Added

MailFerry v2.0.0 is a ground-up rewrite in Go: a single static,
cross-platform binary (macOS arm64/amd64, Linux amd64/arm64, Windows
amd64/arm64) with no runtime dependencies — no Python, no Perl, no
imapsync. The unreleased Python development line (1.2.0-dev, preserved on the
`legacy/python-final` branch) is the behavioural reference; the Go engine
reached functional parity with it in this release candidate (audit:
docs/PARITY-v2.0.0-RC.md). Milestones:

- **M1 (complete)** — native Go IMAP core (pipelined, streamed literals,
  LITERAL+, STARTTLS/TLS, watchdogs), byte-compatible State Database and
  message fingerprints (verified: Go and Python resume each other's
  migration.db with zero duplicate appends), planner with special-use
  mapping and mUTF-7, streaming FETCH→APPEND with bounded backpressure,
  worker pools with per-host budgets, context-based graceful shutdown,
  crash reconciliation incl. the ack-lost-APPEND window, adoption,
  idempotent re-runs, classic live dashboard, wrapper-compatible CSV,
  mailferry.toml, cluster worker registration/heartbeats/dead-worker
  reclaim, results.csv and summary. Go test suite covers fresh migration,
  idempotent re-run, lost-DB adoption, incremental top-up, ack-lost APPEND
  duplicate prevention, rejected-message WARNINGS, cancel/resume and
  Python fingerprint compatibility.
- **RC parity audit (complete)** — a full regression and feature-parity
  audit of the Go release candidate against the final Python reference
  (source-level, both trees; matrix in `docs/PARITY-v2.0.0-RC.md`). Every
  gap found was restored:
  - **mailferry.toml first-run generation fixed**: the default
    configuration is now generated by *any* command on first launch (it
    previously required `run`/`config`), the note states the full path,
    and the event is recorded in `session.log`. New in Go: when a future
    version adds options, they are appended to an existing file as fully
    commented, documented defaults — append-only, idempotent and safe for
    strict TOML parsers; user customisations are never rewritten.
  - **Engine parity restored**: COMPRESS=DEFLATE (RFC 4978, auto when
    offered, wire counters stay honest beneath the deflate layer),
    `--baseline` RFC-3501-only mode, protocol `--trace` with credentials
    always redacted, engine-wide pause gating (paused time never counts
    as stalled), `--order size` admission, `--sync-flags` backup-mode
    flag re-application (order-canonical comparison), NDJSON event +
    progress logs, bounded shutdown escalation (6 s grace, then all
    connections are force-closed so Ctrl+C can never hang on a dead
    socket), "Stale lock auto-reset" history event.
  - **CLI parity restored**: real `check`/`validate` preflight, `doctor`,
    `capabilities`, `verify`, `compact`, `import-state`, `changelog`
    (embedded), `roadmap`, in-binary loopback `benchmark`,
    `failed --ignore`, `config --path`, `--no-retry`,
    `--max-conns-per-mailbox`, `--lock-timeout`, `--reset-stale-locks`
    (compatibility no-op), `--include/--exclude/--map`, `--json-logs`,
    `--json-progress`, `--check/--dry-run`, obsolete wrapper flags
    rejected with guidance, `--init` alias, >20-workers advisory.
  - **TUI parity restored**: Enter detail popups (History, Errors,
    Mailboxes), `/` search in five views, global `p` pause, `r`/`R`
    retry, `u` CSV reload, `s` sort and `f` filter, Space freeze with
    ❄FROZEN badge, `^L` repaint, `?` help, live shutdown-phase
    progression, view-render crash guard, and the system monitor
    (CPU/load/memory/RSS with 60-second sparklines) in Performance.
  - Privacy: all test fixtures, examples, comments and screenshots now
    use RFC-2606 example domains only.
  - Release naming corrected to `mailferry-v2.0.0-<os>-<arch>`
    (the RC's `-m2` suffix read as "Apple M2 only"); `build.sh` is the
    reproducible six-target build; macOS Gatekeeper analysis and the
    Developer ID signing/notarisation pipeline are documented in
    `docs/RELEASING-MACOS.md` (the RC prompt is expected behaviour for an
    unsigned, un-notarised download — production releases will be signed
    and notarised, never worked around).
  - Verification: 16 Go test suites green (including new TOML lifecycle,
    COMPRESS end-to-end, trace redaction and sync-flags idempotency
    regressions); `go vet`/`gofmt` clean; Python reference suites still
    green (89 e2e + 138 TUI).
- **M2 (complete)** — interactive **Bubble Tea + Lip Gloss TUI** compiled
  into the single binary: the familiar dashboard (banner, slogan, live
  mailbox table with progress bars, wire-based Speed/ETA, aggregate
  footer), F1–F10 navigation with 1–0 digit aliases (for terminals/SSH
  that intercept function keys), Workers/cluster roster, Mailboxes with
  detail, Queue, **History/Activity** (honest recovery vocabulary),
  Errors, Performance, **F8 Logs with Follow ON/OFF** (scroll ↑↓/PgUp/PgDn/
  Home/End + mouse wheel; F re-follows), Settings, Help/About, centred
  graceful-shutdown dialog, terminal-resize handling and a too-small
  guard. Automatic terminal detection: an interactive TTY launches the
  TUI, non-interactive/`--no-tui` runs headless — both drive the exact
  same engine (the TUI is a pure consumer of the engine event bus, so a
  resize, lost terminal or SSH disconnect can never corrupt migration
  state). Resilience parity ported and tested: stale supervisor +
  **Recovery Mode**, progressive failed-message isolation, the persistent
  **Failed Message Registry** (`failed` / `retry-failed` commands, JSON/CSV
  export, RECOVERED transitions), **COMPLETED WITH WARNINGS**, and cluster
  worker heartbeats / REMOTE progress mirroring / offline-worker takeover.
  New **`status`** command reads run/worker/registry state without
  competing with active workers. Six static binaries build CGO-free.

### Changed

- **Repository transition: the Go implementation is canonical for v2.**
  The Go module moved from `go/` to the repository root (`go build
  ./cmd/mailferry` works straight after cloning); the Python
  implementation was removed from `main` and preserved permanently on
  the `legacy/python-final` branch (plus the untouched `v1.0.0` tag).
  The embedded changelog now comes from the single root `CHANGELOG.md`
  via `go:embed` — no duplicated copies. Every source file carries the
  standard authorship/SPDX header, enforced by an automated test.

### Known limitations of this release candidate

- Deduplication mode, date-range migration and `mailferry attach` are
  **not implemented yet** (planned for final v2.0.0 — see Unreleased).
- macOS binaries are **not Developer-ID signed and not notarised**:
  Gatekeeper will ask for explicit approval on downloaded binaries
  (System Settings → Privacy & Security → Open Anyway). This is expected
  for the RC; the production signing pipeline is documented in
  docs/RELEASING-MACOS.md and will never rely on weakening Gatekeeper.
- Multi-instance clustering and COMPRESS=DEFLATE are implemented and
  covered by the automated suite, but still undergoing real-world
  validation at RC stage (use `--compress off` to rule compression out
  when diagnosing).
- Windows console support is built and cross-compiled but has had only
  limited interactive testing.

## [1.2.0-dev] - 2026-07-18 (unreleased Python development line)

The final Python-engine feature set — never published to GitHub; retained
in-repo as the behavioural and functional reference for the Go rewrite.
Folds in the never-released v1.1.0 work.

### Added
- **Intelligent failed-message isolation & recovery.** A small batch of
  poison messages can no longer trap a migration (the classic signature:
  the same 8 messages retried through every reconnect until PARTIAL, on
  every resume). MailFerry now distinguishes transport trouble from
  message trouble: pure connection losses retry the same batch (up to
  `batch_attempts`, default 3); repeated deaths on the same window — or a
  drop on messages the server already rejected — switch the folder into
  **Recovery Mode**: the batch is progressively split (8 → 4+4 → 2+2 → 1,
  3 attempts per level, with the message that was mid-APPEND at the moment
  of death probed first), the exact failing message(s) are identified,
  recorded, and the migration continues with everything else. Isolation
  state survives reconnects and restarts; source-side fetch stalls are
  never blamed on a message. Recovery Mode is fast regardless of mailbox
  size — re-entries skip completed work.
- **Persistent Failed Message Registry.** Permanently failed messages are
  recorded in the State Database (`failed_messages`) with folder, UID,
  Message-ID, subject, sender, date, size, failure type (`APPEND_NO`,
  `CONNECTION_RESET`, `TIMEOUT`, `MALFORMED_MIME`, `OVERSIZE`, `UNKNOWN`),
  reason, first/last failure timestamps, failure count and status
  (`FAILED` / `RETRY_PENDING` / `RETRYING` / `RECOVERED` / `IGNORED`).
  Future runs skip known-failed messages by default (logged); nothing is
  ever blacklisted forever: `mailferry retry-failed` re-queues everything,
  one mailbox, one folder or one UID — a successful retry becomes
  **RECOVERED**. `mailferry failed` lists the registry and exports it
  (`--csv FILE` / `--json`); every run also writes
  `logs/failed_messages.csv` when failures are outstanding.
- **COMPLETED WITH WARNINGS.** A mailbox whose healthy messages all
  migrated no longer reports PARTIAL: it completes as **WARNINGS** with a
  clear account (total / migrated / failed / success rate) on the
  dashboard, in the summary, in results.csv (exit code 0) and in History.
  The Mailboxes view gains a Fail column; the dashboard shows a red
  `MsgFail` counter and a per-mailbox warnings line.
- **`mailferry.toml` configuration.** Fully optional: on first launch a
  documented configuration file is generated at
  `~/.config/mailferry/mailferry.toml` (search order: `--config PATH` >
  `./mailferry.toml` > default) with sections `[migration]`, `[retry]`,
  `[recovery]`, `[logging]`, `[dashboard]`, `[database]`. Missing options
  fall back to built-in defaults, invalid values warn and revert, unknown
  keys warn and are ignored — a config problem can never stop MailFerry.
  CLI flags always override the file. `mailferry config` shows the active
  path and effective overrides; `[logging] keep_days` prunes old logs.
- **Clear recovery vocabulary.** History now tells the real story:
  *Stalled transfer detected → Connection recovery 1/3 → Entering
  Recovery Mode → Batch isolation → Failed message isolated → Message
  skipped → Migration resumed → Completed with warnings*. "Recovery
  exhausted" appears only when nothing else can be done (mailbox STALE).
- **Multi-instance clustering on a shared State Database.** MailFerry now
  behaves like a distributed migration platform: several instances (same
  machine or different machines) can point at the same `migration.db` and
  cooperate on one project. Every instance registers as a **Worker**
  (`hostname:pid:uuid`) and heartbeats every 15 s; mailboxes are claimed
  atomically through per-mailbox leases, so two workers can never process
  the same mailbox. A mailbox owned by a live peer shows as **REMOTE** with
  its progress mirrored live from the State Database. If a worker goes
  silent for `--worker-timeout` (default 60 s — crash, kill -9, power
  loss), its mailboxes are **reclaimed automatically** with an atomic
  compare-and-swap and resume from the last confirmed checkpoint — never
  duplicating (recovery re-entry adopts, never re-copies). A displaced
  worker that comes back detects the lost lease on its next heartbeat and
  stops that mailbox immediately. Graceful exits release ownership at once
  (peers resume unfinished jobs). Startup never refuses because "another
  instance is running" — new instances simply join the cluster. The
  Workers view (F2) gains a cluster roster: Worker ID, host, status
  (WORKING / IDLE / OFFLINE), active mailboxes, last heartbeat, connected
  since; the run keeps its dashboard live while peers still hold project
  mailboxes, standing by as a hot failover.
- **Real-time transfer speed.** The Speed column and global Rate ride the
  wire counters (which tick on every socket read/write) with a ~10 s
  rolling average, so throughput is live even while one huge message
  streams or commits are sparse. `0 B/s` now means "connected but idle";
  `-` appears only before a mailbox starts or after it finishes. ETA uses
  the accurate payload rate and falls back to the wire rate during long
  streams, so it keeps counting down instead of freezing.
- **`tail -f` log viewer (F8).** Scroll freely with ↑↓/k j, PgUp/PgDn,
  Home/End — any manual scroll switches to browsing with a clear
  `FOLLOW: OFF (press F to resume)` banner and a position indicator;
  `F` (or End) snaps back to the live tail and auto-scrolls as new entries
  arrive.
- **Stale-sync detection with automatic self-healing.** A supervisor watches
  every running mailbox for *meaningful* progress (messages committed, bytes
  delivered, folders advancing, real wire traffic). A mailbox that delivers
  nothing for `--stale-timeout` (default 5 minutes) — through reconnect
  cycles, keepalive noise or a hung transfer — is verified stale and
  recovered automatically: connections are force-closed and the runner
  reconnects and resumes from the last confirmed checkpoint (never
  duplicates — recovery re-entry adopts, never re-copies). Hard progress
  closes the episode as recovered; otherwise recovery retries up to
  `--recovery-retries` times (default 3), spaced `--recovery-interval`
  seconds (default 30). If all attempts fail the mailbox is marked
  **STALE**, the operator is notified (Errors panel, History, session log,
  summary) and manual actions remain available (`r` retry / rerun).
  Recovery kicks never consume the normal retry budget. The Dashboard shows
  a RECOVER badge during recovery, a red STALE status on failure, and a
  `Stalls n (rec m)` counter; the summary reports stalls detected /
  auto-recovered.
- **Stale locks can never block a run.** Leases left by dead workers —
  cluster or legacy — are verified against heartbeats and reclaimed
  automatically; `--lock-timeout` (default 5 minutes) caps how long any
  unexplained lease survives. A live worker can never be dispossessed:
  every takeover is an atomic compare-and-swap that fails if the owner
  heartbeats. (`--reset-stale-locks` is retained as a compatibility no-op
  — the cluster reclaims on its own.)
- **History / Activity view (F5)** — a chronological, fully navigable
  milestone feed: migrations started/completed, folders migrated,
  reconnects, stale detection & recovery, lock events, pauses and reloads.
  ↑↓ or `k`/`j` select, Enter opens a detail popup, `/` searches, End
  re-enables follow, Esc/`q` returns to the Dashboard. (`k`/`j` and `q` now
  work everywhere.)
- **Terminal User Interface (TUI)** — the primary runtime interface: the
  familiar classic dashboard (banner, info panel, mailbox table with inline
  Source Server / Destination Server detail, summary footer — same layout
  and colour scheme as v1.0.0) remains the main screen, now with an
  F1–F10 navigation bar and nine additional keyboard-driven views.
- Ten views, each an independent module: **Dashboard** (F1/1, the classic
  dashboard), **Workers** (F2/2), **Mailboxes** (F3/3), **Queue** (F4/4),
  **History/Activity** (F5/5), **Errors** (F6/6), **Performance** (F7/7,
  sparklines), **Logs** (F8/8, independent of terminal scrollback),
  **Settings** (F9/9), **Help/About** (F10/0). Digit keys `1`–`0` mirror
  `F1`–`F10` so navigation works on macOS and under tmux/screen where the
  function keys are intercepted.
- Keyboard model: arrows, PgUp/PgDn, Home/End, Enter (details), Esc (back),
  Tab/Shift+Tab, Space (freeze display), `/` (search), `s`/`f` (sort/filter),
  `p` (pause/resume), `r`/`R` (retry), `u` (reload CSV), `Ctrl+L` (redraw).
- Detail popups (Workers, Mailboxes, Errors) with a soft-shadow overlay;
  sort/filter/search on Mailboxes; severity/mailbox filters and follow mode
  on Logs.
- **Graceful shutdown dialog**: Ctrl+C shows a centred, branded Unicode
  dialog with a soft shadow reporting each phase live — stopping the
  scheduler, waiting for active Workers, saving migration state, flushing
  logs, closing IMAP connections, releasing resources, shutdown complete.
  A second Ctrl+C forces an immediate exit.
- New CLI commands: `resume` (alias of `run`), `validate` (alias of `check`),
  `doctor` (environment self-test), `benchmark` (loopback throughput),
  `changelog` and `roadmap` (release history and roadmap in the terminal).
  `--about` and F10 show a full About panel with version, author, repository,
  documentation, issue tracker, community and licence.
- Friendly small-terminal guard: below 80×20 the UI shows a centred notice
  (with live progress) and restores automatically when enlarged.
- Published documentation set under `docs/` (installation, quick start, CSV
  format, commands, TUI, sync modes, reliability, servers, troubleshooting).

### Improved
- macOS memory readout now shows used memory (via `host_statistics64`),
  not just total.
- Rendering avoids full-screen clears except on resize (per-row differential
  repaint); the final frame and summary print into normal scrollback on exit.
- Input, rendering, scheduling, logging and the State Database run on
  independent threads/loop; the interface can never block or slow migration.

### Fixed
- Ctrl+C exits cleanly, always: stop accepting new work → active Workers
  finish their current batch → connections closed with a polite LOGOUT →
  State Database and logs flush → summary prints (exit 130). A second Ctrl+C
  stops immediately; state stays consistent either way.
- **Graceful shutdown is now bounded even when a server has stalled.** A
  single Ctrl+C previously waited on any in-flight network operation (up to
  the inactivity timeout — minutes on a hung server), which looked like a
  hang and forced a second Ctrl+C. MailFerry now escalates automatically: a
  few seconds after the first Ctrl+C it closes all open IMAP connections, so
  stalled reads/writes unwind at once and shutdown completes promptly. On
  stop, connections are aborted rather than waiting for a polite LOGOUT, and
  the LOGOUT wait on clean exit is shorter. State stays consistent — the next
  run resumes exactly where it stopped.
- View table rows are ANSI-aware: colour-wrapped cells (e.g. the Mailboxes
  status icon) can no longer emit a broken escape that ate the first letter
  of the next column, and could desync the terminal into an apparent hang.
- A faulty view renders an inline error panel instead of freezing navigation.
- Stray asyncio errors during reconnects no longer spray tracebacks; they are
  routed to the session log.
- Folder counter no longer over-counts after reconnects, and mailbox totals
  no longer inflate on reconnect re-entries (resynced from the State Database).
- Abandoned transfers during shutdown/reconnect are drained so connection
  readers can never wedge on a full body queue.

### Changed
- The interactive runtime is now the TUI; `--no-tui` (alias `--no-console`)
  falls back to timestamped status lines, and output redirection auto-disables
  the TUI.
- Connection inactivity watchdog allows 3× headroom while an APPEND awaits
  acknowledgement — large messages on slow destinations no longer trigger
  premature reconnects.
- Minimum interactive terminal size is 80×20.

### Performance baseline
Loopback benchmark (`mailferry benchmark`, in-process servers — bounded by the
test harness, not a WAN figure): 20,000 × 8 KB messages across 4 mailboxes /
4 workers migrated in ~17 s ≈ **1,150 msgs/s, 9.1 MB/s**, integrity verified
(no loss, no duplicates). A relative baseline for tracking future releases.

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

[Unreleased]: https://github.com/ajsap/mailferry/compare/v2.0.0...HEAD
[2.0.0]: https://github.com/ajsap/mailferry/compare/v2.0.0-rc.3...v2.0.0
[2.0.0-rc.3]: https://github.com/ajsap/mailferry/compare/v2.0.0-rc.2...v2.0.0-rc.3
[2.0.0-rc.2]: https://github.com/ajsap/mailferry/compare/v1.0.0...v2.0.0-rc.2
[2.0.0-rc.1]: https://github.com/ajsap/mailferry/blob/main/CHANGELOG.md
[1.0.0]: https://github.com/ajsap/mailferry/releases/tag/v1.0.0
