# Supported Servers

MailFerry targets any RFC 3501-compliant IMAP server and adapts to the
capabilities each one advertises. Inspect the negotiated plan for any server:

```bash
mailferry capabilities imap.example.com 993
```

## Capability-driven optimisation

| Capability | Used for |
|---|---|
| UIDPLUS (APPENDUID) | Exact source→destination UID mapping, O(1) idempotency |
| LITERAL+ | Non-blocking uploads (no continuation round trip) |
| COMPRESS=DEFLATE | Wire compression |
| CONDSTORE / QRESYNC | Fast incremental re-runs (roadmap) |
| SPECIAL-USE | Localisation-proof Sent/Trash/Junk/Drafts/Archive mapping |
| LIST-STATUS / STATUS=SIZE | One-round-trip enumeration with byte-accurate ETAs |
| NAMESPACE | Prefix and delimiter discovery |
| APPENDLIMIT | Oversize-message preflight |

Every optimisation has a tested fallback; `--baseline` forces the plain
RFC 3501 path for older or unusual servers.

## Server notes

- **Gmail / Google Workspace** — label-aware: `[Gmail]/All Mail` and
  `Important` are excluded by default (they duplicate content); add
  `--gmail-all-mail` to include them. Expect aggressive throttling.
- **Exchange / Exchange Online** — app passwords work today; basic auth on
  Microsoft 365 is being retired (OAuth is on the roadmap). Per-user
  connection throttling is respected via `--per-host-conns`.
- **Dovecot / Mailcow** — reference target; full extension set.
- **Cyrus, Courier** — namespace/delimiter translation is automatic
  (Courier's `INBOX.` prefix included).
- **Zimbra, Kerio, IceWarp, Axigen, SmarterMail, MailEnable** — handled as
  RFC-compliant with capability gating and automatic baseline downgrade.

Tested terminals for the TUI: macOS Terminal, iTerm2, GNOME Terminal,
Konsole, Alacritty, WezTerm, tmux, screen, and SSH sessions.
