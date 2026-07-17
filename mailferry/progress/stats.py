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

Live telemetry model. Engine (event loop) mutates under a lock; the
renderer thread pulls plain-dict snapshots. Byte counters tick on every
socket read/write, so the dashboard can never mistake slow for stalled.
"""
from __future__ import annotations

import threading
import time
from typing import Dict, List, Optional


class SideStats:
    """One side (source or destination) of one mailbox."""

    def __init__(self, lock: threading.Lock, host: str):
        self._lock = lock
        self.host = host
        self.conn_state = "-"
        self.caps: List[str] = []
        self.folders_total = 0
        self.existing_msgs = 0
        self.quota_pct: float = -1.0
        self.reconnects = 0
        self.rx_bytes = 0
        self.tx_bytes = 0

    # called from the IMAP client
    def rx(self, n):
        with self._lock:
            self.rx_bytes += n

    def tx(self, n):
        with self._lock:
            self.tx_bytes += n

    def state(self, s):
        with self._lock:
            self.conn_state = s

    def snap(self) -> dict:
        return {"host": self.host, "state": self.conn_state, "caps": list(self.caps),
                "folders": self.folders_total, "existing": self.existing_msgs,
                "quota": self.quota_pct, "reconnects": self.reconnects,
                "rx": self.rx_bytes, "tx": self.tx_bytes}


class MailboxStats:
    def __init__(self, lock: threading.Lock, index: int, label: str, src_host: str, dst_host: str,
                 label2: str = ""):
        self._lock = lock
        self.index = index
        self.label = label
        self.label2 = label2
        self.status = "QUEUED"
        self.attempt = 1
        self.max_attempts = 1
        self.op = ""
        self.detail = ""
        self.current_folder = ""
        self.folder_index = 0
        self.folders_total = 0
        self.msgs_done = 0
        self.msgs_total = 0
        self.bytes_done = 0
        self.bytes_total = 0
        self.appended = 0
        self.adopted = 0
        self.dup_skipped = 0
        self.skipped = 0
        self.retries = 0
        self.error = ""
        self.retry_wait_until = 0.0
        self.start_time = 0.0
        self.end_time = 0.0
        self.src = SideStats(lock, src_host)
        self.dst = SideStats(lock, dst_host)
        self.log_path = ""

    def set(self, **kw):
        with self._lock:
            for k, v in kw.items():
                setattr(self, k, v)

    def add(self, **kw):
        with self._lock:
            for k, v in kw.items():
                setattr(self, k, getattr(self, k) + v)

    def snap(self) -> dict:
        return {
            "index": self.index, "label": self.label, "label2": self.label2,
            "status": self.status,
            "attempt": self.attempt, "max_attempts": self.max_attempts,
            "op": self.op, "detail": self.detail,
            "folder": self.current_folder, "fi": self.folder_index, "ft": self.folders_total,
            "msgs_done": self.msgs_done, "msgs_total": self.msgs_total,
            "bytes_done": self.bytes_done, "bytes_total": self.bytes_total,
            "appended": self.appended, "adopted": self.adopted,
            "dup_skipped": self.dup_skipped, "skipped": self.skipped,
            "retries": self.retries, "error": self.error,
            "retry_wait_until": self.retry_wait_until,
            "start": self.start_time, "end": self.end_time,
            "src": self.src.snap(), "dst": self.dst.snap(),
            "log": self.log_path,
        }


class Stats:
    def __init__(self):
        self.lock = threading.Lock()
        self.mailboxes: Dict[int, MailboxStats] = {}
        self.batch_start = time.time()
        self.mode = "Migration"
        self.workers = 0
        self.csv_file = ""
        self.db_path = ""
        self.logs_dir = ""
        self.skipped_prior = 0
        self.interrupted = False

    def mailbox(self, index: int, label: str, src_host: str, dst_host: str,
                label2: str = "") -> MailboxStats:
        with self.lock:
            mb = self.mailboxes.get(index)
            if mb is None:
                mb = MailboxStats(self.lock, index, label, src_host, dst_host, label2)
                self.mailboxes[index] = mb
            return mb

    def snapshot(self) -> dict:
        with self.lock:
            mbs = [m.snap() for m in self.mailboxes.values()]
        mbs.sort(key=lambda m: m["index"])
        agg = {
            "msgs_done": sum(m["msgs_done"] for m in mbs),
            "msgs_total": sum(m["msgs_total"] for m in mbs),
            "bytes_done": sum(m["bytes_done"] for m in mbs),
            "bytes_total": sum(m["bytes_total"] for m in mbs),
            "appended": sum(m["appended"] for m in mbs),
            "adopted": sum(m["adopted"] for m in mbs),
            "dup_skipped": sum(m["dup_skipped"] for m in mbs),
            "skipped_msgs": sum(m["skipped"] for m in mbs),
            "retries": sum(m["retries"] for m in mbs),
            "reconnects": sum(m["src"]["reconnects"] + m["dst"]["reconnects"] for m in mbs),
            "wire_rx": sum(m["src"]["rx"] + m["dst"]["rx"] for m in mbs),
            "wire_tx": sum(m["src"]["tx"] + m["dst"]["tx"] for m in mbs),
        }
        counts: Dict[str, int] = {}
        for m in mbs:
            counts[m["status"]] = counts.get(m["status"], 0) + 1
        return {
            "ts": time.time(), "batch_start": self.batch_start, "mode": self.mode,
            "workers": self.workers, "csv": self.csv_file, "db": self.db_path,
            "logs": self.logs_dir, "skipped_prior": self.skipped_prior,
            "interrupted": self.interrupted,
            "mailboxes": mbs, "agg": agg, "counts": counts,
        }


class RateTracker:
    """Renderer-side rate/ETA computation from consecutive snapshots."""

    def __init__(self, window: float = 10.0):
        self.window = window
        self.samples: List[tuple] = []       # (t, bytes, msgs)

    def update(self, t: float, nbytes: int, msgs: int):
        self.samples.append((t, nbytes, msgs))
        cutoff = t - self.window
        while len(self.samples) > 2 and self.samples[0][0] < cutoff:
            self.samples.pop(0)

    def rates(self) -> tuple:
        if len(self.samples) < 2:
            return 0.0, 0.0
        t0, b0, m0 = self.samples[0]
        t1, b1, m1 = self.samples[-1]
        dt = max(1e-6, t1 - t0)
        return max(0.0, (b1 - b0) / dt), max(0.0, (m1 - m0) / dt)

    def eta(self, remaining_bytes: int, remaining_msgs: int) -> Optional[float]:
        br, mr = self.rates()
        if remaining_bytes > 0 and br > 1:
            return remaining_bytes / br
        if remaining_msgs > 0 and mr > 0.01:
            return remaining_msgs / mr
        return None
