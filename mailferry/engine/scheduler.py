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

Batch scheduler: admission control, per-host connection budgets,
graceful two-stage shutdown.
"""
from __future__ import annotations

import asyncio
import time
from typing import Dict, List

from ..config import MailboxSpec, RunConfig
from ..state.db import StateDB, lease_owner_id
from ..util import safe_name
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
                        mailbox_logger_factory, stop_event: asyncio.Event) -> dict:
    db = StateDB(cfg.db_path, ephemeral=cfg.ephemeral)
    owner = lease_owner_id(cfg.run_id)
    await db.start_run(cfg.run_id, cfg.csv_file)

    pending: List[MailboxSpec] = []
    prior: Dict[int, dict] = {}
    stale_notes: List[str] = []
    for spec in specs:
        row = await db.upsert_mailbox(spec)
        prior[spec.index] = row
        mb = stats.mailbox(spec.index, spec.label, spec.src.host, spec.dst.host, spec.dst.user)
        if (cfg.skip_completed and not cfg.force and row["status"] == "SUCCESS"):
            mb.set(status="SKIPPED")
            stats.skipped_prior += 1
            continue
        if row["status"] == "RUNNING" and row["lease_owner"] and row["lease_owner"] != owner:
            age = time.time() - (row["lease_ts"] or 0)
            if age >= StateDB.LEASE_FRESH:
                stale_notes.append(
                    f"{spec.src.user}: marked RUNNING by {row['lease_owner']} "
                    f"{int(age)}s ago (stale — that run crashed or was killed; resuming is correct)")
        pending.append(spec)

    if cfg.order == "size":
        pending.sort(key=lambda s: -(prior[s.index].get("bytes_total") or 0))
    for note in stale_notes:
        session.log(f"note: {note}")

    workers = max(1, min(cfg.workers, len(pending))) if pending else 0
    stats.workers = workers
    worker_sem = asyncio.Semaphore(max(1, workers))
    hosts = HostSems(cfg.per_host_conns)
    extra_slots = asyncio.Semaphore(max(0, workers * (max(1, cfg.max_conns_per_mailbox) - 1)))

    async def run_one(spec: MailboxSpec):
        mb = stats.mailbox(spec.index, spec.label, spec.src.host, spec.dst.host, spec.dst.user)
        async with worker_sem:
            if stop_event.is_set():
                mb.set(status="CANCELLED")
                return
            await hosts.acquire_pair(spec.src.host, spec.dst.host)
            try:
                runner = MailboxRunner(
                    spec, cfg, db, mb, session,
                    mailbox_logger_factory(spec), stop_event, owner,
                    extra_slots=extra_slots, host_sems=hosts)
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

    tasks = [asyncio.get_event_loop().create_task(run_one(s)) for s in pending]
    try:
        if tasks:
            await asyncio.gather(*tasks, return_exceptions=True)
    finally:
        snap = stats.snapshot()
        counts = snap["counts"]
        result = (f"ok={counts.get('SUCCESS', 0)} partial={counts.get('PARTIAL', 0)} "
                  f"failed={counts.get('FAILED', 0)} cancelled={counts.get('CANCELLED', 0)}")
        await db.end_run(cfg.run_id, result)
        db.close()

    for t in tasks:
        if not t.done():
            t.cancel()
    return stats.snapshot()
