# Security Policy

## Supported Versions

MailFerry follows Semantic Versioning. Security fixes are applied to the
latest stable release of the current major version (currently the v2.0.x
line — a fix ships as the next patch release).

| Version | Supported |
|---------------------------|-----------|
| 2.0.x (latest release)    | ✅        |
| 2.0.x (older patches)     | ⬆️ superseded — upgrade to the latest release |
| 1.x (Python line)         | ❌ end of life — replaced by the v2 Go rewrite |

## Reporting a Vulnerability

Please report security issues **privately** — do not open a public issue for
anything exploitable.

- Preferred: open a private advisory via
  [GitHub Security Advisories](https://github.com/ajsap/mailferry/security/advisories/new).
- Alternatively, email **Andy Saputra** at <andy@saputra.org> with the
  subject line `MailFerry security`.

Please include: the MailFerry version (`mailferry version`), your operating
system and architecture, whether you used an official release binary or a
self-built one, a description of the issue, and reproduction steps or a
proof of concept if available. MailFerry is a self-contained static binary —
no interpreter or runtime version details are needed.

You can expect an acknowledgement within a few days. Once a fix is prepared,
a patched release will be published and the advisory disclosed with credit to
the reporter (unless you prefer to remain anonymous).

## Security Model

- MailFerry is a single static Go binary (CGO-free). All dependencies are
  compiled in; nothing is downloaded or executed at runtime.
- It makes outbound connections **only** to the IMAP servers listed in your
  CSV. There is no telemetry, no update checker, no phone-home of any kind.
- `mailferry attach` and `mailferry status` open the State Database
  **read-only** on the local filesystem. MailFerry runs no network service,
  opens no listening socket and uses no local IPC endpoint.
- Release binaries are built reproducibly (`CGO_ENABLED=0`, `-trimpath`,
  stripped build IDs) and published with a `SHA256SUMS` manifest — verify
  downloads with `shasum -a 256 -c SHA256SUMS`. macOS binaries are not yet
  Developer-ID signed or notarised; Gatekeeper's one-time approval prompt is
  expected (see `docs/INSTALLATION-MACOS.md`).

## Handling of Credentials

MailFerry never stores mailbox passwords in its State Database,
configuration file (`mailferry.toml`), logs, reports, or exports.
Credentials live only in the CSV you provide and in memory for the duration
of a run:

- CSV validation errors never echo password values.
- `--trace` protocol traces always redact `LOGIN`/`AUTHENTICATE` arguments,
  and message bodies are never written to traces.
- Resuming a migration re-reads credentials from your CSV — nothing is
  persisted between runs.

Protect your CSV file accordingly (e.g. `chmod 600 migration.csv`) and
delete it when the migration is complete.

TLS certificate verification is on by default; `--tls-no-verify` disables it
and should only be used for servers you explicitly trust (e.g. lab systems
with self-signed certificates).

## State Database, Logs and Reports

The State Database (`mailferry.db`) and log/report files contain **mailbox
metadata** — folder names, message counts, Message-IDs, and (in the Failed
Message Registry and its exports) subjects and sender addresses of failed
messages. They never contain passwords or message bodies, but they are your
mailbox owners' data: treat them as confidential.

- MailFerry creates its directories with `0700` and its files (database
  including WAL/SHM side files, configuration, logs) with `0600` on POSIX
  systems; Windows inherits your profile's ACLs.
- Keep the State Database on a **local** filesystem. Network filesystems
  (SMB/NFS) are not supported for multi-instance coordination.
- With `--portable`, the configuration, State Database, logs and cache all
  live beside the executable — on removable media, physically protect (or
  encrypt) the media, since it then carries the migration metadata.

## Data-Integrity Posture

MailFerry never expunges or deletes mail during migration, in any mode.
Deduplication (`mailferry dedup`) analyses by default; `--execute` only
relocates duplicates reversibly (quarantine folder move, or copy plus
`\Deleted` flag) and never issues `EXPUNGE`. Uncertain matches always retain
the email. `--dry-run` blocks every mutating IMAP command inside the client
before any byte reaches the wire.
