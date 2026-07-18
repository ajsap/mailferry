# Contributing to MailFerry

Thank you for considering a contribution to **MailFerry v2.0.0-rc.3 — IMAP
Migration & Sync**. Issues, feature requests and pull requests are all
welcome at <https://github.com/ajsap/mailferry/issues>.

MailFerry v2 is a complete native Go rewrite of the original Python tool.
`main` is Go-only: there is no Python code path to build, test or extend
here. The Python history is not lost — v1.0.0 was the original public
Python release, unreleased Python development continued after v1.0.0, and
that final Python line is preserved on the `legacy/python-final` branch
and in the repository's Git history for anyone who needs it as a
behavioural reference. This document covers the current Go project only.

## Developer workflow

Everything below works from a fresh clone. **Go 1.22+ is required to
build MailFerry from source.** This requirement applies to contributors
only — end users of the official release binaries do not need Go, or any
other runtime, installed at all.

```sh
git clone https://github.com/ajsap/mailferry.git
cd mailferry

go build ./cmd/mailferry     # build the mailferry binary for your platform
go test ./...                # run the full test suite
go vet ./...                 # static analysis

gofmt -l cmd internal .      # must print nothing — any output is a formatting failure

./build.sh                   # builds all six release targets (CGO_ENABLED=0, reproducible)
```

A pull request that fails `go test ./...`, `go vet ./...`, or prints any
path from `gofmt -l cmd internal .` is not ready for review. Run all four
checks locally before pushing.

## Code standards

- **gofmt-clean.** `gofmt -l cmd internal .` must produce no output. Run
  `gofmt -w` on anything it flags before committing.
- **go vet-clean.** `go vet ./...` must pass with no findings.
- **Every source file carries the licence header**, enforced automatically
  by `headers_test.go` as part of `go test ./...`. New files without it
  fail the build. Copy the header block verbatim from the top of any
  existing `.go` file in the repository — do not retype it from memory.
- **Identity strings come from one place only.** Product name, title,
  slogan and version are defined in `internal/identity` and must always be
  imported from there. Never hard-code `"MailFerry"`, the slogan, or the
  version string anywhere else in the codebase — banners, reports, the
  TUI, and CLI output must all derive from `internal/identity` so a single
  edit updates everything consistently.
- **Tests must pass, not be weakened.** If a change makes an existing test
  fail, fix the change or fix the test's expectations for a legitimate
  reason — never loosen an assertion, delete a check, or skip a test
  purely to get a green run. A passing suite is only meaningful if the
  assertions in it still mean something.

## Dependency policy

MailFerry does not claim to have zero dependencies, and no contribution
should claim that either. What we do promise, and what every dependency
decision must protect, is: **official binaries carry no external runtime
dependency** — no Go toolchain, no Python, no `imapsync`, no Node, no
.NET, no Java, nothing, for the person who downloads a release binary.

In service of that promise:

- Add a dependency deliberately, not by default. Ask whether the standard
  library already covers the need before reaching for a module.
- Avoid unnecessary dependencies. Every module added is something that has
  to be vetted, kept updated, and trusted with build-time and (for some
  packages) runtime behaviour.
- Prefer maintained, licence-compatible, pure-Go implementations where
  practical. `CGO_ENABLED=0` cross-compilation to all six release targets
  from a single build environment is a hard requirement — a dependency
  that needs CGO or a platform-specific toolchain to cross-compile is a
  strong reason to look for an alternative.
- Preserve standalone, reproducible cross-platform compilation. `./build.sh`
  must keep working, unattended, for `darwin-arm64`, `darwin-amd64`,
  `linux-amd64`, `linux-arm64`, `windows-amd64` and `windows-arm64`.

## Core engineering principles

**Never corrupt, lose, or unnecessarily duplicate email.** This is the
one rule every other convenience yields to. Any change that touches
migration behaviour must preserve, and where possible add test coverage
for:

- **Data integrity** — the message that lands on the destination is the
  message that left the source, byte-for-byte, flags and metadata intact
  where applicable.
- **Idempotency** — running the same migration twice never produces two
  copies of the same message.
- **Safe resume** — interrupting a run at any point (Ctrl+C, crash, power
  loss, network drop) and running again must continue correctly from the
  last confirmed state, never re-copy already-confirmed mail, and never
  skip mail that was never confirmed.
- **APPEND reconciliation** — when a connection is lost after an APPEND is
  sent but before its result is known, MailFerry must reconcile with the
  destination rather than assume success or failure, so an ambiguous
  window can never produce a duplicate or a silent loss.
- **Checkpoint integrity** — recorded progress must always reflect mail
  that is actually, verifiably present on the destination — never
  progress that was merely attempted.
- **UIDVALIDITY handling** — a UIDVALIDITY change on either server must be
  detected and handled correctly, never silently ignored in a way that
  could mis-map or duplicate messages.
- **Failed-message isolation** — a message a server refuses must be
  isolated and recorded without blocking, corrupting, or duplicating the
  transfer of every other message in the same mailbox.
- **Worker/lease safety** — under clustering, a mailbox claimed by one
  worker must never be double-processed by another, and an abandoned
  lease must be reclaimed cleanly rather than left in a state that risks
  duplication.
- **SQLite transactional integrity** — writes to the State Database that
  represent migration progress must be atomic and consistent; a crash
  mid-write must never leave the database claiming a message was
  transferred when it was not, or vice versa.

Forward progress is never worth trading against email integrity. A slower
correct migration is always preferable to a faster one that risks
corrupting, losing or duplicating a single message.

## Reporting bugs

Good bug reports save everyone time. Please include:

- MailFerry version — the output of `mailferry version`.
- Your OS and CPU architecture (e.g. macOS arm64, Linux amd64, Windows
  arm64).
- The source and/or destination IMAP server type, if known.
- Sanitised, relevant log excerpts.
- The output of `mailferry capabilities`, where relevant to the issue.
- Steps to reproduce.
- Whether you were running the TUI or `--no-tui`.
- Whether resuming or restarting the run changes the behaviour.

**Never post passwords, OAuth tokens, authentication secrets, private
email contents, personally identifying mailbox information, or
unredacted logs in an issue, comment or attachment.** Sanitise logs and
CSV excerpts before attaching them. MailFerry itself never writes
passwords to its logs, State Database or reports — but logs can still
contain mailbox names and raw server responses, so review before you
share, and redact anything that identifies a real person or account.

## Versioning & releases

MailFerry follows [Semantic Versioning](https://semver.org). Pre-releases
use `v2.0.0-alpha.N`, `v2.0.0-beta.N` and `v2.0.0-rc.N` naming.

It's worth being precise about what "a release" actually means, because
these are distinct things:

1. **Source on `main`.** Code merged to `main` that has passed CI.
2. **A Git tag.** A tag such as `v2.0.0-rc.3` pointing at a specific
   commit.
3. **A GitHub Release.** A release object on GitHub associated with a tag,
   with release notes and (usually) attached binary artefacts.
4. **Publishing it.** Making that release visible to the public rather
   than leaving it as an unpublished draft.
5. **Marking it a pre-release.** A published release can additionally be
   flagged as a pre-release, which excludes it from GitHub's "latest"
   designation and signals to users that it is not the stable
   recommendation.

Having code on `main` does not mean a version has been released — a
release is an explicit, deliberate action, never something that happens
automatically on push. Release candidates are published as GitHub
**Pre-releases** and must never be marked as the latest stable release.

For the macOS-specific parts of shipping a release — Developer ID signing
and notarisation — see
[`docs/RELEASING-MACOS.md`](docs/RELEASING-MACOS.md). That pipeline is
maintainer-only; it isn't part of the day-to-day contributor workflow
above.

## Licence

MailFerry is licensed under the **GNU AGPL v3.0**. By submitting a
contribution, you agree it is offered under the same licence. Contributors
retain attribution for their own contributions — we do not erase who
wrote what. Please do not submit third-party code without correctly
attributing it and confirming its licence is compatible with the AGPL
v3.0; never present someone else's work as an original contribution.
