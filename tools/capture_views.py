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

Render the F5 History/Activity view and the stale-lock dialog headlessly
to SVG previews (docs/history.svg, docs/lockdialog.svg).
"""
import os
import shutil
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

import mailferry.progress.dashboard as dash  # noqa: E402

dash.ISATTY = True
shutil.get_terminal_size = lambda fallback=(80, 24): os.terminal_size((112, 32))

from mailferry.config import RunConfig  # noqa: E402
from mailferry.control import ControlHub  # noqa: E402
from mailferry.progress.stats import Stats  # noqa: E402
from mailferry.tui.app import TuiApp  # noqa: E402

sys.path.insert(0, str(ROOT / "tools"))
from capture_dashboard import ansi_to_svg  # noqa: E402


class _Sess:
    def log(self, m):
        pass


class _SM:
    def snapshot(self):
        return {"cpu": 23.0, "load": (1.4, 1.1, 0.9), "mem_total": 34.4e9,
                "mem_used": 21.2e9, "rss": 161e6}


def build():
    cfg = RunConfig(csv_file="robin.csv", run_id="20260718-141900", workers=4)
    stats = Stats()
    stats.csv_file, stats.db_path, stats.logs_dir, stats.workers = \
        "robin.csv", "./migration.db", "./logs", 4
    mb = stats.mailbox(1, "robin@example.org", "mail.src.example",
                       "mail.dst.example", "robin@example.org")
    mb.set(status="RUNNING", msgs_total=56595, msgs_done=48328,
           bytes_total=int(61.4e9), bytes_done=int(53.2e9),
           op="FETCH batch 12/14", current_folder="Sent Items",
           folders_total=9, folder_index=7, start_time=time.time() - 1100)
    hub = ControlHub(cfg, stats, _Sess())
    t0 = time.time()
    rows = [
        (t0 - 1105, "Run started", "OK", "-", "robin.csv · 1 mailbox(es) · run 20260718-141900"),
        (t0 - 1100, "Migration started", "OK", "robin@example.org",
         "robin@example.com → robin@example.org"),
        (t0 - 730, "Folder migrated", "OK", "robin@example.org",
         "INBOX: 21,407 new · 3,120 adopted · 0 skipped"),
        (t0 - 512, "Connection reconnect", "WARN", "robin@example.org",
         "Sent Items: attempt 1 — connection lost: read timeout"),
        (t0 - 300, "Stale detected", "WARN", "robin@example.org",
         "no progress for 5m 00s — folder Sent Items · op FETCH"),
        (t0 - 296, "Recovery attempt", "WARN", "robin@example.org",
         "reconnect 1/3 — resume from last checkpoint"),
        (t0 - 281, "Stale recovery successful", "OK", "robin@example.org",
         "transfer resumed after attempt 1 (15s)"),
        (t0 - 60, "Folder migrated", "OK", "robin@example.org",
         "Sent Items: 48,328 new · 0 adopted · 0 skipped"),
    ]
    for r in rows:
        hub.history.append(r)
    app = TuiApp(cfg, stats, hub, _SM())
    r = dash.Renderer(stats, _SM(), hub=hub)
    r.tui = app
    app.renderer = r
    return app, hub


def main():
    app, hub = build()
    app.handle_key("5")
    lines, _ = app.frame()
    ansi_to_svg(lines, ROOT / "docs" / "history.svg")
    print("wrote docs/history.svg")

    class _F:
        def done(self):
            return False
    hub.lock_prompt = {
        "owner": "Mac.genesis.saputra.org:74800:20260718-071909",
        "labels": ["robin@example.org"], "mid": 1, "db": None,
        "ts_seen": time.time() - 164, "created": time.time(),
        "deadline": time.time() + 74, "detail": False, "note": "",
        "future": _F(),
    }
    lines, _ = app.frame()
    ansi_to_svg(lines, ROOT / "docs" / "lockdialog.svg")
    hub.lock_prompt = None
    print("wrote docs/lockdialog.svg")


if __name__ == "__main__":
    main()
