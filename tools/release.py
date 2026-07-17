#!/usr/bin/env python3
"""
MailFerry - IMAP Migration & Sync
High-Performance Native IMAP Migration Engine

Copyright (C) 2026 Andy Saputra <andy@saputra.org>

https://saputra.org
https://github.com/ajsap/mailferry

Licensed under the GNU Affero General Public License v3.0 (AGPL-3.0).
This program is free software: you can redistribute it and/or modify it
under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or (at
your option) any later version.

Contributions welcome: submit issues, feature requests and pull requests
at https://github.com/ajsap/mailferry

MailFerry release builder.

Single-source versioning: reads __version__ from mailferry/__init__.py and
stamps every artefact from it. Refuses to build unless the release gates
pass:

  1. SemVer version format (pre-releases like 2.0.0-rc.1 allowed)
  2. CHANGELOG.md contains a section for this exact version
  3. LICENSE is the GNU AGPL v3 text
  4. Every Python source file carries the standard MailFerry header
  5. No third-party imports anywhere in the package
  6. Branding audit: immutable Product / Title / Slogan, no forbidden or
     superseded names, no duplicated version strings, no placeholders

Artefacts (dist/): mailferry.pyz, mailferry-<v>.pyz,
mailferry-<v>-src.tar.gz, mailferry-<v>-py3-none-any.whl, SHA256SUMS.

Usage:  python3 tools/release.py [--check-only]
"""
from __future__ import annotations

import base64
import hashlib
import re
import shutil
import subprocess
import sys
import tarfile
import zipapp
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

import mailferry  # noqa: E402

V = mailferry.__version__
SEMVER = re.compile(r"^\d+\.\d+\.\d+(?:-(?:alpha|beta|rc)\.\d+)?$")
HEADER_MARK = "Copyright (C) 2026 Andy Saputra"

STDLIB_OK = True  # import audit below

FORBIDDEN = [
    r"(?i)imap migrator", r"(?i)\bmigration tool\b", r"(?i)mail sync tool",
    r"(?i)native imap tool", r"(?i)python imap migration", r"(?i)imap copy utility",
    r"(?i)bulk migrator", r"(?i)mail migration engine", r"(?i)\bimap engine\b",
    r"A Native IMAP Migration Engine",            # superseded slogan forms
    r"A High-Performance Native IMAP Migration Engine",
    r"(?i)\blorem\b", r"PLACEHOLDER", r"1\.0\.0rc1",
]

TEXT_GLOBS = ["mailferry/**/*.py", "tests/**/*.py", "tools/**/*.py",
              "README.md", "CHANGELOG.md", "CONTRIBUTING.md", "CODE_OF_CONDUCT.md"]


def fail(msg: str):
    print(f"RELEASE BLOCKED: {msg}", file=sys.stderr)
    sys.exit(1)


def gate_version():
    if not SEMVER.match(V):
        fail(f"__version__ {V!r} is not valid SemVer")
    hits = []
    for pat in ("mailferry/**/*.py",):
        for p in ROOT.glob(pat):
            if p.name == "__init__.py" and p.parent.name == "mailferry":
                continue
            t = p.read_text(encoding="utf-8")
            if "__version__ =" in t or f'"{V}"' in t or f"'{V}'" in t:
                hits.append(str(p.relative_to(ROOT)))
    if hits:
        fail(f"duplicated version strings in: {', '.join(hits)} "
             "(the version lives only in mailferry/__init__.py)")
    print(f"  ok  version {V} (single source)")


def gate_changelog():
    text = (ROOT / "CHANGELOG.md").read_text(encoding="utf-8")
    if f"## [{V}]" not in text:
        fail(f"CHANGELOG.md has no section '## [{V}]' — add it before releasing")
    print("  ok  changelog entry present")


def gate_license():
    lic = ROOT / "LICENSE"
    if not lic.exists() or "GNU AFFERO GENERAL PUBLIC LICENSE" not in lic.read_text(encoding="utf-8")[:400]:
        fail("LICENSE missing or not the GNU AGPL v3 text")
    print("  ok  LICENSE is GNU AGPL v3")


def gate_headers():
    missing = []
    for pat in ("mailferry/**/*.py", "tests/**/*.py", "tools/**/*.py"):
        for p in sorted(ROOT.glob(pat)):
            if "__pycache__" in p.parts:
                continue
            if HEADER_MARK not in p.read_text(encoding="utf-8")[:2000]:
                missing.append(str(p.relative_to(ROOT)))
    if missing:
        fail(f"source header missing in: {', '.join(missing)} "
             "(run: python3 tools/apply_headers.py)")
    print("  ok  AGPL headers on every source file")


def gate_stdlib_only():
    import ast
    allowed_local = {"mailferry", "tests", "tools"}
    bad = []
    stdlib = getattr(sys, "stdlib_module_names", None)
    for p in sorted(ROOT.glob("mailferry/**/*.py")):
        tree = ast.parse(p.read_text(encoding="utf-8"))
        for node in ast.walk(tree):
            names = []
            if isinstance(node, ast.Import):
                names = [a.name.split(".")[0] for a in node.names]
            elif isinstance(node, ast.ImportFrom) and node.level == 0 and node.module:
                names = [node.module.split(".")[0]]
            for n in names:
                if n in allowed_local:
                    continue
                if stdlib is not None and n not in stdlib:
                    bad.append(f"{p.relative_to(ROOT)}: {n}")
    if bad:
        fail("third-party imports detected: " + "; ".join(bad))
    print("  ok  standard library only")


def gate_branding():
    problems = []
    for pat in TEXT_GLOBS:
        for p in sorted(ROOT.glob(pat)):
            if "__pycache__" in p.parts or p.name == "release.py":
                continue      # this file carries the forbidden patterns themselves
            text = p.read_text(encoding="utf-8")
            for rx in FORBIDDEN:
                for m in re.finditer(rx, text):
                    # allow the superseded-slogan regexes to match nothing else
                    problems.append(f"{p.relative_to(ROOT)}: forbidden text {m.group(0)!r}")
    if mailferry.PRODUCT != "MailFerry":
        problems.append("PRODUCT constant altered")
    if mailferry.TITLE != "MailFerry – IMAP Migration & Sync":
        problems.append("TITLE constant altered")
    if mailferry.SLOGAN != "High-Performance Native IMAP Migration Engine":
        problems.append("SLOGAN constant altered")
    if problems:
        fail("branding audit failed:\n  " + "\n  ".join(problems))
    print("  ok  branding audit (product/title/slogan immutable, no forbidden names)")


def build():
    dist = ROOT / "dist"
    build_dir = ROOT / "build"
    shutil.rmtree(dist, ignore_errors=True)
    shutil.rmtree(build_dir, ignore_errors=True)
    dist.mkdir()
    stage = build_dir / "app"
    shutil.copytree(ROOT / "mailferry", stage / "mailferry",
                    ignore=shutil.ignore_patterns("__pycache__"))
    pyz = dist / "mailferry.pyz"
    zipapp.create_archive(stage, pyz, interpreter="/usr/bin/env python3",
                          main="mailferry.cli:console")
    pyz.chmod(0o755)
    shutil.copy2(pyz, dist / f"mailferry-{V}.pyz")

    # source archive
    src_tar = dist / f"mailferry-{V}-src.tar.gz"
    with tarfile.open(src_tar, "w:gz") as tar:
        for item in ("mailferry", "tests", "tools", "docs", "README.md", "CHANGELOG.md",
                     "CONTRIBUTING.md", "CODE_OF_CONDUCT.md", "LICENSE", ".gitignore"):
            p = ROOT / item
            if p.exists():
                tar.add(p, arcname=f"mailferry-{V}/{item}",
                        filter=lambda ti: None if "__pycache__" in ti.name else ti)

    # wheel (pure stdlib construction)
    whl = dist / f"mailferry-{V}-py3-none-any.whl"
    di = f"mailferry-{V}.dist-info"
    records = []

    def add(zf, arcname, data: bytes):
        zf.writestr(arcname, data)
        digest = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
        records.append(f"{arcname},sha256={digest},{len(data)}")

    with zipfile.ZipFile(whl, "w", zipfile.ZIP_DEFLATED) as zf:
        for p in sorted((stage / "mailferry").rglob("*.py")):
            add(zf, f"mailferry/{p.relative_to(stage / 'mailferry')}", p.read_bytes())
        meta = (f"Metadata-Version: 2.1\nName: mailferry\nVersion: {V}\n"
                f"Summary: {mailferry.TITLE} — {mailferry.SLOGAN}\n"
                f"Home-page: {mailferry.REPOSITORY}\n"
                f"Author: {mailferry.AUTHOR}\nAuthor-email: {mailferry.AUTHOR_EMAIL}\n"
                f"License: AGPL-3.0-or-later\n"
                f"Project-URL: Repository, {mailferry.REPOSITORY}\n"
                f"Project-URL: Issues, {mailferry.SUPPORT_URL}\n"
                f"Requires-Python: >=3.9\n\n"
                f"{mailferry.PRODUCT} — see {mailferry.REPOSITORY}\n").encode()
        add(zf, f"{di}/METADATA", meta)
        add(zf, f"{di}/WHEEL", b"Wheel-Version: 1.0\nGenerator: mailferry-release\n"
                               b"Root-Is-Purelib: true\nTag: py3-none-any\n")
        add(zf, f"{di}/entry_points.txt", b"[console_scripts]\nmailferry = mailferry.cli:console\n")
        add(zf, f"{di}/LICENSE", (ROOT / "LICENSE").read_bytes())
        records.append(f"{di}/RECORD,,")
        zf.writestr(f"{di}/RECORD", "\n".join(records) + "\n")

    # checksums
    sums = []
    for p in sorted(dist.iterdir()):
        if p.name == "SHA256SUMS":
            continue
        sums.append(f"{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}")
    (dist / "SHA256SUMS").write_text("\n".join(sums) + "\n", encoding="utf-8")

    # smoke: the artefact must announce the right identity
    out = subprocess.run([sys.executable, str(pyz), "--version"],
                         capture_output=True, text=True, timeout=30).stdout
    expect = f"{mailferry.PRODUCT} v{V} — IMAP Migration & Sync"
    if not out.startswith(expect):
        fail(f"built artefact reports {out.splitlines()[:1]!r}, expected {expect!r}")
    print(f"  ok  artefacts built and version-stamped ({V})")
    for line in sums:
        print("      " + line)


def main() -> int:
    check_only = "--check-only" in sys.argv
    print(f"MailFerry release gates — v{V}")
    gate_version()
    gate_changelog()
    gate_license()
    gate_headers()
    gate_stdlib_only()
    gate_branding()
    if check_only:
        print("All release gates passed (check-only).")
        return 0
    build()
    print("Release build complete: dist/")
    return 0


if __name__ == "__main__":
    sys.exit(main())
