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
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[0].parent))

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

    print("\n== unit: interval math & mutf7 ==")
    check("mutf7 roundtrip", mutf7.decode(mutf7.encode("Boîte/Épinglés & más")) == "Boîte/Épinglés & más")
    check("intervals_diff", intervals_diff(to_intervals([1, 2, 3, 5, 7, 8]),
                                           to_intervals([2, 7])) == [(1, 1), (3, 3), (5, 5), (8, 8)])

    st.stop()
    print(f"\n{'=' * 50}\nRESULT: {PASS} passed, {FAIL} failed")
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
