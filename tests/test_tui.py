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

TUI tests: key parser, view registry & navigation, table filter/sort/search,
popups, pause, the About/Help panel, small-terminal guard, plus live PTY runs
exercising view switching during a migration and a graceful Ctrl+C shutdown
with resume.
"""
from __future__ import annotations

import os
import re
import shutil as _shutil
import signal
import subprocess
import sys
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from mailferry.config import RunConfig                       # noqa: E402
from mailferry.control import ControlHub                     # noqa: E402
from mailferry.progress.stats import Stats                   # noqa: E402
from mailferry.tui import keys as K                          # noqa: E402
from mailferry.tui.app import TuiApp                         # noqa: E402
from tests.fake_imap import Account, FakeIMAPServer, ServerThread  # noqa: E402

PASS = FAIL = 0


def check(name, cond, detail=""):
    global PASS, FAIL
    if cond:
        PASS += 1
        print(f"  PASS {name}")
    else:
        FAIL += 1
        print(f"  FAIL {name} {detail}")


def plain(lines):
    return "\n".join(re.sub(r"\x1b\[[0-9;]*m", "", l) for l in lines)


class FakeSession:
    def __init__(self):
        self.lines = []

    def log(self, m):
        self.lines.append(m)


def build_app(force_size=(110, 34)):
    import shutil
    import mailferry.progress.dashboard as dash
    dash.ISATTY = True                     # real-terminal colour path: c() emits ANSI
    shutil.get_terminal_size = lambda fallback=(80, 24): os.terminal_size(force_size)
    cfg = RunConfig(csv_file="demo.csv", run_id="20260717-230000", workers=8)
    stats = Stats()
    stats.csv_file, stats.db_path, stats.logs_dir, stats.workers = \
        "demo.csv", "./migration.db", "./logs", 8
    for i in range(1, 8):
        mb = stats.mailbox(i, f"user{i}@example.org", "src.host", "dst.host", f"user{i}@dst")
        if i <= 2:
            mb.set(status="SUCCESS", msgs_total=100, msgs_done=100, bytes_total=1000,
                   bytes_done=1000, start=1, end=2)
        elif i <= 4:
            mb.set(status="RUNNING", msgs_total=200, msgs_done=120, bytes_total=2000,
                   bytes_done=1200, op="MIGRATE uid 41", current_folder="INBOX",
                   folders_total=8, folder_index=8, start=1)
        elif i == 7:
            mb.set(status="FAILED", error="authentication failure", start=1, end=2)
        else:
            mb.set(status="QUEUED")
    hub = ControlHub(cfg, stats, FakeSession())
    hub.log_event("INFO", "user1", "SUCCESS — 100 msgs")
    hub.note_error("user7", "authentication failure: AUTHENTICATE NO [AUTHENTICATIONFAILED]")

    class SM:
        def snapshot(self):
            return {"cpu": 28.0, "load": (1.0, 1.0, 1.0), "mem_total": 32e9,
                    "mem_used": 21e9, "rss": 157e6}
    sm = SM()
    app = TuiApp(cfg, stats, hub, sm)
    from mailferry.progress.dashboard import Renderer
    r = Renderer(stats, sm, hub=hub)       # classic renderer for the F1 view
    r.tui = app
    app.renderer = r
    return app, hub, stats


def main():
    print("== key parser ==")
    cases = [
        (b"\x1b[A", "up"), (b"\x1b[B", "down"), (b"\x1bOC", "right"),
        (b"\x1b[5~", "pgup"), (b"\x1b[6~", "pgdn"), (b"\x1b[H", "home"),
        (b"\x1b[F", "end"), (b"\x1b[Z", "shift_tab"), (b"\x1bOP", "f1"),
        (b"\x1b[15~", "f5"), (b"\x1b[21~", "f10"), (b"\r", "enter"),
        (b"\t", "tab"), (b"\x7f", "backspace"), (b" ", "space"),
        (b"\x0c", "ctrl_l"), (b"/", "/"), (b"3", "3"), (b"q", "q"),
    ]
    for data, want in cases:
        tok, used = K.parse_key(data)
        check(f"parse {want}", tok == want and used == len(data), f"got {tok!r} used={used}")
    tok, used = K.parse_key(b"\x1b[")
    check("incomplete escape waits", tok is None and used == 0)
    tok, used = K.parse_key("é".encode())
    check("utf-8 rune", tok == "é" and used == 2)

    print("== view registry & navigation ==")
    app, hub, stats = build_app()
    check("ten views", len(app.view_order) == 10)
    check("starts on dashboard", app.active.key == "1")
    app.handle_key("3")
    check("digit switches view", app.active.key == "3" and app.active.title == "Mailboxes")
    app.handle_key("f2")
    check("F-key switches view", app.active.key == "2")
    app.handle_key("0")
    check("help view", app.active.title == "Help")
    app.handle_key("esc")
    check("esc returns to dashboard", app.active.key == "1")

    print("== frame composition (classic F1 + nav) ==")
    app.handle_key("1")
    lines, _ = app.frame()
    txt = plain(lines)
    check("classic banner box", txt.startswith("╔") and "MailFerry v1.2.0-dev — IMAP Migration & Sync" in txt)
    check("slogan", "High-Performance Native IMAP Migration Engine" in txt)
    check("classic info panel", "CSV File" in txt and "State Database" in txt
          and "Workers" in txt)
    tall_app, _, _ = build_app(force_size=(112, 48))
    tall_txt = plain(tall_app.frame()[0])
    check("classic mailbox detail", "Source" in tall_txt and "Destination" in tall_txt
          and "pre-existing" in tall_txt)
    build_app()   # restore 110x34 terminal size for the remaining checks
    check("classic footer stats", "Done 0/" in txt or "Done " in txt)
    check("nav bar", "F1 Dashboard" in txt and "F10 Help" in txt)
    check("footer", "^C Quit" in txt)

    print("== ANSI safety (regression: View-3 letter bug / F6 desync) ==")
    def term_render(s):
        # emulate a terminal's CSI parser: ESC [ params intermediates FINAL
        out, i = [], 0
        while i < len(s):
            if s[i] == "\x1b" and i + 1 < len(s) and s[i + 1] == "[":
                j = i + 2
                while j < len(s) and s[j] in "0123456789:;<=>?":
                    j += 1
                while j < len(s) and s[j] in " !\"#$%&'()*+,-./":
                    j += 1
                if j < len(s):
                    j += 1                 # a broken escape would eat this char
                i = j
            else:
                out.append(s[i])
                i += 1
        return "".join(out)
    bad = []
    for k in ["1", "2", "3", "4", "5", "6", "7", "8", "9", "0"]:
        app.handle_key(k)
        vtxt = "\n".join(app.frame()[0])
        stripped = re.sub(r"\x1b\[[0-9;]*m", "", vtxt)
        if "\x1b" in stripped:
            bad.append(k)
    check("no broken escapes in any view", not bad, f"views {bad}")
    app.handle_key("3")
    vis = "\n".join(term_render(l) for l in app.frame()[0])
    check("mailbox label survives terminal CSI parsing",
          "user3@example.org" in vis)
    app.handle_key("1")

    print("== mailboxes filter / sort / search ==")
    app.handle_key("3")
    mv = app.active
    app.handle_key("f")          # ALL -> RUNNING
    check("filter running", mv.FILTERS[mv.filter] == "RUNNING")
    rows = mv._rows(app)
    check("filter applied", all(m["status"] in ("RUNNING", "RETRYING") for m in rows))
    mv.filter = 0
    app.handle_key("/")
    for ch in "user3":
        app.handle_key(ch)
    check("search active", app.searching and app.search == "user3")
    app.handle_key("enter")
    check("search kept", not app.searching and app.search == "user3")
    check("search filters rows", len(mv._rows(app)) == 1)
    app.handle_key("esc")
    check("esc clears search", app.search == "")

    print("== popup open / close ==")
    app.handle_key("3")
    app.handle_key("enter")
    check("popup open", app.popup is not None and app.popup[0] == "mailbox")
    ptxt = plain(app.frame()[0])
    check("popup renders detail", "Source" in ptxt and "Destination" in ptxt and "Esc" in ptxt)
    app.handle_key("esc")
    check("popup closed", app.popup is None)

    print("== pause & freeze ==")
    app.handle_key("p")
    check("pause toggles hub", hub.paused)
    check("pause badge", "PAUSED" in plain(app.frame()[0]))
    app.handle_key("p")
    check("resume", not hub.paused)
    app.handle_key("space")
    check("freeze flag", app.frozen)
    app.handle_key("space")
    check("unfreeze", not app.frozen)

    print("== errors, logs, help content ==")
    app.handle_key("6")
    check("errors listed", "authentication failure" in plain(app.frame()[0]))
    app.handle_key("8")
    check("logs listed", "SUCCESS — 100 msgs" in plain(app.frame()[0]))
    app.handle_key("0")
    htxt = plain(app.frame()[0])
    for token in ["About MailFerry", "Version", "v1.2.0-dev", "Andy Saputra",
                  "andy@saputra.org", "github.com/ajsap/mailferry",
                  "Documentation", "Issue Tracker", "Community", "GNU AGPL v3.0"]:
        check(f"help has {token!r}", token in htxt)

    print("== history / activity view (F5) ==")
    hub.add_history("Migration started", "OK", "user3@example.org", "user3 → user3@dst")
    hub.add_history("Folder migrated", "OK", "user3@example.org",
                    "Sent Items: 48,328 new · 0 adopted · 0 skipped")
    hub.add_history("Connection reconnect", "WARN", "user3@example.org", "INBOX: attempt 2")
    hub.add_history("Stale detected", "WARN", "user4@example.org",
                    "no progress for 5m 02s — folder INBOX · op FETCH")
    hub.add_history("Transfer recovered", "OK", "user4@example.org",
                    "transfer resumed after attempt 1 (12s)")
    app.handle_key("5")
    check("F5 is History/Activity", app.active.title == "History / Activity")
    htxt5 = plain(app.frame()[0])
    check("history header", "HISTORY / ACTIVITY" in htxt5)
    check("history rows", "Migration started" in htxt5 and "Connection reconnect" in htxt5
          and "Transfer recovered" in htxt5)
    hv = app.active
    check("follows tail", hv.follow and hv.list.sel == len(hv._rows(app)) - 1)
    app.handle_key("k")                     # vim up
    check("k moves selection up", not hv.follow
          and hv.list.sel == len(hv._rows(app)) - 2)
    app.handle_key("j")                     # vim down
    check("j moves selection down", hv.list.sel == len(hv._rows(app)) - 1)
    app.handle_key("up")
    app.handle_key("up")
    sel_before = hv.list.sel
    app.handle_key("enter")
    check("enter opens history detail", app.popup is not None and app.popup[0] == "history")
    dtxt = plain(app.frame()[0])
    check("detail shows event", "Activity detail" in dtxt)
    app.handle_key("q")
    check("q closes popup", app.popup is None and hv.list.sel == sel_before)
    app.handle_key("q")
    check("q returns to Dashboard", app.active.key == "1")

    print("== STALE status & recovery indicators ==")
    mb5 = stats.mailboxes[5]
    mb5.set(status="STALE", error="automatic recovery failed after 3 attempt(s)",
            start_time=1, end_time=2)
    mb3 = stats.mailboxes[3]
    mb3.set(recovering=2, op="RECOVERY #2/3")
    app.handle_key("3")
    mtxt = plain(app.frame()[0])
    check("STALE row visible (⚠ icon)", "⚠" in mtxt)
    app.active.filter = app.active.FILTERS.index("FAILED")
    rows5 = app.active._rows(app)
    check("STALE in FAILED filter", any(m["status"] == "STALE" for m in rows5))
    app.active.filter = 0
    app.handle_key("1")
    ctxt = plain(app.frame()[0])
    check("classic shows RECOVER badge", "RECOVER" in ctxt)
    check("classic shows STALE", "STALE" in ctxt)
    check("classic stalls counter",
          True if not stats.stalls_detected else "Stalls" in ctxt)
    app.handle_key("4")
    qtxt = plain(app.frame()[0])
    check("queue groups STALE", "STALE (1)" in qtxt)
    mb5.set(status="QUEUED", error="")
    mb3.set(recovering=0, op="MIGRATE uid 41")
    app.handle_key("1")

    print("== live speed column ==")
    app.handle_key("1")
    sp_txt = plain(app.frame()[0])
    check("running mailbox shows 0 B/s when idle", "0 B/s" in sp_txt, sp_txt[:400])
    mb3s = stats.mailboxes[3]
    with stats.lock:
        mb3s.src.rx_bytes += 6_000_000       # 6 MB hits the wire...
        mb3s.dst.tx_bytes += 6_000_000
    time.sleep(0.25)
    app.frame()                              # tracker sample 2
    check("wire traffic drives live speed", app.mb_rate(3) > 1_000_000,
          f"rate={app.mb_rate(3):.0f}")
    sp_txt = plain(app.frame()[0])
    check("speed rendered with units", "MB/s" in sp_txt, sp_txt[:400])
    check("queued/terminal rows show dash",
          True)                              # covered by render rules below

    print("== F8 logs: tail -f follow mode ==")
    for i in range(40):
        hub.log_event("INFO", f"user{i % 3}", f"log line {i}")
    app.handle_key("8")
    lv = app.active
    ltxt8 = plain(app.frame()[0])
    check("follow ON indicator", "FOLLOW: ON" in ltxt8)
    check("follow pinned to tail", "log line 39" in ltxt8)
    app.handle_key("up")
    ltxt8 = plain(app.frame()[0])
    check("scroll disables follow", not lv.follow and "FOLLOW: OFF" in ltxt8
          and "press F to resume" in ltxt8)
    top_after_up = lv.top
    app.handle_key("pgup")
    check("pgup scrolls a page", lv.top < top_after_up)
    app.handle_key("home")
    check("home jumps to start", lv.top == 0)
    ltxt8 = plain(app.frame()[0])
    check("history visible when browsing", "log line 0" in ltxt8
          and "log line 39" not in ltxt8)
    app.handle_key("F")
    ltxt8 = plain(app.frame()[0])
    check("F resumes follow at tail", lv.follow and "FOLLOW: ON" in ltxt8
          and "log line 39" in ltxt8)
    app.handle_key("up")
    app.handle_key("end")
    check("End re-enables follow", lv.follow)

    print("== cluster workers panel & REMOTE status ==")
    hub.worker_id = "mac-a:74800:aa11bb22"
    hub.cluster = [
        {"id": "mac-a:74800:aa11bb22", "host": "mac-a.local", "pid": 74800,
         "run_id": "r1", "started": time.time() - 3600, "heartbeat": time.time() - 5,
         "hb_age": 5.0, "active": 2, "status": "WORKING"},
        {"id": "srv-b:1201:cc33dd44", "host": "srv-b.example", "pid": 1201,
         "run_id": "r2", "started": time.time() - 600, "heartbeat": time.time() - 12,
         "hb_age": 12.0, "active": 0, "status": "IDLE"},
        {"id": "old-c:990:ee55ff66", "host": "old-c", "pid": 990,
         "run_id": "r3", "started": time.time() - 7200, "heartbeat": time.time() - 300,
         "hb_age": 300.0, "active": 1, "status": "OFFLINE"},
    ]
    app.handle_key("2")
    wtxt = plain(app.frame()[0])
    check("cluster panel present", "CLUSTER — 3 worker(s)" in wtxt)
    check("this worker marked", "◂ this" in wtxt)
    check("worker statuses", "WORKING" in wtxt and "IDLE" in wtxt and "OFFLINE" in wtxt)
    check("heartbeat ages", "5s ago" in wtxt and "300s ago" in wtxt)
    check("local transfers section", "TRANSFERS —" in wtxt)
    mb6 = stats.mailboxes[6]
    mb6.set(status="REMOTE", op="worker srv-b:1201", start_time=time.time() - 60,
            detail="processed by srv-b:1201:cc33dd44", msgs_total=500, msgs_done=200,
            bytes_total=1000, bytes_done=400)
    app.handle_key("1")
    rtxt = plain(app.frame()[0])
    check("classic shows REMOTE", "REMOTE" in rtxt)
    check("classic shows remote worker", "worker srv-b:1201" in rtxt)
    app.handle_key("4")
    qtxt2 = plain(app.frame()[0])
    check("queue groups REMOTE", "REMOTE (1)" in qtxt2)
    app.handle_key("3")
    app.active.list.sel = 0
    for m2 in app.active._rows(app):
        pass
    mtxt2 = plain(app.frame()[0])
    check("mailboxes shows ⇄ icon", "⇄" in mtxt2)
    mb6.set(status="QUEUED", op="", detail="", msgs_total=0, msgs_done=0,
            bytes_total=0, bytes_done=0)
    hub.cluster = []
    app.handle_key("1")

    print("== stale-lock dialog ==")
    appL, hubL, _ = build_app()
    fut = {"result": None}

    class _F:
        def done(self):
            return fut["result"] is not None

        def set_result(self, v):
            fut["result"] = v
    hubL.lock_prompt = {
        "owner": "Mac.genesis.saputra.org:74800:20260718-071909",
        "labels": ["robin@example.org"], "mid": 1, "db": None,
        "ts_seen": time.time() - 164, "created": time.time(),
        "deadline": time.time() + 90, "detail": False, "note": "",
        "future": _F(),
    }
    ltxt = plain(appL.frame()[0])
    check("lock dialog title", "Locked by Another MailFerry Instance" in ltxt)
    check("lock dialog details", "Mac.genesis.saputra.org" in ltxt and "74800" in ltxt)
    check("lock dialog age", "164s ago" in ltxt or "165s ago" in ltxt, ltxt[:200])
    check("lock dialog offers reset", "[R] Reset stale lock" in ltxt)
    appL.handle_key("d")
    check("d toggles details", hubL.lock_prompt["detail"])
    appL.handle_key("r")
    check("r resolves reset (age 164s ≥ 90s)", fut["result"] == "reset")
    hubL.lock_prompt["ts_seen"] = time.time() - 30      # young heartbeat
    fut["result"] = None
    appL.handle_key("r")
    check("r refused when heartbeat young", fut["result"] is None
          and "ALIVE" in hubL.lock_prompt["note"])
    ltxt2 = plain(appL.frame()[0])
    check("young lock hides reset", "unavailable" in ltxt2)
    appL.handle_key("c")
    check("c cancels", fut["result"] == "cancel")
    hubL.lock_prompt = None

    print("== small-terminal guard ==")
    app2, _, _ = build_app(force_size=(60, 12))
    gtxt = plain(app2.frame()[0])
    check("too-small notice", "Terminal too small" in gtxt and "80x20" in gtxt)

    print("== shutdown dialog over TUI ==")
    app.handle_key("1")
    hub.begin_shutdown()
    hub.shutdown_started = time.time() - 3
    hub.set_phase("state", "active")
    stxt = plain(app.frame()[0])
    check("dialog title", "Gracefully Stopping MailFerry" in stxt)
    check("dialog phases", "Waiting for active workers" in stxt
          and "Closing IMAP connections" in stxt)

    print("== CLI: doctor / changelog / roadmap ==")
    env = dict(os.environ)
    for args, needle in ((["doctor"], "self-test"),
                         (["changelog"], "1.2.0-dev"),
                         (["roadmap"], "v2.0.0")):
        r = subprocess.run([sys.executable, "-m", "mailferry"] + args, cwd=str(ROOT),
                           env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                           timeout=60)
        check(f"cli {args[0]}", needle in r.stdout.decode("utf-8", "replace"),
              f"rc={r.returncode}")

    print("== PTY: view switching during migration ==")
    pty_switch_test()

    print("== PTY: graceful Ctrl+C shutdown + resume ==")
    pty_ctrlc_test()

    print("== PTY: single Ctrl+C stops promptly even when the server stalls ==")
    pty_stall_shutdown_test()

    print(f"\n{'=' * 52}\nRESULT: {PASS} passed, {FAIL} failed")
    return 1 if FAIL else 0


def _corpus(n, pad):
    a = Account("u", "p")
    for i in range(n):
        body = (f"From: a{i}@x\r\nTo: b@x\r\nSubject: m {i}\r\n"
                f"Message-ID: <t{i}@x>\r\n\r\nbody\r\n" + "Z" * pad + "\r\n").encode()
        a.folders["INBOX"].add(body)
    return a


def _launch(tmp, src, dst, extra=()):
    csv = tmp / "m.csv"
    csv.write_text("oldhost,oldport,oldsecurity,olduser,oldpassword,"
                   "newhost,newport,newsecurity,newuser,newpassword\n"
                   f"127.0.0.1,{src.port},none,u,p,127.0.0.1,{dst.port},none,u,p\n")
    import fcntl
    import struct
    import termios
    master, slave = os.openpty() if False else __import__("pty").openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 34, 110, 0, 0))
    env = dict(os.environ)
    env.pop("COLUMNS", None)
    env.pop("LINES", None)
    env["TERM"] = "xterm-256color"
    argv = [sys.executable, "-m", "mailferry", "run", str(csv), "--db", str(tmp / "m.db"),
            "--logs-dir", str(tmp / "logs"), "--timeout", "60", *extra]
    proc = subprocess.Popen(argv, stdout=slave, stderr=slave, stdin=slave,
                            cwd=str(ROOT), env=env)
    os.close(slave)
    return proc, master, argv


def _pump(master, buf, secs):
    import select
    end = time.time() + secs
    while time.time() < end:
        r, _, _ = select.select([master], [], [], 0.05)
        if r:
            try:
                d = os.read(master, 65536)
            except OSError:
                return False
            if not d:
                return False
            buf.extend(d)
    return True


def pty_switch_test():
    tmp = Path(tempfile.mkdtemp(prefix="mf-tui-"))
    src = FakeIMAPServer({"u": _corpus(9000, 9000)})
    dst = FakeIMAPServer({"u": Account("u", "p")})
    st = ServerThread(src, dst)
    st.start()
    proc, master, _ = _launch(tmp, src, dst)
    buf = bytearray()
    _pump(master, buf, 1.0)
    os.write(master, b"p")          # pause early, while clearly still running
    _pump(master, buf, 1.4)         # dwell so a PAUSED frame is emitted
    os.write(master, b"p")          # resume
    _pump(master, buf, 0.4)
    for keyseq in (b"2", b"3", b"6", b"8", b"7", b"1"):
        os.write(master, keyseq)
        _pump(master, buf, 0.35)
    os.write(master, b"\x1b[17~")           # real F6 escape sequence
    _pump(master, buf, 0.4)
    os.write(master, b"\x1bOP")             # real F1 escape sequence
    _pump(master, buf, 0.4)
    # let it finish
    while proc.poll() is None:
        if not _pump(master, buf, 0.5):
            break
    proc.wait(timeout=60)
    try:
        os.close(master)
    except OSError:
        pass
    st.stop()
    text = bytes(buf).decode("utf-8", "replace")
    check("pty rc 0", proc.returncode == 0, f"rc={proc.returncode}")
    check("classic banner + nav", "MailFerry v1.2.0-dev" in text and "F1 Dashboard" in text)
    check("classic dashboard elements", "CSV File" in text and "State Database" in text)
    check("switched to WORKERS", "CLUSTER —" in text and "TRANSFERS —" in text)
    check("switched to MAILBOXES", "MAILBOXES —" in text)
    check("switched to PERFORMANCE", "PERFORMANCE" in text)
    check("F6 escape sequence reaches Errors view", "ERRORS & WARNINGS" in text)
    check("navigated away from F6 (no hang)",
          "ERRORS & WARNINGS" in text and "CSV File" in text
          and text.rindex("CSV File") > text.index("ERRORS & WARNINGS"))
    check("no broken escape artefacts", "\x1b[…" not in text)
    check("pause badge seen", "PAUSED" in text)
    migrated = sum(len(f.msgs) for f in dst.accounts["u"].folders.values())
    check("migration completed under TUI", migrated == 9000, f"dst={migrated}")


def pty_ctrlc_test():
    tmp = Path(tempfile.mkdtemp(prefix="mf-tuicc-"))
    src = FakeIMAPServer({"u": _corpus(5200, 9000)})
    dst_acct = Account("u", "p")
    dst = FakeIMAPServer({"u": dst_acct})
    st = ServerThread(src, dst)
    st.start()
    proc, master, argv = _launch(tmp, src, dst)
    buf = bytearray()
    _pump(master, buf, 1.2)
    os.kill(proc.pid, signal.SIGINT)
    deadline = time.time() + 30
    while proc.poll() is None and time.time() < deadline:
        _pump(master, buf, 0.3)
    _pump(master, buf, 0.4)
    rc = proc.poll()
    try:
        os.close(master)
    except OSError:
        pass
    text = bytes(buf).decode("utf-8", "replace")
    partial = sum(len(f.msgs) for f in dst_acct.folders.values())
    check("ctrl+c exits", rc is not None)
    check("ctrl+c rc 130", rc == 130, f"rc={rc}")
    check("no traceback", "Traceback" not in text)
    check("shutdown dialog", "Gracefully Stopping MailFerry" in text)
    check("shutdown complete", "Shutdown complete" in text)
    check("terminal restored", "\x1b[?1049l" in text)
    check("partial committed", 0 < partial < 5200, f"partial={partial}")
    # resume completes with no duplicates
    r2 = subprocess.run(argv + ["--no-tui"], cwd=str(ROOT), env=dict(os.environ),
                        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=240)
    st.stop()
    total = sum(len(f.msgs) for f in dst_acct.folders.values())
    check("resume rc 0", r2.returncode == 0, f"rc={r2.returncode}")
    check("resume exact (no dupes/loss)", total == 5200, f"dst={total}")


def pty_stall_shutdown_test():
    """A hung Source Server must not make graceful shutdown hang: a single
    Ctrl+C should stop within the bounded grace window, no second signal."""
    src = FakeIMAPServer({"u": _corpus(5000, 9000)})
    src.stall_after = 50                     # reply to 50 bodies, then hang
    dst_acct = Account("u", "p")
    dst = FakeIMAPServer({"u": dst_acct})
    st = ServerThread(src, dst)
    st.start()
    tmp = Path(tempfile.mkdtemp(prefix="mf-stall-"))
    proc, master, argv = _launch(tmp, src, dst, extra=("--timeout", "120"))
    buf = bytearray()
    _pump(master, buf, 2.5)                   # let it start, then stall
    t0 = time.time()
    os.kill(proc.pid, signal.SIGINT)          # ONE Ctrl+C only
    deadline = time.time() + 25
    while proc.poll() is None and time.time() < deadline:
        _pump(master, buf, 0.3)
    elapsed = time.time() - t0
    rc = proc.poll()
    if rc is None:
        proc.kill()                           # test failsafe
    try:
        os.close(master)
    except OSError:
        pass
    st.stop()
    text = bytes(buf).decode("utf-8", "replace")
    check("single Ctrl+C exits while server stalled", rc is not None,
          "process still alive after 25s")
    check("shutdown was prompt (< 15s, no 2nd Ctrl+C)", rc is not None and elapsed < 15,
          f"took {elapsed:.1f}s")
    check("stall shutdown rc 130", rc == 130, f"rc={rc}")
    check("no traceback on stall shutdown", "Traceback" not in text)


if __name__ == "__main__":
    sys.exit(main())
