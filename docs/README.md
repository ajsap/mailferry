# MailFerry Documentation

**MailFerry — IMAP Migration & Sync**
High-Performance Native IMAP Migration Engine

Welcome to the official MailFerry documentation. MailFerry migrates,
synchronises and backs up IMAP mailboxes by speaking the IMAP protocol
natively — no imapsync, no Perl, no third-party dependencies.

## Contents

- [Installation](installation.md)
- [Quick Start](quickstart.md)
- [CSV Format](csv-format.md)
- [Command Reference](commands.md)
- [The Terminal User Interface (TUI)](tui.md)
- [Migration, Sync & Backup](sync-modes.md)
- [Resume & Reliability](reliability.md)
- [Supported Servers](servers.md)
- [Troubleshooting](troubleshooting.md)

## At a glance

```bash
mailferry init mailboxes.csv     # write a CSV template
mailferry validate mailboxes.csv # preflight — connect, auth, list, estimate
mailferry run mailboxes.csv      # migrate (opens the TUI)
mailferry run mailboxes.csv      # run again any time — syncs only new mail
```

- **Requires:** Python 3.9+ (3.12+ recommended), macOS or Linux, a terminal.
- **Standalone:** ship a single `mailferry.pyz`; the only requirement on the
  target machine is Python.
- **Safe to repeat:** runs are idempotent — completed messages are never
  copied twice, even if the State Database is lost.

## Project links

- Repository: <https://github.com/ajsap/mailferry>
- Releases: <https://github.com/ajsap/mailferry/releases>
- Issue tracker: <https://github.com/ajsap/mailferry/issues>
- Changelog: [../CHANGELOG.md](../CHANGELOG.md)
- Licence: GNU AGPL v3.0 — see [../LICENSE](../LICENSE)

© 2026 Andy Saputra · <andy@saputra.org> · <https://saputra.org>
