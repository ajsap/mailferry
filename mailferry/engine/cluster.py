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

Cluster coordination: several MailFerry instances sharing one State
Database cooperate as workers on the same migration project.

- every instance registers as a Worker (hostname:pid:uuid) and heartbeats
- mailboxes are claimed atomically through the per-mailbox lease (CAS)
- a mailbox owned by a live peer shows as REMOTE with mirrored progress
- a worker silent for --worker-timeout is offline: its mailboxes are
  reclaimed automatically and resume from the last confirmed checkpoint
- graceful exits release ownership immediately (deregister_worker)

This monitor never blocks migration: it is a low-frequency polling task
beside the scheduler.
"""
from __future__ import annotations

import asyncio
import time

from ..state.db import short_worker
from ..util import fmt_bytes


async def cluster_monitor(cfg, stats, hub, session, db, owner: str,
                          stop_event: asyncio.Event, spawn):
    """Heartbeats this worker, keeps the roster fresh, mirrors REMOTE
    progress and performs automatic failover reclaim."""
    beat = 0.0
    while not stop_event.is_set():
        try:
            await asyncio.wait_for(stop_event.wait(), timeout=3.0)
            return
        except asyncio.TimeoutError:
            pass
        now = time.time()
        try:
            if now - beat >= max(5.0, cfg.db_heartbeat * 0.8):
                beat = now
                await db.worker_heartbeat(owner)
                hub.cluster = await db.list_workers(cfg.worker_timeout)
            await _poll_remote(cfg, stats, hub, session, db, owner, spawn)
        except Exception as e:                       # monitor must never die
            session.log(f"cluster monitor: {e!r}")


async def _poll_remote(cfg, stats, hub, session, db, owner, spawn):
    for label in list(hub.remote_watch.keys()):
        w = hub.remote_watch.get(label)
        if w is None:
            continue
        spec, mid = w["spec"], w["mid"]
        mb = stats.mailbox(spec.index, spec.label, spec.src.host,
                           spec.dst.host, spec.dst.user)
        row = await db.mailbox_live_row(mid)
        if not row:
            continue
        lease_owner = row.get("lease_owner") or ""
        lease_ts = float(row.get("lease_ts") or 0)

        # 1) finished remotely -> reflect the final state and stop watching
        if (not lease_owner or lease_owner == owner) and \
                row.get("status") in ("SUCCESS", "PARTIAL", "FAILED", "STALE", "CANCELLED"):
            status = row["status"]
            del hub.remote_watch[label]
            mb.set(status=status, end_time=time.time(),
                   op=f"worker {short_worker(w['owner'])}",
                   msgs_total=row.get("msgs_total") or mb.msgs_total,
                   msgs_done=row.get("msgs_done") or mb.msgs_done,
                   bytes_total=row.get("bytes_total") or mb.bytes_total,
                   bytes_done=row.get("bytes_done") or mb.bytes_done,
                   error=(row.get("last_error") or "") if status != "SUCCESS" else "")
            session.log(f"[{spec.index:03d}] {spec.src.user}: {status} "
                        f"(completed by worker {short_worker(w['owner'])})")
            hub.add_history(f"Completed by another worker",
                            "OK" if status == "SUCCESS" else "WARN", label,
                            f"{short_worker(w['owner'])} finished with {status}")
            continue

        # 2) lease freed but unfinished (peer exited gracefully mid-project)
        if not lease_owner or lease_owner == owner:
            del hub.remote_watch[label]
            session.log(f"[{spec.index:03d}] {spec.src.user}: released by "
                        f"worker {short_worker(w['owner'])} — resuming here")
            hub.add_history("Job released — resuming", "OK", label,
                            f"worker {short_worker(w['owner'])} released the mailbox")
            mb.set(status="QUEUED", op="", detail="")
            spawn(spec)
            continue

        # 3) owner changed hands (another peer reclaimed first) — keep watching
        if lease_owner != w["owner"]:
            w["owner"] = lease_owner
            mb.set(op=f"worker {short_worker(lease_owner)}",
                   detail=f"processed by {lease_owner}")

        # 4) owner offline? verified reclaim (CAS — a live worker survives it)
        hb_age = await db.worker_hb_age(lease_owner)
        lease_age = time.time() - lease_ts if lease_ts else 1e9
        dead = (hb_age is not None and hb_age > cfg.worker_timeout) \
            or (hb_age is None and lease_age > 150.0)
        if dead:
            if await db.force_lease(mid, owner, lease_owner, lease_ts):
                del hub.remote_watch[label]
                who = short_worker(lease_owner)
                session.log(f"[{spec.index:03d}] {spec.src.user}: worker {who} went "
                            f"offline — reclaimed; resuming from the last checkpoint")
                hub.add_history("Reclaimed from offline worker", "WARN", label,
                                f"worker {who} silent for "
                                f"{int(hb_age if hb_age is not None else lease_age)}s — "
                                "resuming from the last confirmed checkpoint")
                mb.set(status="QUEUED", op="", detail="", error="")
                # keep the CAS claim: the runner re-leases as the same owner
                spawn(spec)
            continue

        # 5) alive and working: mirror progress for the local dashboard
        hb_txt = f"hb {int(hb_age)}s" if hb_age is not None else f"lease {int(lease_age)}s"
        totals = await db.mailbox_totals(mid)
        mb.set(op=f"worker {short_worker(lease_owner)} · {hb_txt}",
               msgs_total=totals["msgs_total"] or mb.msgs_total,
               msgs_done=totals["msgs_done"],
               bytes_total=totals["bytes_total"] or mb.bytes_total,
               bytes_done=totals["bytes_done"])
