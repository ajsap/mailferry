# Installing MailFerry on macOS

This is the end-user guide for downloading and running MailFerry
v2.0.0-rc.3 on macOS. It does not cover how release binaries are signed
or notarised — that maintainer-only pipeline is documented separately and
linked at the bottom of this page.

## 1. Which binary do I need?

MailFerry ships two macOS binaries for v2.0.0-rc.3:

| Your Mac | Binary |
| --- | --- |
| Apple Silicon (any M-series Mac — M1, M2, M3, M4 and later) | `mailferry-v2.0.0-rc.3-darwin-arm64` |
| Intel Mac | `mailferry-v2.0.0-rc.3-darwin-amd64` |

If you're not sure which you have, click the Apple menu → **About This
Mac**. The "Chip" or "Processor" line tells you: anything starting with
"Apple" (e.g. "Apple M2") is Apple Silicon — use `darwin-arm64`. Anything
naming an Intel processor is an Intel Mac — use `darwin-amd64`.

`darwin-arm64` is a single build that covers every Apple Silicon
generation; there is no separate binary per chip.

## 2. Download, make executable, first run

Download the correct binary from the
[Releases page](https://github.com/ajsap/mailferry/releases), then from a
Terminal in the download directory:

```sh
chmod +x mailferry-v2.0.0-rc.3-darwin-arm64
./mailferry-v2.0.0-rc.3-darwin-arm64 --help
```

(Substitute `-darwin-amd64` throughout this guide if you're on an Intel
Mac.)

`--help`, `version` and `about` are read-only informational commands —
they print to the terminal and exit. They never create a configuration
file, a State Database, log files, or any directory on your system. The
first files MailFerry creates appear only once you run an actual
operation (or explicit `mailferry config`) — see [section 5](#5-where-mailferry-keeps-its-files-on-macos).

## 3. macOS Gatekeeper

**For this release candidate specifically:** the v2.0.0-rc.3 macOS
binaries are not yet Apple-notarised, so Gatekeeper may block the first
launch. This is expected for this release candidate and is not a
limitation of Go or of MailFerry. (This note describes rc.2; it is easy
to update once a future release is signed and notarised — don't assume
every future version behaves this way.)

If macOS blocks the first launch, here is the exact procedure:

1. Download the correct MailFerry binary.
2. Make it executable if required (`chmod +x …`, see section 2).
3. Attempt to run it once.
4. If macOS blocks it, open **System Settings → Privacy & Security**.
5. Scroll to the **Security** section.
6. Locate the notice indicating MailFerry was blocked.
7. Click **Open Anyway**.
8. Authenticate with Touch ID or administrator credentials if requested.
9. Confirm that MailFerry should be opened.
10. Run MailFerry again.

This one-time, per-binary approval is macOS's own sanctioned path for
running software you have chosen to run yourself. It is **not** a
workaround or a compromise.

**Do not** disable Gatekeeper globally (`spctl --master-disable`) and do
not otherwise weaken macOS security to run MailFerry — that is never
necessary and never recommended. The `Open Anyway` step above is
sufficient and is the correct, intended mechanism for this situation.

Future MailFerry releases are planned to be Developer ID signed and
notarised, which removes this step entirely — a notarised, signed binary
launches without any Gatekeeper prompt.

## 4. Verifying your download

Every release publishes a `SHA256SUMS` file alongside the binaries. You
can verify your download with the checksum tool already built into
macOS — no extra installation required.

Download `SHA256SUMS` from the same release into the same directory as
the binary, then:

```sh
shasum -a 256 -c SHA256SUMS
```

Or check a single file directly and compare the printed hash by eye
against the matching line in `SHA256SUMS`:

```sh
shasum -a 256 mailferry-v2.0.0-rc.3-darwin-arm64
```

**A matching checksum proves one thing only: the file you downloaded is
byte-for-byte identical to the published release artefact.** It is *not*
Apple code signing and it is *not* notarisation — those are separate
mechanisms, unrelated to checksums, and are what section 3 above is
about. A checksum match tells you the download wasn't corrupted or
substituted; it says nothing about Apple's assessment of the binary.

## 5. Where MailFerry keeps its files on macOS

| What | Path |
| --- | --- |
| Configuration | `~/Library/Application Support/MailFerry/mailferry.toml` |
| State Database | `~/Library/Application Support/MailFerry/mailferry.db` |
| Logs | `~/Library/Logs/MailFerry/` |
| Cache | `~/Library/Caches/MailFerry/` |

These are created **lazily** — only when an operation actually needs
them. Informational commands (`--help`, `version`, `about`, `changelog`,
`roadmap`) never create any of them. The configuration file, in
particular, is written on your first operational run (or when you
explicitly run `mailferry config`), not before.

Run `mailferry config paths` at any time to have MailFerry print the
exact paths it is using on your machine — this always reflects any
`--config`/`--db`/`--logs-dir` flags you've passed, since CLI flags take
precedence over the TOML configuration, which in turn takes precedence
over these native OS defaults.

Your migration CSV files (see [section 6](#6-first-migration-quick-start))
are not part of this table — they stay exactly where you save them.
MailFerry never moves, copies or relocates a CSV file you point it at; it
is entirely user-owned.

## 6. First migration quick-start

```sh
mailferry init mailboxes.csv        # write a template
$EDITOR mailboxes.csv               # fill in your source/destination details
mailferry check mailboxes.csv       # preflight only — no writes
mailferry mailboxes.csv             # migrate
```

See the [README](../README.md) for the full command reference, CSV
format, TUI walkthrough and resume/recovery behaviour.

---

For the production Developer ID signing and notarisation pipeline behind
official releases, see the maintainer documentation:
[`docs/RELEASING-MACOS.md`](RELEASING-MACOS.md).
