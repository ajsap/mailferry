# MailFerry — macOS Distribution & Signing

This document defines the production release pipeline for frictionless,
legitimate macOS distribution of MailFerry. It exists because the v2.0.0
release-candidate binaries trip Gatekeeper, and the fix is a proper signing
pipeline — **never** telling users to weaken macOS security.

## 1. Why the RC build triggers "Privacy & Security"

Measured on the actual RC artefacts (Mach-O load-command analysis):

| Binary | Signature state |
| --- | --- |
| `mailferry-v2.0.0-darwin-arm64` | **Ad-hoc** signature only. The Go linker auto-signs arm64 Mach-O binaries (Apple Silicon refuses to execute unsigned arm64 code at all). The signature has `CS_ADHOC` set, identifier `a.out`, and an **empty CMS blob** — no signing identity, no Developer ID, no team. |
| `mailferry-v2.0.0-darwin-amd64` | **Completely unsigned** (`LC_CODE_SIGNATURE` absent — the Go linker only auto-signs arm64). |

Neither binary is notarised, and notarisation requires a Developer ID
signature first.

What the user experiences:

1. The binary is downloaded with a browser → macOS attaches the
   `com.apple.quarantine` extended attribute.
2. On first launch, Gatekeeper assesses the quarantined file: **no
   Developer ID + no notarisation ticket** → "Apple could not verify …" →
   the app is blocked until the user allows it under **System Settings →
   Privacy & Security → Open Anyway**.
3. Files that arrive without quarantine (e.g. `scp`, `curl`) skip the
   prompt — which is why testing over SSH can behave differently from a
   browser download. This is an artefact of quarantine, not a fix.

So: the RC behaviour is *expected* for an unsigned, un-notarised download.
Nothing is broken; the release pipeline below is what removes the friction.

## 2. Production pipeline (per release)

Prerequisites (one-off):

- Apple Developer Program membership (US$99/yr) for **Andy Saputra**.
- A **Developer ID Application** certificate (created in the developer
  portal or Xcode; lives in the login keychain on the build Mac, or is
  exported as `.p12` for CI).
- An **App Store Connect API key** (Issuer ID + Key ID + `.p8`) for
  notarisation — preferred over Apple-ID app passwords for automation.

Per release, for each of `darwin-arm64` and `darwin-amd64` (or a universal
binary, §4):

```sh
# 0. Reproducible build (already done by go/build.sh: CGO_ENABLED=0,
#    -trimpath, -ldflags "-s -w -buildid=", pinned Go toolchain).

# 1. Sign with Developer ID + hardened runtime + secure timestamp,
#    and a real identifier (the Go linker default is "a.out"):
codesign --force --options runtime --timestamp \
  --identifier org.saputra.mailferry \
  --sign "Developer ID Application: Andy Saputra (TEAMID)" \
  mailferry-v2.0.0-darwin-arm64

# 2. Zip it (notarytool wants an archive) and submit for notarisation:
ditto -c -k mailferry-v2.0.0-darwin-arm64 mailferry-arm64.zip
xcrun notarytool submit mailferry-arm64.zip \
  --key AuthKey_XXXX.p8 --key-id XXXX --issuer YYYY --wait

# 3. Verify:
codesign --verify --strict --verbose=2 mailferry-v2.0.0-darwin-arm64
spctl --assess --type execute -vv mailferry-v2.0.0-darwin-arm64
```

### Stapling — know the limitation

`xcrun stapler staple` works on **apps, disk images and installer
packages — not on bare Mach-O executables.** For a bare binary the
notarisation ticket lives on Apple's servers and Gatekeeper fetches it
online on first launch (fine for almost everyone, but not offline-proof).

Recommended: **also ship a stapled `.dmg`** so offline first launches
work:

```sh
hdiutil create -volname MailFerry -srcfolder ./payload -ov -format UDZO mailferry-v2.0.0-macos.dmg
codesign --sign "Developer ID Application: Andy Saputra (TEAMID)" --timestamp mailferry-v2.0.0-macos.dmg
xcrun notarytool submit mailferry-v2.0.0-macos.dmg --key … --wait
xcrun stapler staple mailferry-v2.0.0-macos.dmg
```

Release both: the raw signed+notarised binaries (with `SHA256SUMS`) for
CLI users, and the stapled `.dmg` for a zero-friction download.

### CI without a Mac

`rcodesign` (the `apple-codesign` Rust project) performs Developer ID
signing **and** notarisation from Linux CI using the exported `.p12` and
the App Store Connect API key:

```sh
rcodesign sign --p12-file devid.p12 --p12-password-file pw \
  --code-signature-flags runtime mailferry-v2.0.0-darwin-arm64
rcodesign notary-submit --api-key-file key.json --wait mailferry-arm64.zip
```

Either path (macOS runner with `codesign`/`notarytool`, or Linux runner
with `rcodesign`) is acceptable; the artefacts are identical in effect.

## 3. What we do NOT do

- We do not tell users to run `xattr -d com.apple.quarantine`, disable
  Gatekeeper (`spctl --master-disable`), or lower security settings as a
  "solution". Those are developer/tester conveniences at best; release
  builds must pass Gatekeeper on their own merits.
- We do not distribute through channels that strip quarantine to dodge
  the check.

Interim note for pre-release testers only: right-click → Open (or
Privacy & Security → Open Anyway) is macOS's sanctioned operator override
for software they themselves chose to test. It is not a distribution
strategy.

## 4. Naming and architectures

Release names carry the version and target, nothing else:

```
mailferry-v2.0.0-darwin-arm64      macOS Apple Silicon (M1/M2/M3/M4…)
mailferry-v2.0.0-darwin-amd64      macOS Intel
mailferry-v2.0.0-linux-amd64
mailferry-v2.0.0-linux-arm64
mailferry-v2.0.0-windows-amd64.exe
mailferry-v2.0.0-windows-arm64.exe
```

The earlier RC suffix `-m2` meant *milestone 2* but read as "Apple M2
only" — retired for exactly that reason. `darwin-arm64` covers every
Apple Silicon generation.

Optionally a `mailferry-v2.0.0-darwin-universal` fat binary can be
produced (`lipo -create arm64 amd64 -output universal` on macOS, or the
pure-Go `makefat` in CI) and signed/notarised once.

## 5. Verification story for users

Every release publishes `SHA256SUMS` (and the GitHub release page shows
the digests). Users verify:

```sh
shasum -a 256 -c SHA256SUMS            # integrity
codesign --verify --strict --verbose=2 ./mailferry-v2.0.0-darwin-arm64
spctl --assess --type execute -vv ./mailferry-v2.0.0-darwin-arm64   # notarisation
```

## 6. Reproducibility

`go/build.sh` builds with `CGO_ENABLED=0 -trimpath -ldflags "-s -w
-buildid="`; with the same Go toolchain version, two builds of the same
commit produce byte-identical binaries **before signing** (signing adds
the CMS blob, so verify reproducibility on the unsigned artefacts and
publish both digests).
