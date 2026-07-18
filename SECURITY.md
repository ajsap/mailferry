# Security Policy

## Supported Versions

MailFerry follows Semantic Versioning. Security fixes are applied to the
latest released minor version.

| Version | Supported |
|---------|-----------|
| 1.1.x   | ✅        |
| 1.0.x   | ✅        |
| < 1.0   | ❌        |

## Reporting a Vulnerability

Please report security issues **privately** — do not open a public issue for
anything exploitable.

- Preferred: open a private advisory via
  [GitHub Security Advisories](https://github.com/ajsap/mailferry/security/advisories/new).
- Alternatively, email **Andy Saputra** at <andy@saputra.org> with the
  subject line `MailFerry security`.

Please include: the MailFerry version (`mailferry --version`), your platform
and Python version, a description of the issue, and reproduction steps or a
proof of concept if available.

You can expect an acknowledgement within a few days. Once a fix is prepared,
a patched release will be published and the advisory disclosed with credit to
the reporter (unless you prefer to remain anonymous).

## Handling of Credentials

MailFerry never stores mailbox passwords in its State Database, logs, or
reports. Credentials live only in the CSV you provide and in memory for the
duration of a run. Protect your CSV file accordingly (e.g. `chmod 600`).
`--trace` output redacts credentials. TLS certificate verification is on by
default; `--tls-no-verify` disables it and should only be used for servers
you explicitly trust.
