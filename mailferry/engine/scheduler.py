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

Batch scheduler: admission control, per-host connection budgets, dynamic
re-queueing (console `retry` / `reload`), graceful two-stage shutdown.
"""
from __future__ import annotations

import asyncio
import time
from typing import Dict, List

from ..config import MailboxSpec, RunConfig, parse_csv
from ..state.db import StateDB, lease_owner_id
from .mailbox import MailboxRunner


class HostSems:
    def __init__(self, per_host: int):
        self.per_host = max(1, per_host)
        self._sems: Dict[str, asyncio.Semaphore] = {}

    def sem(self, host: str) -> asyncio.Semaphore:
        h = host.lower()
        if h not in self._sems:
            self._sems[h] = asyncio.Semaphore(self.per_host)
        return self._sems[h]

    async def acquire_pair(self, a: str, b: str):
        for h in sorted((a.lower(), b.lower())):
            await self.sem(h).acquire()

    def release_pair(self, a: str, b: str):
        for h in (a.lower(), b.lower()):
            self.sem(h).release()


async def run_migration(cfg: RunConfig, specs: List[MailboxSpec], stats, session,
                        mailbox_logger_factory, stop_event: asyncio.Event,
                        hub=None) -> dict:
    loop = asyncio.get_event_loop()
    db = StateDB(cfg.db_path, ephemeral=cfg.ephemeral,
                 lease_fresh=getattr(cfg, "lock_timeout", StateDB.LEASE_FRESH))
    owner = lease_owner_id(cfg.run_id)
    await db.start_run(cfg.run_id, cfg.csv_file)
    await db.register_worker(owner, cfg.run_id)      # join the cluster
    if hub is not None:
        hub.worker_id = owner
        hub.add_history("Run started", "OK", "-",
                        f"{cfg.csv_file} · {len(specs)} mailbox(es) · run {cfg.run_id}")
        hub.add_history("Worker joined", "OK", "-",
                        f"{owner} — cluster on {cfg.db_path}")
        session.log(f"cluster: joined as worker {owner} "
                    f"(offline threshold {int(cfg.worker_timeout)}s)")

    pending: List[MailboxSpec] = []
    known_keys = set()
    for spec in specs:
        known_keys.add(spec.key)
        row = await db.upsert_mailbox(spec)
        mb = stats.mailbox(spec.index, spec.label, spec.src.host, spec.dst.host, spec.dst.user)
        if cfg.skip_completed and not cfg.force and row["status"] == "SUCCESS":
            mb.set(status="SKIPPED")
            stats.skipped_prior += 1
            continue
        if row["status"] == "RUNNING" and row["lease_owner"] and row["lease_owner"] != owner:
            age = time.time() - (row["lease_ts"] or 0)
            if age >= db.lease_fresh:
                session.log(f"note: {spec.src.user} was marked RUNNING by {row['lease_owner']} "
                            f"{int(age)}s ago (stale — that run crashed; resuming is correct)")
        pending.append(spec)

    if cfg.order == "size":
        sizes = {}
        for spec in pending:
            r = await db.upsert_mailbox(spec)
            sizes[spec.key] = r.get("bytes_total") or 0
        pending.sort(key=lambda s: -sizes.get(s.key, 0))

    workers = max(1, min(cfg.workers, len(pending))) if pending else 1
    stats.workers = workers
    worker_sem = asyncio.Semaphore(workers)
    hosts = HostSems(cfg.per_host_conns)
    extra_slots = asyncio.Semaphore(max(0, workers * (max(1, cfg.max_conns_per_mailbox) - 1)))
    tasks: List[asyncio.Task] = []

    async def run_one(spec: MailboxSpec):
        mb = stats.mailbox(spec.index, spec.label, spec.src.host, spec.dst.host, spec.dst.user)
        if hub is not None:
            hub.active_labels.add(spec.label)
        try:
            async with worker_sem:
                if stop_event.is_set():
                    mb.set(status="CANCELLED")
                    return
                await hosts.acquire_pair(spec.src.host, spec.dst.host)
                try:
                    runner = MailboxRunner(
                        spec, cfg, db, mb, session,
                        mailbox_logger_factory(spec), stop_event, owner,
                        extra_slots=extra_slots, host_sems=hosts, hub=hub)
                    await runner.run()
                except asyncio.CancelledError:
                    mb.set(status="CANCELLED", end_time=time.time())
                    raise
                except BaseException as e:      # a runner bug must not sink the batch
                    from ..errors import reason_of
                    mb.set(status="FAILED", error=f"internal error: {reason_of(e)}",
                           end_time=time.time())
                    session.log(f"[{spec.index:03d}] {spec.src.user}: INTERNAL ERROR {e!r}")
                    if cfg.debug:
                        raise
                finally:
                    hosts.release_pair(spec.src.host, spec.dst.host)
        finally:
            if hub is not None:
                hub.active_labels.discard(spec.label)

    def spawn(spec: MailboxSpec):
        mb = stats.mailbox(spec.index, spec.label, spec.src.host, spec.dst.host, spec.dst.user)
        mb.set(status="QUEUED", error="", end_time=0.0, retry_wait_until=0.0, attempt=1)
        tasks.append(loop.create_task(run_one(spec)))

    def reload_csv():
        added, known = 0, 0
        for spec in parse_csv(cfg.csv_file):
            if spec.key in known_keys:
                known += 1
                continue
            known_keys.add(spec.key)
            if hub is not None:
                hub.specs_by_label[spec.label] = spec
            spawn(spec)
            session.log(f"console: reload added {spec.src.user} -> {spec.dst.user}")
            added += 1
        return added, known

    if hub is not None:
        hub.db = db
        hub.spawn = spawn
        hub.reload_csv = reload_csv
        for spec in specs:
            hub.specs_by_label[spec.label] = spec

    for spec in pending:
        spawn(spec)

    supervisor_task = None
    if hub is not None and cfg.stale_timeout > 0:
        from .supervisor import stale_supervisor
        supervisor_task = loop.create_task(
            stale_supervisor(cfg, stats, hub, session, stop_event))
    monitor_task = None
    if hub is not None:
        from .cluster import cluster_monitor
        monitor_task = loop.create_task(
            cluster_monitor(cfg, stats, hub, session, db, owner, stop_event, spawn))

    try:
        while True:
            alive = [t for t in tasks if not t.done()]
            if alive:
                await asyncio.wait(alive, return_when=asyncio.FIRST_COMPLETED)
                continue
            if hub is not None and hub.remote_watch and not stop_event.is_set():
                # our local work is done but peers still hold mailboxes from
                # this project: stay alive — mirror their progress and stand
                # by for automatic failover (the monitor re-queues via spawn)
                try:
                    await asyncio.wait_for(stop_event.wait(), timeout=2.0)
                except asyncio.TimeoutError:
                    pass
                continue
            break
    finally:
        if supervisor_task is not None:
            supervisor_task.cancel()
        if monitor_task is not None:
            monitor_task.cancel()
        try:
            await db.deregister_worker(owner)        # graceful release
        except Exception:
            pass
        if hub is not None:
            hub.spawn = None                    # run is winding down
            hub.reload_csv = None
            if hub.shutdown_active:
                # scheduler stopped admitting; workers have now drained and
                # each runner already closed its connections with LOGOUT.
                hub.set_phase("scheduler", "done")
                hub.set_phase("workers", "done")
                hub.set_phase("conns", "done")
                hub.set_phase("state", "active")
        snap = stats.snapshot()
        counts = snap["counts"]
        result = (f"ok={counts.get('SUCCESS', 0)} partial={counts.get('PARTIAL', 0)} "
                  f"failed={counts.get('FAILED', 0)} stale={counts.get('STALE', 0)} "
                  f"cancelled={counts.get('CANCELLED', 0)}"
                  + (f" remote={counts.get('REMOTE', 0)}" if counts.get("REMOTE") else ""))
        if hub is not None:
            hub.add_history("Run finished", "OK", "-", result)
            try:
                hub.failed_registry = await db.failed_rows(
                    statuses=("FAILED", "RETRY_PENDING", "RETRYING"))
            except Exception:
                hub.failed_registry = []
        await db.end_run(cfg.run_id, result)
        db.close()
        if hub is not None and hub.shutdown_active:
            hub.set_phase("state", "done")       # State Database committed + closed

    for t in tasks:
        if not t.done():
            t.cancel()
    return stats.snapshot()
