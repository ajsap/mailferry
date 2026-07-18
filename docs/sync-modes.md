# Migration, Sync & Backup

One command handles three scenarios, detected automatically per folder.

| Situation | What MailFerry does |
|---|---|
| Destination folder empty | Fast native migration |
| Destination already has mail (synced earlier by imapsync or any tool — or the State Database was lost) | **Adoption**: a fingerprint scan records existing messages as done — nothing is copied twice |
| The State Database already knows the folder | **Incremental sync**: only new messages are copied |

## Duplicate prevention

Every message has a fingerprint: the normalised `Message-ID`, or a hash of
`Date | From | To | Subject | size` when no Message-ID exists. Matching is
count-aware (a mailbox with three identical newsletters keeps three).
Provider IDs (`X-GM-MSGID`, `OBJECTID`) corroborate when present.

Because adoption reconstructs state from the destination itself, **losing
`migration.db` can never cause duplicates** — the next run simply re-adopts.

## Backup semantics

- Strictly one-way, top-up: run it hourly or nightly from cron.
- **Deletions are never propagated** — a message removed from the source
  stays on the destination. That is what makes MailFerry usable as a backup.
- `--sync-flags` optionally re-applies changed flags (read/unread, flagged)
  to already-synced messages.

## Useful flags

| Flag | Effect |
|---|---|
| `--skip-completed` | Skip mailboxes already recorded SUCCESS |
| `--rescan-dest` | Force a fresh destination fingerprint scan |
| `--no-dedup-scan` | Skip adoption for a guaranteed-empty destination |
| `--force` | Full replan + destination re-verification (never blind re-copy) |
