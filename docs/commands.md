# Command Reference

```
mailferry run CSV            migrate / sync (opens the TUI); default command
mailferry resume CSV         alias of run — resuming is just running again
mailferry validate CSV       preflight: connect, auth, list, estimate; no writes
mailferry check CSV          alias of validate
mailferry doctor             local environment self-test
mailferry benchmark          built-in loopback throughput benchmark
mailferry init FILE          write a CSV template
mailferry import-state FILE  import the legacy wrapper's migration.state
mailferry capabilities H P   probe a server's capabilities & optimisation plan
mailferry verify CSV         compare Source / Destination / State Database counts
mailferry compact            prune per-message rows for completed folders
mailferry changelog [--full] show release history
mailferry roadmap            show the project roadmap
mailferry --version | --about | --help
```

## Frequently used run options

| Option | Purpose |
|---|---|
| `--workers N` | Concurrent mailboxes (default 10) |
| `--max-conns-per-mailbox N` | Parallel folder pipelines per mailbox (default 3) |
| `--per-host-conns N` | Connection cap per server host (default 8) |
| `--retries N` / `--retry-delay S` | Transient-failure retries; auth failures never auto-retry |
| `--stale-timeout S` | Auto-recover a mailbox with no progress for S seconds (default 300; 0 off) |
| `--recovery-retries N` / `--recovery-interval S` | Recovery attempts per stall (default 3 × 30 s) before marking STALE |
| `--worker-timeout S` | Cluster worker offline after S seconds of heartbeat silence — its mailboxes are reclaimed (default 60) |
| `--lock-timeout S` | Hard cap for any unexplained lease (default 300) |
| `--include` / `--exclude GLOB` | Folder filters (repeatable) |
| `--map FILE` | Explicit `Source = Destination` folder mapping |
| `--sync-flags` | Re-apply changed flags to already-synced messages |
| `--skip-completed` | Skip mailboxes recorded SUCCESS instead of re-checking |
| `--rescan-dest` | Force a fresh destination fingerprint scan |
| `--ephemeral` | Keep no persistent state (one-shot) |
| `--order size` | Largest known mailboxes first |
| `--compress off` | Disable COMPRESS=DEFLATE |
| `--tls-no-verify` | Disable TLS certificate verification (default: on) |
| `--baseline` | Conservative RFC-3501-only mode for quirky servers |
| `--no-tui` | Plain status-line output instead of the TUI |
| `--json-logs` / `--json-progress` | NDJSON event / progress streams |
| `--trace` | Protocol-level logging (credentials redacted) |

## Failed Message Registry

| Command | Purpose |
|---|---|
| `mailferry failed` | List outstanding permanently failed messages (metadata + reasons) |
| `mailferry failed --csv FILE` / `--json` | Export the registry for investigation |
| `mailferry failed --ignore` | Mark entries IGNORED (still skipped, no longer outstanding) |
| `mailferry retry-failed [--mailbox U] [--folder F --uid N]` | Re-queue for the next run; successes become RECOVERED |

## Configuration (mailferry.toml)

Optional. Generated with full documentation on first launch
(`~/.config/mailferry/mailferry.toml`; search order `--config PATH` >
`./mailferry.toml` > default). Missing/invalid settings fall back to
built-in defaults with a warning — configuration problems can never stop
MailFerry. CLI flags always override the file. `mailferry config` shows
the active path and effective overrides.

## Exit codes

`0` success, incl. COMPLETED WITH WARNINGS (failures recorded in the registry) · `1` failures, partial or stale · `130` interrupted · `141` broken pipe.
