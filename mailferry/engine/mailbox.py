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

Per-mailbox orchestration: connection pairs, folder workers, reconnect
and retry policy (auth failures are NEVER auto-retried — lockout rule).
"""
from __future__ import annotations

import asyncio
import time
from collections import deque
from typing import List, Optional

from ..config import MailboxSpec, RunConfig
from ..errors import (AuthFailed, CommandFailed, ConnectionLost, MFError,
                      PermanentError, ProtocolError, QuotaExceeded, StopRun,
                      TransientError, is_transient, reason_of)
from ..imap.client import ImapClient
from ..util import backoff_delay, fmt_bytes, fmt_dhms, safe_name
from .folder import FolderOutcome, FolderSyncer
from .planner import build_plan


class Pair:
    def __init__(self, spec: MailboxSpec, cfg: RunConfig, mb, log):
        self.spec = spec
        self.cfg = cfg
        self.mb = mb
        self.log = log
        self.src: Optional[ImapClient] = None
        self.dst: Optional[ImapClient] = None

    async def open(self):
        self.src = ImapClient(self.spec.src, self.cfg, side=self.mb.src, log=self.log, role="src")
        self.dst = ImapClient(self.spec.dst, self.cfg, side=self.mb.dst, log=self.log, role="dst")

        async def prep(cli: ImapClient):
            await cli.connect()
            await cli.login()
            await cli.maybe_compress()
        await asyncio.gather(prep(self.src), prep(self.dst))
        self._badges()

    def _badges(self):
        def badge(cli: ImapClient, ep):
            b = []
            b.append({"ssl": "SSL", "tls": "STARTTLS", "none": "PLAIN"}[ep.security])
            if cli.compressed:
                b.append("COMP")
            if cli.has("UIDPLUS"):
                b.append("UID+")
            if cli.literal_plus:
                b.append("LIT+")
            return b
        with self.mb._lock:
            self.mb.src.caps = badge(self.src, self.spec.src)
            self.mb.dst.caps = badge(self.dst, self.spec.dst)

    async def close(self, graceful=True):
        for cli in (self.src, self.dst):
            if cli is None:
                continue
            try:
                if graceful and cli.alive:
                    await cli.logout()
                else:
                    cli.abort()
            except Exception:
                cli.abort()


class Ctx:
    """Everything a FolderSyncer needs, bound to one connection pair."""

    def __init__(self, runner: "MailboxRunner", pair: Pair):
        self.runner = runner
        self.pair = pair
        self.cfg = runner.cfg
        self.db = runner.db
        self.mb = runner.mb
        self.mid = runner.mid
        self.log = runner.log
        self.stop_event = runner.stop_event

    @property
    def src(self):
        return self.pair.src

    @property
    def dst(self):
        return self.pair.dst


class MailboxRunner:
    RECONNECTS_PER_FOLDER = 5

    def __init__(self, spec: MailboxSpec, cfg: RunConfig, db, mb, session,
                 mailbox_logger, stop_event: asyncio.Event, owner: str,
                 extra_slots: Optional[asyncio.Semaphore] = None,
                 host_sems=None):
        self.spec = spec
        self.cfg = cfg
        self.db = db
        self.mb = mb
        self.session = session
        self.logger = mailbox_logger
        self.stop_event = stop_event
        self.owner = owner
        self.extra_slots = extra_slots
        self.host_sems = host_sems
        self.mid = 0
        self.failed_folders: List[str] = []
        self.first_error = ""

    def log(self, msg: str):
        self.logger.log(msg)

    async def _sleep_interruptible(self, secs: float) -> bool:
        try:
            await asyncio.wait_for(self.stop_event.wait(), timeout=secs)
            return True
        except asyncio.TimeoutError:
            return False

    # ---------------------------------------------------------------------

    async def run(self) -> str:
        spec, mb = self.spec, self.mb
        row = await self.db.upsert_mailbox(spec)
        self.mid = row["id"]
        ok, other, age = await self.db.try_lease(self.mid, self.owner)
        if not ok:
            msg = (f"locked by another live instance ({other}, {int(age)}s ago) — "
                   "two instances must never process the same mailbox")
            mb.set(status="FAILED", error=msg, end_time=time.time())
            await self.db.set_mailbox(self.mid, status="FAILED", last_error=msg)
            self.session.log(f"[{spec.index:03d}] {spec.src.user} -> {spec.dst.user}: FAILED ({msg})")
            return "FAILED"

        lease_task = asyncio.get_event_loop().create_task(self._lease_loop())
        max_attempts = 1 + max(0, self.cfg.retries)
        mb.set(max_attempts=max_attempts, start_time=time.time(), status="RUNNING",
               log_path=str(self.logger.path))
        await self.db.set_mailbox(self.mid, status="RUNNING", last_error="")
        self.log(f"=== mailbox {spec.src.label} -> {spec.dst.label} start")
        status = "FAILED"
        try:
            for attempt in range(1, max_attempts + 1):
                mb.set(attempt=attempt)
                try:
                    status = await self._attempt()
                    break
                except StopRun:
                    status = "CANCELLED"
                    break
                except AuthFailed as e:
                    self._fail(e)
                    status = "FAILED"
                    break
                except (QuotaExceeded, PermanentError) as e:
                    self._fail(e)
                    status = "FAILED"
                    break
                except (ConnectionLost, TransientError, ProtocolError, CommandFailed, OSError) as e:
                    reason = reason_of(e)
                    self.first_error = self.first_error or reason
                    if attempt >= max_attempts:
                        self._fail(e)
                        status = "FAILED"
                        break
                    delay = backoff_delay(self.cfg.retry_delay, attempt)
                    self.log(f"attempt {attempt}/{max_attempts} failed ({reason}) — "
                             f"retrying in {int(delay)}s")
                    self.session.log(f"[{spec.index:03d}] {spec.src.user} -> {spec.dst.user}: "
                                     f"attempt {attempt}/{max_attempts} failed ({reason}) — "
                                     f"retrying in {int(delay)}s")
                    mb.set(status="RETRYING", error=reason,
                           retry_wait_until=time.time() + delay)
                    mb.add(retries=1)
                    if await self._sleep_interruptible(delay):
                        status = "CANCELLED"
                        break
                    mb.set(status="RUNNING", retry_wait_until=0.0)
        finally:
            lease_task.cancel()
            await self.db.clear_lease(self.mid, self.owner)

        totals = await self.db.mailbox_totals(self.mid)
        mb.set(status=status, end_time=time.time())
        await self.db.set_mailbox(
            self.mid, status=status, attempts=mb.attempt,
            last_error=self.first_error if status != "SUCCESS" else "",
            msgs_total=totals["msgs_total"], msgs_done=totals["msgs_done"],
            bytes_total=totals["bytes_total"], bytes_done=totals["bytes_done"])
        snap = mb.snap()
        elapsed = fmt_dhms((snap["end"] or time.time()) - snap["start"]) if snap["start"] else "-"
        extra = f" ({self.first_error})" if status not in ("SUCCESS",) and self.first_error else ""
        self.session.log(
            f"[{spec.index:03d}] {spec.src.user} -> {spec.dst.user}: {status}{extra}"
            + (f" attempts={snap['attempt']}" if snap["attempt"] > 1 else "")
            + f" elapsed={elapsed} new={snap['appended']} adopted={snap['adopted']}"
              f" skipped={snap['skipped']} data={fmt_bytes(snap['bytes_done'])}")
        self.log(f"=== mailbox end: {status}{extra}")
        return status

    def _fail(self, e: BaseException):
        reason = reason_of(e)
        self.first_error = self.first_error or reason
        self.mb.set(error=reason)
        self.log(f"FAILED: {reason}")

    async def _lease_loop(self):
        try:
            while True:
                await asyncio.sleep(60)
                await self.db.refresh_lease(self.mid, self.owner)
        except asyncio.CancelledError:
            pass

    # ---------------------------------------------------------------------

    async def _attempt(self) -> str:
        spec, cfg, mb = self.spec, self.cfg, self.mb
        mb.set(op="CONNECT", error="")
        base = Pair(spec, cfg, mb, self.log)
        await base.open()
        self.log(f"connected: src caps={len(base.src.caps)} dst caps={len(base.dst.caps)}"
                 f" (src: {base.src.server_greeting[:60]!r})")
        try:
            mb.set(op="LIST folders")
            plans = await build_plan(base.src, base.dst, cfg, cfg.folder_map(), log=self.log)
            with mb._lock:
                mb.folders_total = len(plans)
                mb.folder_index = 0
                mb.src.folders_total = len(plans)
                mb.dst.folders_total = len(plans)
                mb.src.existing_msgs = 0
                mb.dst.existing_msgs = 0
                mb.msgs_total = 0
                mb.bytes_total = 0
                mb.msgs_done = 0
                mb.bytes_done = 0
            self.log(f"plan: {len(plans)} folder(s), est {sum(p.est_msgs for p in plans)} msgs, "
                     f"{fmt_bytes(sum(p.est_bytes for p in plans))}")
            queue = deque(plans)
            self.failed_folders = []

            # optional extra connection pairs (intra-mailbox parallelism)
            workers = [self._folder_worker(Ctx(self, base), queue, base_pair=True)]
            extra_pairs: List[Pair] = []
            grants = 0
            while (self.extra_slots is not None and grants < cfg.max_conns_per_mailbox - 1
                   and len(queue) > 1 + grants and not self.stop_event.is_set()):
                if self.extra_slots.locked():
                    break
                try:
                    await asyncio.wait_for(self.extra_slots.acquire(), timeout=0.01)
                except asyncio.TimeoutError:
                    break
                grants += 1
            for _ in range(grants):
                p = Pair(spec, cfg, mb, self.log)
                extra_pairs.append(p)
                workers.append(self._extra_worker(p, queue))
            if grants:
                self.log(f"parallel folders: {1 + grants} connection pair(s)")
            try:
                await asyncio.gather(*workers)
            finally:
                for _ in range(grants):
                    self.extra_slots.release()
                for p in extra_pairs:
                    await p.close(graceful=not self.stop_event.is_set())
        finally:
            await base.close(graceful=not self.stop_event.is_set())

        if self.stop_event.is_set() and queue:
            return "CANCELLED"
        snap = mb.snap()
        if self.failed_folders:
            any_progress = snap["msgs_done"] > 0
            return "PARTIAL" if any_progress else "FAILED"
        if snap["skipped"] > 0:
            return "PARTIAL"
        return "SUCCESS"

    async def _extra_worker(self, pair: Pair, queue: deque):
        try:
            await pair.open()
        except (MFError, OSError) as e:
            self.log(f"extra connection pair unavailable: {reason_of(e)}")
            return
        await self._folder_worker(Ctx(self, pair), queue, base_pair=False)

    async def _folder_worker(self, ctx: Ctx, queue: deque, base_pair: bool):
        while queue and not self.stop_event.is_set():
            plan = queue.popleft()
            try:
                await self._sync_folder_with_reconnect(ctx, plan)
            except StopRun:
                queue.appendleft(plan)
                if base_pair:
                    raise
                return

    async def _sync_folder_with_reconnect(self, ctx: Ctx, plan):
        for rc in range(self.RECONNECTS_PER_FOLDER + 1):
            try:
                outcome: FolderOutcome = await FolderSyncer(ctx, plan).run()
                if not outcome.ok:
                    self.failed_folders.append(plan.src_display)
                    self.first_error = self.first_error or outcome.error
                return
            except StopRun:
                raise
            except AuthFailed:
                raise
            except (QuotaExceeded,) as e:
                raise
            except (ConnectionLost, ProtocolError, TransientError, OSError,
                    CommandFailed) as e:
                reason = reason_of(e)
                if rc >= self.RECONNECTS_PER_FOLDER:
                    self.log(f"[{plan.src_display}] giving up after "
                             f"{rc} reconnect(s): {reason}")
                    self.failed_folders.append(plan.src_display)
                    self.first_error = self.first_error or reason
                    return
                delay = backoff_delay(2.0, rc + 1, cap=60.0)
                self.log(f"[{plan.src_display}] connection trouble ({reason}) — "
                         f"reconnecting in {delay:.0f}s")
                with self.mb._lock:
                    self.mb.src.reconnects += 1
                self.mb.set(op=f"RECONNECT in {int(delay)}s", detail=reason[:120])
                await ctx.pair.close(graceful=False)
                if await self._sleep_interruptible(delay):
                    raise StopRun()
                self.mb.set(op="RECONNECT")
                await ctx.pair.open()
