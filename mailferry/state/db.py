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

SQLite state layer: WAL, one dedicated writer thread (the asyncio loop
never blocks on disk). Per-UID message state is the resume/idempotency core.
"""
from __future__ import annotations

import asyncio
import json
import os
import socket
import sqlite3
import time
from concurrent.futures import ThreadPoolExecutor
from typing import Dict, List, Optional, Tuple

from ..config import MailboxSpec, mailbox_key
from ..util import to_intervals

SCHEMA = """
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA busy_timeout=10000;
CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT);
CREATE TABLE IF NOT EXISTS runs(
  id TEXT PRIMARY KEY, started REAL, finished REAL, argv TEXT, result TEXT);
CREATE TABLE IF NOT EXISTS mailboxes(
  id INTEGER PRIMARY KEY, key TEXT UNIQUE,
  src_host TEXT, src_user TEXT, dst_host TEXT, dst_user TEXT,
  status TEXT DEFAULT 'NEW', attempts INTEGER DEFAULT 0, last_error TEXT DEFAULT '',
  lease_ts REAL DEFAULT 0, lease_owner TEXT DEFAULT '',
  msgs_total INTEGER DEFAULT 0, msgs_done INTEGER DEFAULT 0,
  bytes_total INTEGER DEFAULT 0, bytes_done INTEGER DEFAULT 0,
  updated REAL DEFAULT 0);
CREATE TABLE IF NOT EXISTS folders(
  id INTEGER PRIMARY KEY, mailbox_id INTEGER NOT NULL,
  src_name TEXT NOT NULL, dst_name TEXT DEFAULT '',
  uv_src INTEGER DEFAULT 0, uv_dst INTEGER DEFAULT 0,
  last_uidnext_src INTEGER DEFAULT 0, last_uidnext_dst INTEGER DEFAULT 0,
  highestmodseq INTEGER DEFAULT 0, adopt_done INTEGER DEFAULT 0,
  msgs_total INTEGER DEFAULT 0, bytes_total INTEGER DEFAULT 0,
  msgs_done INTEGER DEFAULT 0, bytes_done INTEGER DEFAULT 0,
  status TEXT DEFAULT 'PENDING', last_error TEXT DEFAULT '',
  UNIQUE(mailbox_id, src_name));
CREATE TABLE IF NOT EXISTS messages(
  folder_id INTEGER NOT NULL, src_uid INTEGER NOT NULL,
  dst_uid INTEGER, size INTEGER DEFAULT 0, flags TEXT DEFAULT '',
  internaldate TEXT DEFAULT '', fp TEXT, state TEXT DEFAULT 'planned',
  origin TEXT DEFAULT '', error TEXT DEFAULT '',
  PRIMARY KEY(folder_id, src_uid)) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS ix_msg_state ON messages(folder_id, state);
CREATE TABLE IF NOT EXISTS events(
  ts REAL, mailbox_id INTEGER, folder_id INTEGER, kind TEXT, detail TEXT);
CREATE TABLE IF NOT EXISTS workers(
  id TEXT PRIMARY KEY, host TEXT, pid INTEGER, run_id TEXT,
  started REAL DEFAULT 0, heartbeat REAL DEFAULT 0,
  status TEXT DEFAULT 'active');
CREATE TABLE IF NOT EXISTS failed_messages(
  id INTEGER PRIMARY KEY,
  mailbox_id INTEGER NOT NULL, folder TEXT NOT NULL, src_uid INTEGER NOT NULL,
  message_id TEXT DEFAULT '', subject TEXT DEFAULT '',
  sender TEXT DEFAULT '', date TEXT DEFAULT '', size INTEGER DEFAULT 0,
  ftype TEXT DEFAULT 'UNKNOWN', reason TEXT DEFAULT '',
  first_ts REAL DEFAULT 0, last_ts REAL DEFAULT 0,
  fail_count INTEGER DEFAULT 0, recovered_ts REAL DEFAULT 0,
  status TEXT DEFAULT 'FAILED',
  UNIQUE(mailbox_id, folder, src_uid));
"""


def lease_owner_id(run_id: str) -> str:
    """Unique Worker ID: hostname + PID + UUID (pid reuse across reboots is
    covered by the random component)."""
    import uuid
    return f"{socket.gethostname()}:{os.getpid()}:{uuid.uuid4().hex[:8]}"


def short_worker(owner: str) -> str:
    """Compact display form of a Worker ID: host:pid."""
    bits = (owner or "").split(":")
    host = bits[0].split(".")[0] if bits and bits[0] else "?"
    pid = bits[1] if len(bits) > 1 else "?"
    return f"{host}:{pid}"


class StateDB:
    LEASE_FRESH = 300.0     # a lease younger than this means "someone is live"
    HEARTBEAT = 15.0        # a live worker refreshes leases/heartbeat this often

    def __init__(self, path: str, ephemeral: bool = False,
                 lease_fresh: float = LEASE_FRESH):
        self.path = ":memory:" if ephemeral else path
        self.ephemeral = ephemeral
        self.lease_fresh = max(30.0, float(lease_fresh or self.LEASE_FRESH))
        self._ex = ThreadPoolExecutor(max_workers=1, thread_name_prefix="mf-db")
        self._con: Optional[sqlite3.Connection] = None
        f = self._ex.submit(self._open)
        f.result()

    def _open(self):
        self._con = sqlite3.connect(self.path)
        self._con.row_factory = sqlite3.Row
        self._con.executescript(SCHEMA)
        self._con.execute("INSERT OR IGNORE INTO meta(k,v) VALUES('schema_version','1')")
        self._con.commit()

    async def run(self, fn):
        loop = asyncio.get_event_loop()
        return await loop.run_in_executor(self._ex, fn)

    def run_sync(self, fn):
        return self._ex.submit(fn).result()

    def close(self):
        def f():
            try:
                self._con.commit()
                self._con.close()
            except Exception:
                pass
        try:
            self._ex.submit(f).result(timeout=10)
        except Exception:
            pass
        self._ex.shutdown(wait=False)

    # ------------------------------------------------------------- runs --

    async def start_run(self, run_id: str, argv: str):
        def f():
            self._con.execute("INSERT OR REPLACE INTO runs(id,started,argv) VALUES(?,?,?)",
                              (run_id, time.time(), argv))
            self._con.commit()
        await self.run(f)

    async def end_run(self, run_id: str, result: str):
        def f():
            self._con.execute("UPDATE runs SET finished=?, result=? WHERE id=?",
                              (time.time(), result, run_id))
            self._con.commit()
        await self.run(f)

    # -------------------------------------------------------- mailboxes --

    async def upsert_mailbox(self, spec: MailboxSpec) -> Dict:
        def f():
            self._con.execute(
                "INSERT OR IGNORE INTO mailboxes(key,src_host,src_user,dst_host,dst_user) "
                "VALUES(?,?,?,?,?)",
                (spec.key, spec.src.host, spec.src.user, spec.dst.host, spec.dst.user))
            self._con.commit()
            row = self._con.execute("SELECT * FROM mailboxes WHERE key=?", (spec.key,)).fetchone()
            return dict(row)
        return await self.run(f)

    async def set_mailbox(self, mid: int, **fields):
        keys = ", ".join(f"{k}=?" for k in fields)
        vals = list(fields.values()) + [time.time(), mid]

        def f():
            self._con.execute(f"UPDATE mailboxes SET {keys}, updated=? WHERE id=?", vals)
            self._con.commit()
        await self.run(f)

    async def try_lease(self, mid: int, owner: str, steal_stale=True) -> Tuple[bool, str, float]:
        def f():
            row = self._con.execute(
                "SELECT lease_ts, lease_owner FROM mailboxes WHERE id=?", (mid,)).fetchone()
            age = time.time() - (row["lease_ts"] or 0)
            other = row["lease_owner"] or ""
            if other and other != owner and age < self.lease_fresh:
                return (False, other, age)
            self._con.execute("UPDATE mailboxes SET lease_ts=?, lease_owner=? WHERE id=?",
                              (time.time(), owner, mid))
            self._con.commit()
            return (True, other, age)
        return await self.run(f)

    async def read_lease(self, mid: int) -> Tuple[str, float]:
        """Current lease (owner, heartbeat timestamp) — 0.0 when unleased."""
        def f():
            row = self._con.execute(
                "SELECT lease_ts, lease_owner FROM mailboxes WHERE id=?", (mid,)).fetchone()
            return (row["lease_owner"] or "", float(row["lease_ts"] or 0.0))
        return await self.run(f)

    async def force_lease(self, mid: int, owner: str, observed_owner: str,
                          observed_ts: float) -> bool:
        """Atomically take over a lock verified as stale. Compare-and-swap:
        succeeds only if the lease still belongs to the observed (dead)
        owner with the observed heartbeat — if the instance came back to
        life and refreshed its lease, the takeover fails. Safe when several
        machines race: exactly one CAS wins."""
        def f():
            cur = self._con.execute(
                "UPDATE mailboxes SET lease_ts=?, lease_owner=? "
                "WHERE id=? AND lease_owner=? AND lease_ts<=?",
                (time.time(), owner, mid, observed_owner, observed_ts + 0.001))
            self._con.commit()
            return cur.rowcount == 1
        return await self.run(f)

    async def refresh_lease(self, mid: int, owner: str) -> bool:
        """Heartbeat our lease. Returns False when the lease is no longer
        ours — another worker took the mailbox over (failover) and this
        worker must stop processing it immediately."""
        def f():
            cur = self._con.execute(
                "UPDATE mailboxes SET lease_ts=? WHERE id=? AND lease_owner=?",
                (time.time(), mid, owner))
            self._con.commit()
            return cur.rowcount == 1
        return await self.run(f)

    async def clear_lease(self, mid: int, owner: str):
        def f():
            self._con.execute(
                "UPDATE mailboxes SET lease_ts=0, lease_owner='' WHERE id=? AND lease_owner=?",
                (mid, owner))
            self._con.commit()
        await self.run(f)

    # ---------------------------------------------------------- workers --

    async def register_worker(self, owner: str, run_id: str):
        """Join the cluster: register this instance as a worker."""
        def f():
            now = time.time()
            self._con.execute("DELETE FROM workers WHERE heartbeat < ?",
                              (now - 86400,))          # prune day-old corpses
            self._con.execute(
                "INSERT OR REPLACE INTO workers(id,host,pid,run_id,started,heartbeat,status) "
                "VALUES(?,?,?,?,?,?, 'active')",
                (owner, socket.gethostname(), os.getpid(), run_id, now, now))
            self._con.commit()
        await self.run(f)

    async def worker_heartbeat(self, owner: str):
        def f():
            self._con.execute("UPDATE workers SET heartbeat=? WHERE id=?",
                              (time.time(), owner))
            self._con.commit()
        await self.run(f)

    async def deregister_worker(self, owner: str):
        """Graceful exit: release every lease we still hold and remove the
        worker row so peers see the jobs freed immediately."""
        def f():
            self._con.execute(
                "UPDATE mailboxes SET lease_ts=0, lease_owner='' WHERE lease_owner=?",
                (owner,))
            self._con.execute("DELETE FROM workers WHERE id=?", (owner,))
            self._con.commit()
        await self.run(f)

    async def list_workers(self, offline_after: float) -> List[Dict]:
        """Cluster roster with liveness + active mailbox counts."""
        def f():
            now = time.time()
            rows = self._con.execute(
                "SELECT w.id, w.host, w.pid, w.run_id, w.started, w.heartbeat, "
                "  (SELECT COUNT(*) FROM mailboxes m "
                "   WHERE m.lease_owner = w.id AND m.lease_ts > 0) AS active "
                "FROM workers w ORDER BY w.started").fetchall()
            out = []
            for r in rows:
                age = now - (r["heartbeat"] or 0)
                status = ("OFFLINE" if age > offline_after
                          else ("WORKING" if r["active"] else "IDLE"))
                out.append({"id": r["id"], "host": r["host"], "pid": r["pid"],
                            "run_id": r["run_id"], "started": r["started"],
                            "heartbeat": r["heartbeat"], "hb_age": age,
                            "active": r["active"], "status": status})
            return out
        return await self.run(f)

    async def worker_hb_age(self, owner: str) -> Optional[float]:
        """Heartbeat age of a specific worker, None when unregistered
        (a legacy instance that predates the workers table)."""
        def f():
            r = self._con.execute("SELECT heartbeat FROM workers WHERE id=?",
                                  (owner,)).fetchone()
            return None if r is None else max(0.0, time.time() - (r["heartbeat"] or 0))
        return await self.run(f)

    # ------------------------------------------- failed message registry --

    async def record_failed(self, mid: int, folder: str, uid: int, *,
                            message_id="", subject="", sender="", date="",
                            size=0, ftype="UNKNOWN", reason=""):
        """Upsert a permanently failed message. Repeat failures bump the
        count and timestamps; a previously RECOVERED/IGNORED row that fails
        again returns to FAILED."""
        def f():
            now = time.time()
            self._con.execute(
                "INSERT INTO failed_messages(mailbox_id,folder,src_uid,message_id,"
                "subject,sender,date,size,ftype,reason,first_ts,last_ts,fail_count,status) "
                "VALUES(?,?,?,?,?,?,?,?,?,?,?,?,1,'FAILED') "
                "ON CONFLICT(mailbox_id,folder,src_uid) DO UPDATE SET "
                "message_id=CASE WHEN excluded.message_id!='' THEN excluded.message_id ELSE message_id END, "
                "subject=CASE WHEN excluded.subject!='' THEN excluded.subject ELSE subject END, "
                "sender=CASE WHEN excluded.sender!='' THEN excluded.sender ELSE sender END, "
                "date=CASE WHEN excluded.date!='' THEN excluded.date ELSE date END, "
                "size=MAX(size, excluded.size), ftype=excluded.ftype, "
                "reason=excluded.reason, last_ts=excluded.last_ts, "
                "fail_count=fail_count+1, status='FAILED'",
                (mid, folder, uid, message_id, subject[:200], sender[:200],
                 date[:80], size, ftype, reason[:300], now, now))
            self._con.commit()
        await self.run(f)

    async def failed_rows(self, mid: Optional[int] = None,
                          statuses: Tuple[str, ...] = ()) -> List[Dict]:
        def f():
            q = ("SELECT f.*, m.src_user AS mailbox FROM failed_messages f "
                 "JOIN mailboxes m ON m.id=f.mailbox_id")
            cond, args = [], []
            if mid is not None:
                cond.append("f.mailbox_id=?")
                args.append(mid)
            if statuses:
                cond.append(f"f.status IN ({','.join('?' * len(statuses))})")
                args.extend(statuses)
            if cond:
                q += " WHERE " + " AND ".join(cond)
            q += " ORDER BY m.src_user, f.folder, f.src_uid"
            return [dict(r) for r in self._con.execute(q, args).fetchall()]
        return await self.run(f)

    async def failed_registry_uids(self, mid: int, folder: str) -> Dict[int, str]:
        """uid -> status for a folder (skip decisions + recovery marking)."""
        def f():
            rows = self._con.execute(
                "SELECT src_uid, status FROM failed_messages "
                "WHERE mailbox_id=? AND folder=?", (mid, folder)).fetchall()
            return {r[0]: r[1] for r in rows}
        return await self.run(f)

    async def outstanding_failed(self, mid: Optional[int] = None) -> int:
        def f():
            q = "SELECT COUNT(*) FROM failed_messages WHERE status IN ('FAILED','RETRY_PENDING','RETRYING')"
            args = []
            if mid is not None:
                q += " AND mailbox_id=?"
                args.append(mid)
            return self._con.execute(q, args).fetchone()[0]
        return await self.run(f)

    async def set_failed_status(self, status: str, mid: Optional[int] = None,
                                folder: str = "", uid: Optional[int] = None) -> int:
        """Bulk status transition (RETRY_PENDING, IGNORED, ...). Also
        re-plans the matching message rows when retrying, so the next run
        picks them up."""
        def f():
            cond, args = ["status != 'RECOVERED'"], []
            if mid is not None:
                cond.append("mailbox_id=?")
                args.append(mid)
            if folder:
                cond.append("folder=?")
                args.append(folder)
            if uid is not None:
                cond.append("src_uid=?")
                args.append(uid)
            where = " AND ".join(cond)
            rows = self._con.execute(
                f"SELECT mailbox_id, folder, src_uid FROM failed_messages WHERE {where}",
                args).fetchall()
            cur = self._con.execute(
                f"UPDATE failed_messages SET status=? WHERE {where}", [status] + args)
            if status == "RETRY_PENDING":
                for r in rows:
                    self._con.execute(
                        "UPDATE messages SET state='planned', error='' "
                        "WHERE src_uid=? AND state='failed' AND folder_id IN "
                        "(SELECT id FROM folders WHERE mailbox_id=? AND src_name=?)",
                        (r[2], r[0], r[1]))
            self._con.commit()
            return cur.rowcount
        return await self.run(f)

    async def mark_recovered(self, mid: int, folder: str, uid: int):
        def f():
            self._con.execute(
                "UPDATE failed_messages SET status='RECOVERED', recovered_ts=? "
                "WHERE mailbox_id=? AND folder=? AND src_uid=?",
                (time.time(), mid, folder, uid))
            self._con.commit()
        await self.run(f)

    async def mailbox_live_row(self, mid: int) -> Dict:
        """Status + lease + progress for a mailbox (cluster monitor poll)."""
        def f():
            r = self._con.execute(
                "SELECT status, last_error, lease_owner, lease_ts, msgs_total, "
                "msgs_done, bytes_total, bytes_done FROM mailboxes WHERE id=?",
                (mid,)).fetchone()
            return dict(r) if r else {}
        return await self.run(f)

    # ---------------------------------------------------------- folders --

    async def folder_row(self, mid: int, src_name: str, dst_name: str) -> Dict:
        def f():
            self._con.execute(
                "INSERT OR IGNORE INTO folders(mailbox_id, src_name, dst_name) VALUES(?,?,?)",
                (mid, src_name, dst_name))
            self._con.execute(
                "UPDATE folders SET dst_name=? WHERE mailbox_id=? AND src_name=?",
                (dst_name, mid, src_name))
            self._con.commit()
            return dict(self._con.execute(
                "SELECT * FROM folders WHERE mailbox_id=? AND src_name=?",
                (mid, src_name)).fetchone())
        return await self.run(f)

    async def update_folder(self, fid: int, **fields):
        keys = ", ".join(f"{k}=?" for k in fields)
        vals = list(fields.values()) + [fid]

        def f():
            self._con.execute(f"UPDATE folders SET {keys} WHERE id=?", vals)
            self._con.commit()
        await self.run(f)

    async def reset_folder_messages(self, fid: int, keep_done_as_planned: bool):
        """UIDVALIDITY churn: src change -> wipe rows; dst change -> demote
        done rows to planned (presence re-verified by adoption, never blind)."""
        def f():
            if keep_done_as_planned:
                self._con.execute(
                    "UPDATE messages SET state='planned', dst_uid=NULL, origin='' "
                    "WHERE folder_id=? AND state IN ('done','inflight')", (fid,))
                self._con.execute(
                    "UPDATE folders SET adopt_done=0, uv_dst=0 WHERE id=?", (fid,))
            else:
                self._con.execute("DELETE FROM messages WHERE folder_id=?", (fid,))
                self._con.execute(
                    "UPDATE folders SET adopt_done=0, uv_src=0, last_uidnext_src=0, "
                    "msgs_total=0, bytes_total=0, msgs_done=0, bytes_done=0 WHERE id=?", (fid,))
            self._con.commit()
        await self.run(f)

    # ---------------------------------------------------------- messages --

    async def known_uid_intervals(self, fid: int) -> List[Tuple[int, int]]:
        def f():
            rows = self._con.execute(
                "SELECT src_uid FROM messages WHERE folder_id=? ORDER BY src_uid", (fid,)).fetchall()
            return [r[0] for r in rows]
        return to_intervals(await self.run(f))

    async def insert_planned(self, fid: int, rows: List[Tuple[int, int, str, str, Optional[str]]]):
        """rows: (uid, size, flags, internaldate, fp|None)"""
        def f():
            self._con.executemany(
                "INSERT OR IGNORE INTO messages(folder_id,src_uid,size,flags,internaldate,fp) "
                "VALUES(?,?,?,?,?,?)",
                [(fid, u, s, fl, d, fp) for (u, s, fl, d, fp) in rows])
            self._con.commit()
        await self.run(f)

    async def set_fp(self, fid: int, pairs: List[Tuple[int, str]]):
        def f():
            self._con.executemany(
                "UPDATE messages SET fp=? WHERE folder_id=? AND src_uid=?",
                [(fp, fid, uid) for uid, fp in pairs])
            self._con.commit()
        await self.run(f)

    async def rows_by_state(self, fid: int, state: str, limit=0) -> List[Dict]:
        def f():
            q = ("SELECT src_uid,dst_uid,size,flags,internaldate,fp,error FROM messages "
                 "WHERE folder_id=? AND state=? ORDER BY src_uid")
            if limit:
                q += f" LIMIT {int(limit)}"
            return [dict(r) for r in self._con.execute(q, (fid, state)).fetchall()]
        return await self.run(f)

    async def mark_state(self, fid: int, uids: List[int], state: str, error: str = ""):
        def f():
            self._con.executemany(
                "UPDATE messages SET state=?, error=? WHERE folder_id=? AND src_uid=?",
                [(state, error, fid, u) for u in uids])
            self._con.commit()
        await self.run(f)

    async def mark_done(self, fid: int, triples: List[Tuple[int, Optional[int], Optional[str]]],
                        origin: str):
        """triples: (src_uid, dst_uid|None, fp|None)"""
        def f():
            self._con.executemany(
                "UPDATE messages SET state='done', dst_uid=?, origin=?, error='', "
                "fp=COALESCE(?, fp) WHERE folder_id=? AND src_uid=?",
                [(du, origin, fp, fid, su) for (su, du, fp) in triples])
            self._con.commit()
        await self.run(f)

    async def update_flags(self, fid: int, pairs: List[Tuple[int, str]]):
        def f():
            self._con.executemany(
                "UPDATE messages SET flags=? WHERE folder_id=? AND src_uid=?",
                [(fl, fid, u) for u, fl in pairs])
            self._con.commit()
        await self.run(f)

    async def folder_counts(self, fid: int) -> Dict[str, Tuple[int, int]]:
        def f():
            rows = self._con.execute(
                "SELECT state, COUNT(*), COALESCE(SUM(size),0) FROM messages "
                "WHERE folder_id=? GROUP BY state", (fid,)).fetchall()
            return {r[0]: (r[1], r[2]) for r in rows}
        return await self.run(f)

    async def mailbox_totals(self, mid: int) -> Dict[str, int]:
        def f():
            row = self._con.execute(
                "SELECT COALESCE(SUM(m.size),0), COUNT(*) FROM messages m "
                "JOIN folders fo ON fo.id=m.folder_id WHERE fo.mailbox_id=? AND m.state='done'",
                (mid,)).fetchone()
            row2 = self._con.execute(
                "SELECT COALESCE(SUM(m.size),0), COUNT(*) FROM messages m "
                "JOIN folders fo ON fo.id=m.folder_id WHERE fo.mailbox_id=?", (mid,)).fetchone()
            return {"bytes_done": row[0], "msgs_done": row[1],
                    "bytes_total": row2[0], "msgs_total": row2[1]}
        return await self.run(f)

    async def add_event(self, mid: int, fid: int, kind: str, detail: str):
        def f():
            self._con.execute("INSERT INTO events(ts,mailbox_id,folder_id,kind,detail) "
                              "VALUES(?,?,?,?,?)", (time.time(), mid, fid, kind, detail))
            self._con.commit()
        await self.run(f)

    # -------------------------------------------------------- utilities --

    def import_wrapper_state(self, path: str) -> int:
        """Import the old wrapper's migration.state (JSONL, mailbox-granular)."""
        n = 0

        def f():
            nonlocal n
            with open(path, encoding="utf-8") as fh:
                for raw in fh:
                    raw = raw.strip()
                    if not raw:
                        continue
                    try:
                        rec = json.loads(raw)
                    except ValueError:
                        continue
                    if not isinstance(rec, dict) or rec.get("type") != "result":
                        continue
                    if rec.get("status") != "SUCCESS":
                        continue
                    row = {"oldhost": rec.get("oldhost", ""), "olduser": rec.get("olduser", ""),
                           "newhost": rec.get("newhost", ""), "newuser": rec.get("newuser", "")}
                    key = rec.get("key") or mailbox_key(row)
                    self._con.execute(
                        "INSERT OR IGNORE INTO mailboxes(key,src_host,src_user,dst_host,dst_user) "
                        "VALUES(?,?,?,?,?)",
                        (key, row["oldhost"], row["olduser"], row["newhost"], row["newuser"]))
                    self._con.execute(
                        "UPDATE mailboxes SET status='SUCCESS', last_error='' WHERE key=?", (key,))
                    n += 1
            self._con.commit()
        self.run_sync(f)
        return n

    def compact(self) -> int:
        def f():
            cur = self._con.execute(
                "DELETE FROM messages WHERE state='done' AND folder_id IN "
                "(SELECT id FROM folders WHERE status='DONE')")
            self._con.commit()
            self._con.execute("VACUUM")
            return cur.rowcount
        return self.run_sync(f)
