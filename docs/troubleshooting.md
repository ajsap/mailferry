# Troubleshooting

## Start with the doctor

```bash
mailferry doctor            # environment self-test
mailferry validate mailboxes.csv   # per-mailbox connect/auth/list preflight
```

## Common issues

**Authentication failure.** MailFerry never auto-retries auth failures (to
avoid lockouts). Fix the password/app-password in the CSV and re-run. On
Microsoft 365 / Gmail, use an app password or wait for OAuth support.

**TLS certificate verification failed.** Verification is on by default. If
you trust a server with a self-signed or mismatched certificate, add
`--tls-no-verify`. Prefer fixing the certificate where possible.

**Connection dropped / timeouts.** These are transient — MailFerry reconnects
and resumes automatically. Persistent drops on one host: lower
`--per-host-conns` or `--workers`. Watch the Errors view (F6 / `6`).

**Server rejects a message (APPENDLIMIT / over quota).** Oversize messages
are skipped with a recorded reason and the mailbox finishes PARTIAL; re-run
after resolving quota to migrate the gaps.

**A folder looks stalled.** It usually isn't — large messages stream in the
background. The Dashboard shows a live byte counter and operation verb; a
genuine stall surfaces as a reconnect. If nothing at all moves for
`--stale-timeout` (default 5 min), MailFerry recovers on its own — you'll
see a RECOVER badge, then `Stale recovery successful` in History (F5). If
every recovery attempt fails, the mailbox turns **STALE**: verify the
server, then press `r` (or just re-run) — resume is always duplicate-safe.
Use `--trace` for wire-level detail (credentials are redacted).

**A mailbox shows REMOTE.** Another MailFerry instance sharing this State
Database has claimed it — that is normal cluster behaviour, not an error.
Its progress mirrors live, the owning worker appears in the row and in
the Workers view (F2), and if that worker dies this instance reclaims the
mailbox automatically after `--worker-timeout` (default 60 s) and resumes
from the last checkpoint. Locks left by crashed pre-cluster versions are
reclaimed automatically too; nothing ever blocks startup.

**Quirky / older server.** Try `--baseline` (RFC 3501 only). Send the output
of `mailferry capabilities HOST PORT` when reporting an issue.

## The TUI won't draw / looks broken

- Enlarge the terminal to at least 80×20 — below that MailFerry shows a
  friendly notice and keeps migrating in the background.
- Over tmux/screen, the digit keys `1`–`0` always work even when `F1`–`F10`
  are intercepted.
- `--no-tui` falls back to timestamped status lines.
- `Ctrl+L` forces a full redraw.

## Reporting a bug

Include `mailferry --version`, your OS and Python version, the relevant
per-mailbox log from `logs/`, and (for protocol issues) a `--trace` excerpt.
File issues at <https://github.com/ajsap/mailferry/issues>.
