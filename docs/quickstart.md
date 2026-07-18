# Quick Start

```bash
# 1) Write a CSV template and fill in your accounts
mailferry init mailboxes.csv
vi mailboxes.csv

# 2) Preflight — connects, authenticates, lists folders, estimates sizes.
#    Writes nothing.
mailferry validate mailboxes.csv

# 3) Migrate. The Terminal User Interface opens automatically.
mailferry run mailboxes.csv --workers 6

# 4) Run again any time — only new messages are copied (a safe backup top-up).
mailferry run mailboxes.csv
```

Interrupted? Crashed? Rebooted? **Run the same command again** — MailFerry
resumes exactly where it stopped, from per-message state in its State
Database.

## What a run looks like

Once a migration starts, control transitions to the TUI — a full-screen
operations console (see [tui.md](tui.md)). Press the digit keys `1`–`0`
(or `F1`–`F10`) to switch views, `/` to search, `p` to pause, and `Ctrl+C`
for a graceful shutdown.

For unattended or scripted runs, redirect output (or pass `--no-tui`) and
MailFerry prints timestamped status lines instead, plus optional
`--json-progress` snapshots.

## Common options

```
--workers N               concurrent mailboxes (default 10)
--sync-flags              re-apply changed flags to already-synced messages
--include / --exclude G   only / never sync folders matching glob G
--map FILE                explicit "Source = Destination" folder mapping
--dry-run                 alias of `validate`
--ephemeral               keep no persistent state (one-shot)
```

Run `mailferry run --help` for the complete list.
