# CSV Format

MailFerry reads a CSV with a header row and one mailbox per line:

```csv
oldhost,oldport,oldsecurity,olduser,oldpassword,newhost,newport,newsecurity,newuser,newpassword
imap.example.com,993,ssl,jane@example.com,Secret1,imap.example.org,993,ssl,jane@example.org,Secret2
```

| Column | Meaning |
|---|---|
| `oldhost` / `newhost` | Source / Destination Server hostname |
| `oldport` / `newport` | IMAP port (usually 993 for SSL, 143 for STARTTLS/plain) |
| `oldsecurity` / `newsecurity` | `ssl` (implicit TLS) · `tls` (STARTTLS) · `none` |
| `olduser` / `newuser` | Login username (often the full email address) |
| `oldpassword` / `newpassword` | Password or app password |

Generate a ready-to-edit template with `mailferry init mailboxes.csv`.

## Notes

- The header row is required; column order is fixed.
- Passwords live only in this file and in memory during a run — never in the
  State Database, logs, or reports. Protect the file: `chmod 600 mailboxes.csv`.
- A mailbox's identity is `oldhost + olduser → newhost + newuser`, so editing
  ports, security, or passwords between runs does not disturb resume.
- Add rows and press `u` in the TUI (or re-run) to pick up new mailboxes.
