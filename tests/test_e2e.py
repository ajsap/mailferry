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

End-to-end tests: fresh migration, idempotent re-run, adoption of a
pre-synced destination (no duplicates), incremental top-up, flag sync.
"""
from __future__ import annotations

import csv as csvmod
import os
import sys
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[0].parent
sys.path.insert(0, str(ROOT))

from mailferry import cli                                    # noqa: E402
from mailferry.imap import mutf7                             # noqa: E402
from mailferry.util import intervals_diff, to_intervals      # noqa: E402
from tests.fake_imap import Account, FakeIMAPServer, Folder, ServerThread  # noqa: E402

PASS = 0
FAIL = 0


def check(name, cond, detail=""):
    global PASS, FAIL
    if cond:
        PASS += 1
        print(f"  PASS {name}")
    else:
        FAIL += 1
        print(f"  FAIL {name} {detail}")


def msg_body(i, subj, with_mid=True, pad=0):
    mid = f"Message-ID: <m{i}@example.test>\r\n" if with_mid else ""
    body = (f"From: alice@example.test\r\nTo: bob@example.test\r\n"
            f"Subject: {subj} {i}\r\nDate: Thu, 16 Jul 2026 10:{i % 60:02d}:00 +0000\r\n"
            f"{mid}\r\n"
            f"Body of message {i}.\r\n" + ("X" * pad + "\r\n" if pad else ""))
    return body.encode("utf-8")


def build_src() -> Account:
    a = Account("alice", "pw1")
    inbox = a.folders["INBOX"]
    for i in range(1, 41):
        flags = set()
        if i % 3 == 0:
            flags.add("\\Seen")
        if i % 7 == 0:
            flags.add("\\Flagged")
        if i % 11 == 0:
            flags.add("$Label1")
        inbox.add(msg_body(i, "inbox", with_mid=(i not in (5, 6)), pad=i * 37), flags,
                  internaldate=f"{(i % 27) + 1:02d}-Jun-2026 09:00:00 +0000")
    sent = Folder("Sent", attrs=["\\Sent"], uidvalidity=1201)
    for i in range(100, 110):
        sent.add(msg_body(i, "sent"), {"\\Seen"})
    a.folders["Sent"] = sent
    arch = Folder("Archive/2025", uidvalidity=1301)
    for i in range(200, 215):
        arch.add(msg_body(i, "archive", pad=500))
    a.folders["Archive/2025"] = arch
    uni = Folder(mutf7.encode("Ünicode"), uidvalidity=1401)
    for i in range(300, 305):
        uni.add(msg_body(i, "unicode"))
    a.folders[mutf7.encode("Ünicode")] = uni
    return a


def mids(folder: Folder):
    out = []
    for m in folder.msgs:
        for line in m.headers().split(b"\r\n"):
            if line.lower().startswith(b"message-id:"):
                out.append(line.split(b":", 1)[1].strip())
    return out


def run_cli(args):
    return cli.main(args)


def read_results(logs):
    with open(Path(logs) / "results.csv", newline="") as f:
        return list(csvmod.DictReader(f))


def main():
    tmp = Path(tempfile.mkdtemp(prefix="mailferry-test-"))
    print(f"workdir: {tmp}")
    src_acct = build_src()
    dst_acct = Account("bob", "pw2")
    s_src = FakeIMAPServer({"alice": src_acct})
    s_dst = FakeIMAPServer({"bob": dst_acct})
    st = ServerThread(s_src, s_dst)
    st.start()

    csv_path = tmp / "mailboxes.csv"
    with open(csv_path, "w", newline="") as f:
        w = csvmod.writer(f)
        w.writerow(["oldhost", "oldport", "oldsecurity", "olduser", "oldpassword",
                    "newhost", "newport", "newsecurity", "newuser", "newpassword"])
        w.writerow(["127.0.0.1", s_src.port, "none", "alice", "pw1",
                    "127.0.0.1", s_dst.port, "none", "bob", "pw2"])
    db1 = str(tmp / "migration.db")
    logs = str(tmp / "logs")
    base = ["run", str(csv_path), "--db", db1, "--logs-dir", logs,
            "--workers", "2", "--timeout", "45", "--retry-delay", "2"]

    total_src = sum(len(f.msgs) for f in src_acct.folders.values())

    print("\n== T1: fresh migration ==")
    rc = run_cli(base)
    check("exit code 0", rc == 0, f"rc={rc}")
    check("INBOX count", len(dst_acct.folders["INBOX"].msgs) == 40,
          f"got {len(dst_acct.folders['INBOX'].msgs)}")
    check("Sent exists+count", "Sent" in dst_acct.folders and len(dst_acct.folders["Sent"].msgs) == 10)
    check("hierarchy folder", "Archive/2025" in dst_acct.folders
          and len(dst_acct.folders["Archive/2025"].msgs) == 15)
    uni = mutf7.encode("Ünicode")
    check("unicode folder (mUTF-7)", uni in dst_acct.folders
          and len(dst_acct.folders[uni].msgs) == 5, f"folders={list(dst_acct.folders)}")
    total_dst = sum(len(f.msgs) for f in dst_acct.folders.values())
    check("total msgs", total_dst == total_src, f"{total_dst} != {total_src}")
    # flags + internaldate preserved
    s3 = next(m for m in src_acct.folders["INBOX"].msgs if m.uid == 3)
    d3 = next((m for m in dst_acct.folders["INBOX"].msgs
               if m.body == s3.body), None)
    check("body byte-identical", d3 is not None)
    check("flags preserved", d3 is not None and "\\Seen" in d3.flags, f"{d3 and d3.flags}")
    s11 = next(m for m in src_acct.folders["INBOX"].msgs if m.uid == 11)
    d11 = next((m for m in dst_acct.folders["INBOX"].msgs if m.body == s11.body), None)
    check("keyword preserved", d11 is not None and "$Label1" in d11.flags, f"{d11 and d11.flags}")
    check("internaldate preserved", d3 is not None and d3.internaldate == s3.internaldate,
          f"{d3 and d3.internaldate} vs {s3.internaldate}")

    print("\n== T2: idempotent re-run (same DB) ==")
    before = s_dst.append_count
    rc = run_cli(base)
    check("exit code 0", rc == 0, f"rc={rc}")
    check("no new appends", s_dst.append_count == before,
          f"{s_dst.append_count - before} extra appends")
    check("counts unchanged", sum(len(f.msgs) for f in dst_acct.folders.values()) == total_src)

    print("\n== T3: adoption — lost DB, pre-synced destination, no duplicates ==")
    db2 = str(tmp / "migration2.db")
    before = s_dst.append_count
    rc = run_cli(["run", str(csv_path), "--db", db2, "--logs-dir", logs,
                  "--workers", "2", "--timeout", "45"])
    check("exit code 0", rc == 0, f"rc={rc}")
    check("no duplicate appends", s_dst.append_count == before,
          f"{s_dst.append_count - before} appends happened")
    check("counts still exact", sum(len(f.msgs) for f in dst_acct.folders.values()) == total_src)
    rows = read_results(logs)
    check("results adopted == total", rows and int(rows[0]["msgs_adopted"]) == total_src,
          f"{rows and rows[0]['msgs_adopted']}")

    print("\n== T4: incremental top-up (backup mode) ==")
    for i in range(500, 503):
        src_acct.folders["INBOX"].add(msg_body(i, "new"), {"\\Seen"} if i % 2 else set())
    src_acct.folders[uni].add(msg_body(600, "uninew"))
    before = s_dst.append_count
    rc = run_cli(base)
    check("exit code 0", rc == 0, f"rc={rc}")
    check("exactly 4 new appends", s_dst.append_count == before + 4,
          f"{s_dst.append_count - before}")
    check("INBOX 43", len(dst_acct.folders["INBOX"].msgs) == 43)
    check("unicode 6", len(dst_acct.folders[uni].msgs) == 6)

    print("\n== T5: flag re-sync ==")
    s1 = next(m for m in src_acct.folders["INBOX"].msgs if m.uid == 1)
    s1.flags = {"\\Seen", "\\Answered"}
    rc = run_cli(base + ["--sync-flags"])
    check("exit code 0", rc == 0, f"rc={rc}")
    d1 = next((m for m in dst_acct.folders["INBOX"].msgs if m.body == s1.body), None)
    check("flags updated on dest", d1 is not None and d1.flags >= {"\\Seen", "\\Answered"},
          f"{d1 and d1.flags}")

    print("\n== T6: no-Message-ID fallback did not duplicate ==")
    check("dst INBOX has exactly one copy of mid-less uid5 body",
          sum(1 for m in dst_acct.folders["INBOX"].msgs
              if m.body == next(x for x in src_acct.folders["INBOX"].msgs if x.uid == 5).body) == 1)

    print("\n== T7: crash-window reconciliation (inflight rows) ==")
    # simulate a crash after APPEND but before commit: demote 3 done rows
    import sqlite3
    con = sqlite3.connect(db1)
    con.execute("UPDATE messages SET state='inflight', dst_uid=NULL "
                "WHERE src_uid IN (2,3,4) AND folder_id="
                "(SELECT id FROM folders WHERE src_name='INBOX')")
    # pretend the crash predates the last bracket so the dest tail scan covers them
    con.execute("UPDATE folders SET last_uidnext_dst=1 WHERE src_name='INBOX'")
    con.commit()
    con.close()
    before = s_dst.append_count
    rc = run_cli(base)
    check("exit code 0", rc == 0, f"rc={rc}")
    check("reconciled without re-copy", s_dst.append_count == before,
          f"{s_dst.append_count - before} appends")
    check("INBOX still 43", len(dst_acct.folders["INBOX"].msgs) == 43)
    con = sqlite3.connect(db1)
    st_rows = con.execute("SELECT state, COUNT(*) FROM messages WHERE src_uid IN (2,3,4) "
                          "AND folder_id=(SELECT id FROM folders WHERE src_name='INBOX') "
                          "GROUP BY state").fetchall()
    con.close()
    check("inflight rows now done", st_rows == [("done", 3)], f"{st_rows}")

    print("\n== T8: wrapper state import + --skip-completed ==")
    import json as jsonmod
    db3 = str(tmp / "migration3.db")
    state_file = tmp / "migration.state"
    key = "\x1f".join(["127.0.0.1", "alice", "127.0.0.1", "bob"])
    with open(state_file, "w") as f:
        f.write(jsonmod.dumps({"type": "meta", "version": 1}) + "\n")
        f.write(jsonmod.dumps({"type": "result", "key": key, "status": "SUCCESS",
                               "oldhost": "127.0.0.1", "olduser": "alice",
                               "newhost": "127.0.0.1", "newuser": "bob"}) + "\n")
    rc = run_cli(["import-state", str(state_file), "--db", db3])
    check("import rc 0", rc == 0, f"rc={rc}")
    before = s_dst.append_count
    rc = run_cli(["run", str(csv_path), "--db", db3, "--logs-dir", logs,
                  "--skip-completed", "--timeout", "45"])
    check("skip-completed rc 0", rc == 0, f"rc={rc}")
    check("skipped: zero appends", s_dst.append_count == before)
    rows = read_results(logs)
    check("status SKIPPED", rows and rows[0]["status"] == "SKIPPED", f"{rows and rows[0]['status']}")

    print("\n== T9: stale detection — automatic recovery (server hangs once) ==")
    t9_src = Account("u9", "p")
    for i in range(40):
        t9_src.folders["INBOX"].add(
            (f"Message-ID: <t9m{i}@x>\r\nSubject: t9 {i}\r\n\r\n" + "S" * 1500).encode())
    t9_dst = Account("u9", "p")
    s9a, s9b = FakeIMAPServer({"u9": t9_src}), FakeIMAPServer({"u9": t9_dst})
    s9a.stall_after = 10                    # hang mid-folder ...
    s9a.stall_mode = "once"                 # ... but recover on the next connection
    st9 = ServerThread(s9a, s9b)
    st9.start()
    csv9 = tmp / "t9.csv"
    with open(csv9, "w", newline="") as f:
        w = csvmod.writer(f)
        w.writerow(["oldhost", "oldport", "oldsecurity", "olduser", "oldpassword",
                    "newhost", "newport", "newsecurity", "newuser", "newpassword"])
        w.writerow(["127.0.0.1", s9a.port, "none", "u9", "p",
                    "127.0.0.1", s9b.port, "none", "u9", "p"])
    base9 = ["run", str(csv9), "--db", str(tmp / "t9.db"), "--logs-dir", logs,
             "--workers", "1", "--timeout", "60", "--retry-delay", "1",
             "--stale-timeout", "2", "--recovery-interval", "1", "--recovery-retries", "3"]
    t0 = time.time()
    rc = run_cli(base9)
    dt9 = time.time() - t0
    check("recovered run rc 0", rc == 0, f"rc={rc}")
    check("recovery was prompt (<25s)", dt9 < 25, f"{dt9:.1f}s")
    check("all messages delivered", len(t9_dst.folders["INBOX"].msgs) == 40,
          f"{len(t9_dst.folders['INBOX'].msgs)}")
    check("no duplicates after recovery",
          len({m.body for m in t9_dst.folders["INBOX"].msgs}) == 40)
    slog = (Path(logs) / "session.log").read_text()
    check("stall detected logged", "stalled transfer detected" in slog)
    check("recovery success logged", "transfer recovered" in slog)

    print("\n== T10: recovery exhausted -> STALE, then clean resume ==")
    t10_src = Account("u10", "p")
    for i in range(30):
        t10_src.folders["INBOX"].add(
            (f"Message-ID: <t10m{i}@x>\r\nSubject: t10 {i}\r\n\r\n" + "S" * 1200).encode())
    t10_dst = Account("u10", "p")
    sa, sb = FakeIMAPServer({"u10": t10_src}), FakeIMAPServer({"u10": t10_dst})
    sa.stall_after = 8                      # stalls EVERY connection
    st10 = ServerThread(sa, sb)
    st10.start()
    csv10 = tmp / "t10.csv"
    with open(csv10, "w", newline="") as f:
        w = csvmod.writer(f)
        w.writerow(["oldhost", "oldport", "oldsecurity", "olduser", "oldpassword",
                    "newhost", "newport", "newsecurity", "newuser", "newpassword"])
        w.writerow(["127.0.0.1", sa.port, "none", "u10", "p",
                    "127.0.0.1", sb.port, "none", "u10", "p"])
    base10 = ["run", str(csv10), "--db", str(tmp / "t10.db"), "--logs-dir", logs,
              "--workers", "1", "--timeout", "60", "--retry-delay", "1",
              "--stale-timeout", "2", "--recovery-interval", "1", "--recovery-retries", "2"]
    rc = run_cli(base10)
    rows = read_results(logs)
    check("exhausted run rc 1", rc == 1, f"rc={rc}")
    check("status STALE", rows and rows[0]["status"] == "STALE",
          f"{rows and rows[0]['status']}")
    check("STALE notes explain", rows and "recovery failed" in rows[0]["notes"])
    slog = (Path(logs) / "session.log").read_text()
    check("RECOVERY EXHAUSTED logged", "RECOVERY EXHAUSTED" in slog)
    delivered = len(t10_dst.folders["INBOX"].msgs)
    check("partial progress kept", 0 < delivered < 30, f"{delivered}")
    sa.stall_after = None                   # server comes back
    before10 = sb.append_count
    rc = run_cli(base10)
    check("resume rc 0", rc == 0, f"rc={rc}")
    check("resume completed all", len(t10_dst.folders["INBOX"].msgs) == 30,
          f"{len(t10_dst.folders['INBOX'].msgs)}")
    check("resume appended only the gap", sb.append_count - before10 == 30 - delivered,
          f"{sb.append_count - before10} vs {30 - delivered}")
    check("no duplicates after STALE resume",
          len({m.body for m in t10_dst.folders["INBOX"].msgs}) == 30)
    st10.stop()

    print("\n== T11: cluster leases — dead workers are reclaimed, never refused ==")
    import sqlite3 as sq
    dead = "deadhost:999:aabbccdd"

    def plant_lock(age_secs, worker_hb=None):
        con = sq.connect(tmp / "t9.db")
        con.execute("UPDATE mailboxes SET lease_owner=?, lease_ts=?",
                    (dead, time.time() - age_secs))
        con.execute("DELETE FROM workers")
        if worker_hb is not None:
            con.execute("INSERT INTO workers(id,host,pid,run_id,started,heartbeat) "
                        "VALUES(?,?,?,?,?,?)", (dead, "deadhost", 999, "x",
                                                time.time() - 500,
                                                time.time() - worker_hb))
        con.commit()
        con.close()

    def slog():
        return (Path(logs) / "session.log").read_text()

    plant_lock(170)                          # legacy lease (no worker row), silent 170s
    rc = run_cli(base9)
    check("legacy dead lease reclaimed rc 0", rc == 0, f"rc={rc}")
    check("reclaim logged", "reclaimed from offline worker" in slog())
    plant_lock(10, worker_hb=120)            # cluster worker silent 120s > 60s timeout
    rc = run_cli(base9)
    check("offline worker reclaimed rc 0", rc == 0, f"rc={rc}")
    plant_lock(400)                          # ancient lease: auto-reset path
    rc = run_cli(base9)
    check("expired lock auto-reset rc 0", rc == 0, f"rc={rc}")
    check("auto-reset logged", "stale lock auto-reset" in slog())
    check("worker registration logged", "cluster: joined as worker" in slog())
    st9.stop()

    print("\n== T12: two live instances — split, watch, kill -9 failover ==")
    import subprocess
    t12_src = Account("u12", "p")
    for i in range(36):
        t12_src.folders["INBOX"].add(
            (f"Message-ID: <t12m{i}@x>\r\nSubject: t12 {i}\r\n\r\n" + "T" * 1200).encode())
    t12_dst = Account("u12", "p")
    sc, sd = FakeIMAPServer({"u12": t12_src}), FakeIMAPServer({"u12": t12_dst})
    sc.stall_after = 6                       # instance A hangs mid-folder ...
    sc.stall_mode = "once"                   # ... and the line is clear for B
    st12 = ServerThread(sc, sd)
    st12.start()
    csv12 = tmp / "t12.csv"
    with open(csv12, "w", newline="") as f:
        w = csvmod.writer(f)
        w.writerow(["oldhost", "oldport", "oldsecurity", "olduser", "oldpassword",
                    "newhost", "newport", "newsecurity", "newuser", "newpassword"])
        w.writerow(["127.0.0.1", sc.port, "none", "u12", "p",
                    "127.0.0.1", sd.port, "none", "u12", "p"])
    db12 = str(tmp / "t12.db")
    logsA, logsB = str(tmp / "logsA"), str(tmp / "logsB")
    argv = [sys.executable, "-m", "mailferry", "run", str(csv12), "--db", db12,
            "--workers", "1", "--timeout", "60", "--retry-delay", "1",
            "--stale-timeout", "0", "--worker-timeout", "4", "--no-tui"]
    env = dict(os.environ)
    procA = subprocess.Popen(argv + ["--logs-dir", logsA], cwd=str(ROOT), env=env,
                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    deadline = time.time() + 30              # wait until A owns the mailbox & stalls
    while time.time() < deadline and sc.fetch_body_count <= 6:
        time.sleep(0.3)
    check("A claimed and hit the stall", sc.fetch_body_count > 6,
          f"fetches={sc.fetch_body_count}")
    procB = subprocess.Popen(argv + ["--logs-dir", logsB], cwd=str(ROOT), env=env,
                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    time.sleep(4.0)                          # B joins, sees the mailbox as REMOTE
    procA.kill()                             # power-loss simulation (kill -9)
    try:
        outB, _ = procB.communicate(timeout=90)
    except subprocess.TimeoutExpired:
        procB.kill()
        outB = b"(timeout)"
        check("B finished after failover", False, "B timed out")
    else:
        check("B finished after failover rc 0", procB.returncode == 0,
              f"rc={procB.returncode}")
    procA.wait(timeout=10)
    slogB = (Path(logsB) / "session.log").read_text()
    check("B watched the remote worker", "REMOTE — being processed by worker" in slogB)
    check("B reclaimed after kill -9", "went offline — reclaimed" in slogB
          or "reclaimed from offline worker" in slogB, slogB[-400:])
    check("B joined the cluster", "cluster: joined as worker" in slogB)
    got12 = len(t12_dst.folders["INBOX"].msgs)
    check("all messages delivered across failover", got12 == 36, f"{got12}")
    check("no duplicates across failover",
          len({m.body for m in t12_dst.folders["INBOX"].msgs}) == 36)
    st12.stop()

    print("\n== T13: poison messages — isolation, registry, WARNINGS, retry ==")
    t13_src = Account("u13", "p")
    f13 = t13_src.folders["INBOX"]
    for i in range(20):
        marker = b"KILLME" if i == 7 else (b"REJECTME" if i == 12 else b"OK")
        f13.add((f"Message-ID: <t13m{i}@x>\r\nSubject: poison {i}\r\n"
                 f"From: Boss <boss@x>\r\nDate: Fri, 17 Jul 2026 10:00:00 +0000\r\n"
                 "\r\n").encode() + marker + b" " + b"B" * 900)
    t13_dst = Account("u13", "p")
    se, sf = FakeIMAPServer({"u13": t13_src}), FakeIMAPServer({"u13": t13_dst})
    sf.append_kill = b"KILLME"       # content filter drops the connection
    sf.append_reject = b"REJECTME"   # server answers a clean NO
    st13 = ServerThread(se, sf)
    st13.start()
    csv13 = tmp / "t13.csv"
    with open(csv13, "w", newline="") as f:
        w = csvmod.writer(f)
        w.writerow(["oldhost", "oldport", "oldsecurity", "olduser", "oldpassword",
                    "newhost", "newport", "newsecurity", "newuser", "newpassword"])
        w.writerow(["127.0.0.1", se.port, "none", "u13", "p",
                    "127.0.0.1", sf.port, "none", "u13", "p"])
    base13 = ["run", str(csv13), "--db", str(tmp / "t13.db"), "--logs-dir", logs,
              "--workers", "1", "--timeout", "45", "--retry-delay", "1",
              "--stale-timeout", "0"]
    t0 = time.time()
    rc = run_cli(base13)
    dt13 = time.time() - t0
    rows = read_results(logs)
    check("poison run rc 0 (warnings complete)", rc == 0, f"rc={rc}")
    check("status WARNINGS", rows[0]["status"] == "WARNINGS", rows[0]["status"])
    check("2 failed recorded", rows[0]["msgs_failed"] == "2", rows[0]["msgs_failed"])
    check("18 healthy delivered", len(t13_dst.folders["INBOX"].msgs) == 18,
          f"{len(t13_dst.folders['INBOX'].msgs)}")
    check("isolation was fast (<60s)", dt13 < 60, f"{dt13:.1f}s")
    check("no duplicates around poison",
          len({m.body for m in t13_dst.folders["INBOX"].msgs}) == 18)
    mlog13 = next(Path(logs).glob("0*u13.log")).read_text()
    check("Recovery Mode logged", "Recovery Mode" in mlog13)
    check("registry block logged", "Failed Message Registry" in mlog13
          and "Subject: poison" in mlog13)
    check("failed_messages.csv written",
          (Path(logs) / "failed_messages.csv").exists())
    import sqlite3 as sq13
    con = sq13.connect(tmp / "t13.db")
    freg = con.execute("SELECT src_uid, ftype, status, subject, fail_count "
                       "FROM failed_messages ORDER BY src_uid").fetchall()
    con.close()
    check("registry has both UIDs", [r[0] for r in freg] == [8, 13], f"{freg}")
    types = {r[0]: r[1] for r in freg}
    check("kill classified CONNECTION_RESET", types.get(8) == "CONNECTION_RESET", f"{types}")
    check("reject classified APPEND_NO", types.get(13) == "APPEND_NO", f"{types}")
    check("metadata captured", all(r[3].startswith("poison") for r in freg), f"{freg}")
    before13 = sf.append_count
    rc = run_cli(base13)
    check("resume skips known-failed instantly", rc == 0 and sf.append_count == before13,
          f"rc={rc} appends+{sf.append_count - before13}")
    mlog13 = next(Path(logs).glob("0*u13.log")).read_text()
    check("skip logged", "previously failed message(s)" in mlog13)
    check("failed command lists them", run_cli(["failed", "--db", str(tmp / "t13.db")]) == 0)
    sf.append_kill = b""
    sf.append_reject = b""            # server fixed (filter removed)
    rc = run_cli(["retry-failed", "--db", str(tmp / "t13.db")])
    check("retry-failed rc 0", rc == 0, f"rc={rc}")
    rc = run_cli(base13)
    rows = read_results(logs)
    check("retry run completes SUCCESS", rc == 0 and rows[0]["status"] == "SUCCESS",
          f"rc={rc} {rows[0]['status']}")
    check("all 20 delivered after recovery", len(t13_dst.folders["INBOX"].msgs) == 20)
    con = sq13.connect(tmp / "t13.db")
    recovered = con.execute("SELECT COUNT(*) FROM failed_messages "
                            "WHERE status='RECOVERED'").fetchone()[0]
    con.close()
    check("registry rows RECOVERED", recovered == 2, f"{recovered}")
    st13.stop()

    print("\n== T14: mailferry.toml — generate, load, validate, override ==")
    cfgdir = tmp / "cfg"
    os.environ["MAILFERRY_CONFIG_DIR"] = str(cfgdir)
    try:
        from mailferry.configfile import load_config
        vals, warns, cpath, created = load_config()
        check("config auto-generated", created and cpath.exists(), f"{cpath}")
        check("defaults produce no warnings", not warns, f"{warns}")
        text = cpath.read_text()
        check("config documented", text.count("#") > 20 and "[migration]" in text
              and "Andy Saputra" in text)
        cpath.write_text(text.replace("parallel_mailboxes = 10", "parallel_mailboxes = 4")
                         .replace("worker_timeout_seconds = 60",
                                  "worker_timeout_seconds = 5")   # invalid (< 20)
                         + "\n[future]\nshiny = true\n")
        vals, warns, _, _ = load_config()
        check("valid override applied", vals.get("workers") == 4, f"{vals.get('workers')}")
        check("invalid value warned + dropped",
              "worker_timeout_seconds" in " ".join(warns) and "worker_timeout" not in vals,
              f"{warns}")
        check("unknown section warned, not fatal",
              any("future.shiny" in w for w in warns), f"{warns}")
        cpath.write_text("this is { not toml [at all")
        vals, warns, _, _ = load_config()
        check("broken config never fatal", vals == {} and warns, f"{warns}")
    finally:
        os.environ.pop("MAILFERRY_CONFIG_DIR", None)

    print("\n== unit: interval math & mutf7 ==")
    check("mutf7 roundtrip", mutf7.decode(mutf7.encode("Boîte/Épinglés & más")) == "Boîte/Épinglés & más")
    check("intervals_diff", intervals_diff(to_intervals([1, 2, 3, 5, 7, 8]),
                                           to_intervals([2, 7])) == [(1, 1), (3, 3), (5, 5), (8, 8)])

    st.stop()
    print(f"\n{'=' * 50}\nRESULT: {PASS} passed, {FAIL} failed")
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
