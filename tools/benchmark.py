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

Loopback benchmark: migrate a synthetic corpus between two in-process fake
IMAP servers and report messages/s, MB/s, duration, workers and peak RSS.
Numbers are a relative baseline (loopback, no network latency), not a WAN
figure. Usage: python3 tools/benchmark.py [--messages N] [--size-kb K]
[--workers W] [--mailboxes M]
"""
from __future__ import annotations

import argparse
import os
import resource
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

import mailferry  # noqa: E402
from mailferry.config import RunConfig  # noqa: E402
from mailferry.progress.stats import Stats  # noqa: E402
from mailferry.report import Session, logger_factory  # noqa: E402
from tests.fake_imap import Account, FakeIMAPServer, ServerThread  # noqa: E402


def build(messages, size_kb, mailboxes):
    pad = "X" * (size_kb * 1024)
    total_bytes = 0
    src_accounts = {}
    dst_accounts = {}
    for n in range(mailboxes):
        user = f"user{n}"
        a = Account(user, "p")
        for i in range(messages):
            body = (f"From: a{i}@x\r\nTo: b{i}@x\r\nSubject: bench {i}\r\n"
                    f"Message-ID: <bench{n}-{i}@x>\r\n\r\n{pad}\r\n").encode()
            a.folders["INBOX"].add(body)
            total_bytes += len(body)
        src_accounts[user] = a
        dst_accounts[user] = Account(user, "p")
    return src_accounts, dst_accounts, total_bytes


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--messages", type=int, default=5000)
    ap.add_argument("--size-kb", type=int, default=8)
    ap.add_argument("--workers", type=int, default=4)
    ap.add_argument("--mailboxes", type=int, default=4)
    args = ap.parse_args()

    import tempfile
    tmp = Path(tempfile.mkdtemp(prefix="mf-bench-"))
    src_accounts, dst_accounts, total_bytes = build(args.messages, args.size_kb, args.mailboxes)
    s_src = FakeIMAPServer(src_accounts)
    s_dst = FakeIMAPServer(dst_accounts)
    st = ServerThread(s_src, s_dst)
    st.start()

    import csv as csvmod
    csv_path = tmp / "b.csv"
    with open(csv_path, "w", newline="") as f:
        w = csvmod.writer(f)
        w.writerow(["oldhost", "oldport", "oldsecurity", "olduser", "oldpassword",
                    "newhost", "newport", "newsecurity", "newuser", "newpassword"])
        for user in src_accounts:
            w.writerow(["127.0.0.1", s_src.port, "none", user, "p",
                        "127.0.0.1", s_dst.port, "none", user, "p"])

    total_msgs = args.messages * args.mailboxes
    cfg = RunConfig(csv_file=str(csv_path), workers=args.workers, logs_dir=str(tmp / "logs"),
                    db_path=str(tmp / "b.db"), timeout=60.0,
                    run_id=time.strftime("%Y%m%d-%H%M%S"))
    from mailferry.config import parse_csv
    specs = parse_csv(str(csv_path))
    stats = Stats()
    session = Session(Path(cfg.logs_dir) / "session.log")
    Path(cfg.logs_dir).mkdir(parents=True, exist_ok=True)

    import asyncio

    from mailferry.engine.scheduler import run_migration

    loop = asyncio.new_event_loop()
    asyncio.set_event_loop(loop)
    t0 = time.time()
    stop = asyncio.Event()
    loop.run_until_complete(
        run_migration(cfg, specs, stats, session, logger_factory(Path(cfg.logs_dir)), stop))
    dur = time.time() - t0
    loop.close()
    st.stop()

    migrated = sum(len(f.msgs) for a in dst_accounts.values() for f in a.folders.values())
    peak_rss = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
    peak_mb = peak_rss / (1024 if sys.platform.startswith("linux") else 1024 * 1024)
    mb = total_bytes / (1024 * 1024)
    print(f"{mailferry.PRODUCT} v{mailferry.__version__} loopback benchmark")
    print(f"  mailboxes           : {args.mailboxes}")
    print(f"  workers             : {args.workers}")
    print(f"  messages            : {total_msgs:,} ({args.size_kb} KB each)")
    print(f"  migrated (verified) : {migrated:,}")
    print(f"  data                : {mb:.1f} MB")
    print(f"  duration            : {dur:.2f} s")
    print(f"  throughput          : {total_msgs / dur:,.0f} msgs/s, {mb / dur:.1f} MB/s")
    print(f"  peak RSS            : {peak_mb:.0f} MB")
    ok = migrated == total_msgs
    print(f"  integrity           : {'OK (no loss, no dupes)' if ok else 'MISMATCH'}")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
