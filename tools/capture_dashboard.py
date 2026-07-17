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

Capture a real MailFerry Dashboard frame and render it to docs/dashboard.svg.

Runs MailFerry under a pseudo-terminal against the in-process fake IMAP
servers, reconstructs the alternate-screen differential repaints, picks the
best live frame, and converts the ANSI output to a polished SVG screenshot.
Nothing is mocked: the pixels are the real Dashboard.
"""
from __future__ import annotations

import fcntl
import os
import pty
import re
import struct
import subprocess
import sys
import termios
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from tests.fake_imap import Account, FakeIMAPServer, Folder, ServerThread  # noqa: E402

COLS, ROWS = 112, 34

FG = "#c9d1d9"
BG = "#0d1117"
PALETTE = {31: "#ff7b72", 32: "#3fb950", 33: "#d29922", 36: "#39c5cf", 37: "#c9d1d9"}


def build_servers():
    def body(i, folder, pad):
        return (f"From: user{i}@example.com\r\nTo: dest{i}@example.com\r\n"
                f"Subject: {folder} message {i}\r\n"
                f"Date: Thu, 16 Jul 2026 10:00:00 +0000\r\n"
                f"Message-ID: <cap{folder}{i}@example.com>\r\n\r\n"
                f"Body {i}\r\n" + "X" * pad + "\r\n").encode()

    accounts = {}
    for user, sizes in (("finance", {"INBOX": (700, 2600), "Sent": (160, 1200),
                                     "Archive/2025": (420, 5200), "Archive/2024": (300, 3400)}),
                        ("sales", {"INBOX": (520, 1900), "Sent": (140, 900),
                                   "Clients/Active": (380, 4700)})):
        a = Account(user, "pw")
        for fname, (n, pad) in sizes.items():
            f = a.folders.get(fname) or Folder(fname, uidvalidity=4000 + len(a.folders))
            a.folders[fname] = f
            for i in range(n):
                f.add(body(i, fname.replace("/", "_"), pad))
        accounts[user] = a
    src = FakeIMAPServer(accounts)
    dst = FakeIMAPServer({"finance": Account("finance", "pw"),
                          "sales": Account("sales", "pw")})
    return src, dst


def run_capture(src_port, dst_port, tmp: Path) -> bytes:
    csv = tmp / "mailboxes.csv"
    csv.write_text(
        "oldhost,oldport,oldsecurity,olduser,oldpassword,"
        "newhost,newport,newsecurity,newuser,newpassword\n"
        f"127.0.0.1,{src_port},none,finance,pw,127.0.0.1,{dst_port},none,finance,pw\n"
        f"127.0.0.1,{src_port},none,sales,pw,127.0.0.1,{dst_port},none,sales,pw\n")
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))
    env = dict(os.environ)
    env.pop("COLUMNS", None)
    env.pop("LINES", None)
    env["TERM"] = "xterm-256color"
    proc = subprocess.Popen(
        [sys.executable, "-m", "mailferry", "run", str(csv),
         "--db", str(tmp / "m.db"), "--logs-dir", str(tmp / "logs"),
         "--workers", "2", "--timeout", "60"],
        stdout=slave, stderr=slave, stdin=slave, cwd=str(ROOT), env=env)
    os.close(slave)
    buf = bytearray()
    while True:
        try:
            d = os.read(master, 65536)
        except OSError:
            break
        if not d:
            break
        buf += d
    proc.wait()
    os.close(master)
    return bytes(buf)


def reconstruct_frames(raw: bytes):
    """Replay the differential alt-screen repaints into full frames."""
    text = raw.decode("utf-8", "replace")
    alt = text.find("\x1b[?1049h")
    end = text.find("\x1b[?1049l")
    if alt < 0:
        return []
    body = text[alt:end if end > 0 else len(text)]
    frames = []
    screen = [""] * ROWS
    for seg in body.split("\x1b[H")[1:]:
        seg = seg.split("\x1b[?1049l")[0]
        seg = seg.replace("\x1b[2J", "")
        rows = seg.split("\n")
        for i, row in enumerate(rows):
            if i >= ROWS:
                break
            row = row.replace("\x1b[J", "")
            if row == "":
                continue                      # unchanged row (diff repaint)
            screen[i] = row.replace("\x1b[K", "")
        frames.append(list(screen))
    return frames


def trim_to_footer(frame):
    """Cut any stale rows left below the frame's real footer (ETA line)."""
    last = None
    for i, row in enumerate(frame):
        if "Batch ETA" in row:
            last = i
    return frame[: last + 1] if last is not None else frame


def pick_frame(frames):
    best = None
    for f in frames:
        joined = "\n".join(f)
        if "RUNNING" in joined and ("MIGRATE" in joined or "SCAN" in joined):
            best = f
    return best or (frames[-1] if frames else None)


def ansi_to_svg(lines, out: Path):
    char_w, line_h, pad = 8.0, 19, 18
    width = int(COLS * char_w + pad * 2)
    height = int(len(lines) * line_h + pad * 2 + 8)
    esc = re.compile(r"\x1b\[([0-9;]*)m")

    def escape(s):
        return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")

    parts = [f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" '
             f'viewBox="0 0 {width} {height}">',
             f'<rect width="100%" height="100%" rx="8" fill="{BG}"/>',
             f'<text font-family="SFMono-Regular,Consolas,Liberation Mono,Menlo,monospace" '
             f'font-size="13" xml:space="preserve">']
    for row, line in enumerate(lines):
        y = pad + (row + 1) * line_h - 5
        fill, dim, bold = FG, False, False
        parts.append(f'<tspan x="{pad}" y="{y}">')
        pos = 0
        for m in esc.finditer(line):
            chunk = line[pos:m.start()]
            if chunk:
                col = "#8b949e" if dim else fill
                w = ' font-weight="600"' if bold else ""
                parts.append(f'<tspan fill="{col}"{w}>{escape(chunk)}</tspan>')
            for code in (m.group(1) or "0").split(";"):
                n = int(code or 0)
                if n == 0:
                    fill, dim, bold = FG, False, False
                elif n == 1:
                    bold = True
                elif n == 2:
                    dim = True
                elif n in PALETTE:
                    fill = PALETTE[n]
            pos = m.end()
        tailtxt = line[pos:]
        if tailtxt:
            col = "#8b949e" if dim else fill
            w = ' font-weight="600"' if bold else ""
            parts.append(f'<tspan fill="{col}"{w}>{escape(tailtxt)}</tspan>')
        parts.append("</tspan>")
    parts.append("</text></svg>")
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text("".join(parts), encoding="utf-8")


def main() -> int:
    import tempfile
    tmp = Path(tempfile.mkdtemp(prefix="mailferry-capture-"))
    src, dst = build_servers()
    st = ServerThread(src, dst)
    st.start()
    try:
        raw = run_capture(src.port, dst.port, tmp)
    finally:
        st.stop()
    frames = reconstruct_frames(raw)
    frame = pick_frame(frames)
    if not frame:
        print("no dashboard frame captured", file=sys.stderr)
        return 1
    frame = trim_to_footer(frame)
    while frame and not frame[-1].strip():
        frame.pop()
    ansi_to_svg(frame, ROOT / "docs" / "dashboard.svg")
    plain = "\n".join(re.sub(r"\x1b\[[0-9;]*m", "", l).rstrip() for l in frame)
    (ROOT / "docs" / "dashboard.txt").write_text(plain + "\n", encoding="utf-8")
    print(f"captured {len(frames)} frames; wrote docs/dashboard.svg and docs/dashboard.txt")
    print(plain[:600])
    return 0


if __name__ == "__main__":
    sys.exit(main())
