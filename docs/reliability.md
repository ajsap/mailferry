# Resume & Reliability

## Per-message State Database

MailFerry records every message's journey in `migration.db` (SQLite, WAL):
`planned → inflight → done` (or `skipped`), with source and destination UIDs
and fingerprints. Resume after a crash, `kill -9`, reboot, power cut, or
dropped network continues exactly where it stopped, with only a bounded
window of in-flight messages to reconcile against the destination.

## Idempotency invariants

- No `APPEND` without an `inflight` intent row written first.
- Nothing is marked `done` without an `APPENDUID` confirmation or a positive
  destination probe.
- A `UIDVALIDITY` change triggers re-verification, never a blind re-copy.
- The destination is never expunged or deleted by MailFerry.

## Retries & reconnects

- Transient failures (timeouts, resets, dropped connections) reconnect with
  jittered exponential backoff and resume mid-folder.
- Server throttling is respected without consuming the retry budget.
- **Authentication failures are never auto-retried** — repeating them cannot
  succeed and can trigger lockouts. Fix the credential and re-run.

## Graceful shutdown

`Ctrl+C` (or the TUI shutdown) stops admitting new work, lets active workers
finish their current batch, closes connections with a polite `LOGOUT`,
flushes the State Database and logs, and prints a summary — exit code `130`.
A second `Ctrl+C` forces an immediate exit; state stays consistent either
way, and the next run resumes cleanly. If a stalled server is holding the
shutdown up, connections are force-closed after a few seconds so a single
`Ctrl+C` always completes promptly.

## Stale detection & self-healing

Two watchdog layers keep a sync moving without operator attention:

1. **Connection watchdog** (`--timeout`, default 120 s) — a socket with no
   activity at all is closed and reconnected automatically.
2. **Progress supervisor** (`--stale-timeout`, default 300 s) — watches
   *meaningful* progress per mailbox: messages committed, bytes delivered,
   folders advancing, or a meaningful amount of wire traffic. A RUNNING
   mailbox that delivers nothing for the whole window — even through
   reconnect cycles or keepalive noise — is verified stale and recovered:
   its connections are force-closed and the runner reconnects and resumes
   from the last confirmed checkpoint. Recovery re-entry adopts existing
   messages rather than re-copying, so recovery can never duplicate mail,
   and recovery kicks never consume the normal retry budget.

Hard progress within `--recovery-interval` (default 30 s) closes the
episode as **recovered** (logged in History and the session log).
Otherwise recovery retries up to `--recovery-retries` times (default 3);
when exhausted the mailbox is marked **STALE**, the operator is notified
(Errors panel, History, summary, results.csv) and manual actions remain:
`r` retries in place, and rerunning the same command resumes from the last
confirmed state. A mailbox waiting in a deliberate retry backoff is never
treated as stalled. `--stale-timeout 0` disables the supervisor.

## Multi-instance clustering & failover

Several MailFerry instances — on one machine or many — can share a single
State Database and cooperate on the same project. Point every instance at
the same `migration.db` (and CSV) and they form a cluster:

- **Workers.** Each instance registers a Worker ID (`hostname:pid:uuid`)
  and heartbeats every 15 s. The Workers view (F2) shows the roster:
  status (WORKING / IDLE / OFFLINE), active mailboxes, last heartbeat,
  connected since.
- **Atomic claims.** A mailbox is claimed through its lease row in a
  single transaction — two workers can never process the same mailbox.
  Mailboxes claimed by peers show as **REMOTE**, with progress mirrored
  live from the State Database and the owning worker named in the UI.
- **Failover.** A worker silent for `--worker-timeout` (default 60 s —
  crash, `kill -9`, power loss) is offline: its mailboxes are reclaimed
  with an atomic compare-and-swap and **resume from the last confirmed
  checkpoint**. Per-message intent rows and fingerprint adoption make the
  handover duplicate-free. A live worker can never be dispossessed — the
  CAS fails if it heartbeats — and a worker that comes back after losing
  a lease notices within one heartbeat and stops that mailbox at once.
- **Graceful exits** release all ownership immediately
  (`deregister_worker`), so peers resume unfinished mailboxes without
  waiting for any timeout.
- **Startup never refuses.** "Another instance is running" is not an
  error any more — a new instance simply joins, claims whatever is free,
  watches the rest, and stands by as a hot failover (its dashboard stays
  live while peers still hold project mailboxes).

Leases left by pre-cluster (legacy) versions are handled too: silent for
2.5× the old refresh interval → reclaimed; `--lock-timeout` (default
300 s) remains the hard cap for any unexplained lease.

## Failed-message isolation & the registry

Transport trouble and message trouble are handled differently. A pure
connection loss retries the same batch after reconnecting (up to
`batch_attempts`, default 3). Repeated deaths on the same window — or a
drop on messages the server already rejected — switch the folder into
**Recovery Mode**: the failing batch is progressively split
(8 → 4+4 → 2+2 → 1, three attempts per level, with the message that was
mid-APPEND at the moment of death probed first) until the exact failing
message(s) are identified. Each is recorded in the persistent
`failed_messages` registry (folder, UID, Message-ID, subject, sender,
date, size, failure type, reason, timestamps, fail count, status) and the
migration continues — a bad message never stops a mailbox. Source-side
fetch stalls are never blamed on a message.

Future runs skip registry entries by default (logged). Nothing is
blacklisted forever: `mailferry retry-failed` re-queues all/one
mailbox/folder/UID; a successful retry becomes **RECOVERED**. A mailbox
whose healthy messages all migrated finishes **COMPLETED WITH WARNINGS**
(exit 0) with the failure account in the summary, results.csv and
`logs/failed_messages.csv`.

## Verifying a migration

```bash
mailferry verify mailboxes.csv
```

Compares per-folder message counts across the Source Server, Destination
Server, and the State Database.
