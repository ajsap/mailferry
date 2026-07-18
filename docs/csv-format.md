# CSV Format

MailFerry reads a CSV with a header row and one mailbox per line:

```csv
srchost,srcport,srcsecurity,srcuser,srcpassword,dsthost,dstport,dstsecurity,dstuser,dstpassword
imap.example.com,993,ssl,jeslyn@example.com,Secret1,imap.example.org,993,ssl,jeslyn@example.org,Secret2
```

| Column | Meaning |
|---|---|
| `srchost` / `dsthost` | Source / Destination Server hostname |
| `srcport` / `dstport` | IMAP port (usually 993 for SSL, 143 for STARTTLS/plain) |
| `srcsecurity` / `dstsecurity` | `ssl` (implicit TLS) · `tls` (STARTTLS) · `none` |
| `srcuser` / `dstuser` | Login username (often the full email address) |
| `srcpassword` / `dstpassword` | Password or app password |

Generate a ready-to-edit template with `mailferry init mailboxes.csv`.

## Notes

- The header row is required; column order is fixed.
- Passwords live only in this file and in memory during a run — never in the
  State Database, logs, or reports. Protect the file: `chmod 600 mailboxes.csv`.
- A mailbox's identity is `srchost + srcuser → dsthost + dstuser`, so editing
  ports, security, or passwords between runs does not disturb resume.
- Add rows and press `u` in the TUI (or re-run) to pick up new mailboxes.


## Obsolete v1 header

The v1 `old*`/`new*` column names are rejected with an actionable
error. Rename the columns (`old*` → `src*`, `new*` → `dst*`); the values
are unchanged. MailFerry never silently misinterprets columns.
