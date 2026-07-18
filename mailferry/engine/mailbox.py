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
from ..errors import (AuthFailed, CommandFailed, ConnectionLost, LeaseLost,
                      MFError, PermanentError, ProtocolError, QuotaExceeded,
                      StaleFailed, StaleKick, StopRun, TransientError,
                      is_transient, reason_of)
from ..state.db import StateDB, short_worker
from ..imap.client import ImapClient
from ..util import backoff_delay, fmt_bytes, fmt_dhms, safe_name
from .folder import FolderOutcome, FolderSyncer
from .planner import build_plan


class Pair:
    def __init__(self, spec: MailboxSpec, cfg: RunConfig, mb, log, hub=None):
        self.spec = spec
        self.cfg = cfg
        self.mb = mb
        self.log = log
        self.hub = hub
        self.src: Optional[ImapClient] = None
        self.dst: Optional[ImapClient] = None

    async def open(self):
        self.src = ImapClient(self.spec.src, self.cfg, side=self.mb.src, log=self.log,
                              role="src", hub=self.hub, owner_label=self.spec.label)
        self.dst = ImapClient(self.spec.dst, self.cfg, side=self.mb.dst, log=self.log,
                              role="dst", hub=self.hub, owner_label=self.spec.label)

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
        self.hub = runner.hub

    @property
    def src(self):
        return self.pair.src

    @property
    def dst(self):
        return self.pair.dst

    def set_existing(self, folder: str, n: int):
        """Track pre-existing Destination Server messages per folder —
        idempotent across reconnect re-entries (no double counting)."""
        r = self.runner
        r._existing[folder] = n
        with self.mb._lock:
            self.mb.dst.existing_msgs = sum(r._existing.values())


class MailboxRunner:
    RECONNECTS_PER_FOLDER = 5

    def __init__(self, spec: MailboxSpec, cfg: RunConfig, db, mb, session,
                 mailbox_logger, stop_event: asyncio.Event, owner: str,
                 extra_slots: Optional[asyncio.Semaphore] = None,
                 host_sems=None, hub=None):
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
        self.hub = hub
        self.mid = 0
        self.failed_folders: List[str] = []
        self.first_error = ""
        self._existing: dict = {}
        self._started_folders: set = set()
        self.lease_lost = asyncio.Event()   # another worker took this mailbox
        self.poison: dict = {}              # per-folder failure-isolation state

    def poison_rec(self, folder: str) -> dict:
        """Per-folder failure-isolation state — survives reconnect
        re-entries, which is what breaks the endless retry-the-same-batch
        loop."""
        return self.poison.setdefault(folder, {
            "losses": 0, "suspects": set(), "ladder": [],
            "no_counts": {}, "loss_marks": {}, "iso_losses": 0,
            "announced": False,
        })

    def _note_error(self, reason: str):
        if self.hub is not None:
            self.hub.note_error(self.spec.src.user, reason)

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
        if ok and other and other != self.owner and age >= self.db.lease_fresh:
            msg = (f"stale lock auto-reset: {other} last heartbeat {int(age)}s ago "
                   f"(> {int(self.db.lease_fresh)}s) — that worker is dead; continuing")
            self.session.log(f"[{spec.index:03d}] {spec.src.user}: {msg}")
            if self.hub is not None:
                self.hub.add_history("Stale lock auto-reset", "OK", spec.label,
                                     f"held by {other} — heartbeat {int(age)}s ago")
        if not ok:
            ok = await self._try_reclaim_dead_worker(other, age)
        if not ok:
            # cluster mode: another live worker owns this mailbox. Do not
            # fail — mark it REMOTE and let the cluster monitor mirror its
            # progress and reclaim it automatically if that worker dies.
            who = short_worker(other)
            mb.set(status="REMOTE", op=f"worker {who}",
                   detail=f"processed by {other}", error="",
                   start_time=time.time())
            self.session.log(f"[{spec.index:03d}] {spec.src.user} -> {spec.dst.user}: "
                             f"REMOTE — being processed by worker {who} "
                             f"(heartbeat {int(age)}s ago); watching")
            if self.hub is not None:
                self.hub.watch_remote(spec, self.mid, other)
                self.hub.add_history("Mailbox on another worker", "OK", spec.label,
                                     f"claimed by {who} — this instance watches and "
                                     "takes over automatically if it goes offline")
            return "REMOTE"

        lease_task = asyncio.get_event_loop().create_task(self._lease_loop())
        max_attempts = 1 + max(0, self.cfg.retries)
        mb.set(max_attempts=max_attempts, start_time=time.time(), status="RUNNING",
               log_path=str(self.logger.path))
        await self.db.set_mailbox(self.mid, status="RUNNING", last_error="")
        self.log(f"=== mailbox {spec.src.label} -> {spec.dst.label} start")
        if self.hub is not None:
            self.hub.add_history("Migration started", "OK", spec.label,
                                 f"{spec.src.user} → {spec.dst.user}")
        status = "FAILED"
        try:
            attempt = 0
            kicks = 0
            while True:
                attempt += 1
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
                except StaleFailed as e:
                    self._fail(e)
                    status = "STALE"
                    break
                except LeaseLost:
                    status = "REMOTE"
                    break
                except (ConnectionLost, TransientError, ProtocolError, CommandFailed, OSError) as e:
                    if self.lease_lost.is_set():
                        status = "REMOTE"
                        break
                    if isinstance(e, StaleKick) and kicks < 50:
                        # supervisor-forced reconnect: never consumes the
                        # retry budget; resume from the last checkpoint
                        kicks += 1
                        attempt -= 1
                        if self.hub is not None and self.hub.is_stale_failed(spec.label):
                            self._fail(StaleFailed(
                                f"recovery exhausted after "
                                f"{self.hub.stale_attempts(spec.label)} attempt(s)"))
                            status = "STALE"
                            break
                        self.log("stale recovery: forced reconnect "
                                 "(does not consume the retry budget)")
                        if await self._sleep_interruptible(1.0):
                            status = "CANCELLED"
                            break
                        continue
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
            await self.db.clear_lease(self.mid, self.owner)   # owner-guarded

        if status == "REMOTE":
            # lost the lease to a failover mid-run: hand off cleanly — the
            # new owner writes the row from here; we just watch.
            owner_now, ts_now = await self.db.read_lease(self.mid)
            who = short_worker(owner_now)
            mb.set(status="REMOTE", op=f"worker {who}", recovering=0,
                   detail=f"taken over by {owner_now}", retry_wait_until=0.0)
            self.session.log(f"[{spec.index:03d}] {spec.src.user} -> {spec.dst.user}: "
                             f"REMOTE — taken over by worker {who}; watching")
            if self.hub is not None and owner_now:
                self.hub.watch_remote(spec, self.mid, owner_now)
            return "REMOTE"

        totals = await self.db.mailbox_totals(self.mid)
        mb.set(status=status, end_time=time.time(), recovering=0)
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
        if self.hub is not None:
            # close any stale-recovery episode still open at the finish line
            att = self.hub.stale_attempts_by.pop(spec.label, None)
            if att and spec.label not in self.hub.stale_failed \
                    and status in ("SUCCESS", "PARTIAL"):
                self.hub.stats.bump("stalls_recovered")
                self.session.log(f"transfer recovered: {spec.label} resumed and "
                                 f"completed (reconnect {att})")
                self.hub.log_event("INFO", spec.label,
                                   "transfer recovered — mailbox completed")
                self.hub.add_history("Transfer recovered", "OK", spec.label,
                                     f"resumed and completed (reconnect {att})")
        if self.hub is not None:
            word = {"SUCCESS": ("Migration completed", "OK"),
                    "WARNINGS": ("Completed with warnings", "WARN"),
                    "PARTIAL": ("Migration partial", "WARN"),
                    "FAILED": ("Migration failed", "FAIL"),
                    "STALE": ("Recovery exhausted — marked STALE", "FAIL"),
                    "CANCELLED": ("Migration cancelled", "WARN")}.get(
                        status, (f"Migration {status}", "OK"))
            det = (f"total {fmt_bytes(snap['bytes_done'])} · "
                   f"{snap['msgs_done']:,} msgs · elapsed {elapsed}")
            if status == "WARNINGS":
                nf = snap.get("failed", 0) + snap.get("skipped", 0)
                tot = snap["msgs_total"] or (snap["msgs_done"] + nf)
                pctv = (snap["msgs_done"] * 100.0 / tot) if tot else 100.0
                det = (f"{snap['msgs_done']:,}/{tot:,} migrated · {nf} failed · "
                       f"{pctv:.2f}% complete — see the Failed Message Registry")
            elif status not in ("SUCCESS",) and self.first_error:
                det += f" · {self.first_error}"
            self.hub.add_history(word[0], word[1], spec.label, det)
        return status

    LEGACY_STALE = 150.0    # lease age that means a legacy (pre-cluster) writer died

    async def _try_reclaim_dead_worker(self, other: str, age: float) -> bool:
        """The mailbox is leased by another worker. If that worker is
        provably dead (no cluster heartbeat for --worker-timeout, or a
        legacy lease with no refresh for 2.5x the old refresh interval),
        take the job over with an atomic compare-and-swap. A live worker
        can never be dispossessed: the CAS fails if it refreshed."""
        hb_age = await self.db.worker_hb_age(other)
        dead = (hb_age is not None and hb_age > self.cfg.worker_timeout) \
            or (hb_age is None and age > self.LEGACY_STALE)
        if not dead:
            return False
        obs_owner, obs_ts = await self.db.read_lease(self.mid)
        if obs_owner != other:
            ok, _, _ = await self.db.try_lease(self.mid, self.owner)
            return ok
        if not await self.db.force_lease(self.mid, self.owner, obs_owner, obs_ts):
            return False                    # it came back to life — stay REMOTE
        who = short_worker(other)
        self.log(f"reclaimed from offline worker {other} "
                 f"(silent {int(hb_age if hb_age is not None else age)}s)")
        self.session.log(f"[{self.spec.index:03d}] {self.spec.src.user}: reclaimed from "
                         f"offline worker {who} — resuming from the last checkpoint")
        if self.hub is not None:
            self.hub.add_history("Reclaimed from offline worker", "WARN", self.spec.label,
                                 f"worker {who} went silent — resuming from the last "
                                 "confirmed checkpoint (duplicate-safe)")
        return True

    def _fail(self, e: BaseException):
        reason = reason_of(e)
        self.first_error = self.first_error or reason
        self.mb.set(error=reason)
        self.log(f"FAILED: {reason}")
        self._note_error(reason)

    async def _lease_loop(self):
        try:
            while True:
                await asyncio.sleep(max(5.0, self.cfg.db_heartbeat))
                if await self.db.refresh_lease(self.mid, self.owner):
                    continue
                # Failover: another worker CAS-claimed this mailbox after our
                # heartbeats went silent. Stop ALL work on it immediately —
                # the new owner resumes from the checkpoint; per-message
                # intent rows keep the handover duplicate-safe.
                self.lease_lost.set()
                who = ""
                try:
                    owner_now, _ = await self.db.read_lease(self.mid)
                    who = short_worker(owner_now)
                except Exception:
                    pass
                self.log(f"lease lost — mailbox taken over by worker {who or '?'}; "
                         "stopping local work")
                if self.hub is not None:
                    self.hub.add_history("Mailbox taken over", "WARN", self.spec.label,
                                         f"worker {who or '?'} claimed it after our "
                                         "heartbeats went silent — stopping here")
                    for cli in self.hub.clients_of(self.spec.label):
                        cli.abort(LeaseLost(f"taken over by worker {who or '?'}"))
                return
        except asyncio.CancelledError:
            pass

    # ---------------------------------------------------------------------

    async def _attempt(self) -> str:
        spec, cfg, mb = self.spec, self.cfg, self.mb
        mb.set(op="CONNECT", error="")
        base = Pair(spec, cfg, mb, self.log, hub=self.hub)
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
                p = Pair(spec, cfg, mb, self.log, hub=self.hub)
                extra_pairs.append(p)
                workers.append(self._extra_worker(p, queue))
            if grants:
                self.log(f"parallel folders: {1 + grants} connection pair(s)")
            try:
                await asyncio.gather(*workers)
            finally:
                # On stop, abort (no LOGOUT wait) so shutdown is fast; on a
                # clean finish, log out politely.
                graceful = not self.stop_event.is_set()
                for _ in range(grants):
                    self.extra_slots.release()
                for p in extra_pairs:
                    await p.close(graceful=graceful)
        finally:
            await base.close(graceful=not self.stop_event.is_set())

        if self.stop_event.is_set() and queue:
            return "CANCELLED"
        snap = mb.snap()
        if self.failed_folders:
            any_progress = snap["msgs_done"] > 0
            return "PARTIAL" if any_progress else "FAILED"
        if snap["skipped"] > 0 or snap.get("failed", 0) > 0:
            # every healthy message migrated; a handful could not be —
            # recorded in the Failed Message Registry for later retry
            return "WARNINGS"
        return "SUCCESS"

    async def _extra_worker(self, pair: Pair, queue: deque):
        try:
            await pair.open()
        except (MFError, OSError) as e:
            self.log(f"extra connection pair unavailable: {reason_of(e)}")
            return
        await self._folder_worker(Ctx(self, pair), queue, base_pair=False)

    async def _folder_worker(self, ctx: Ctx, queue: deque, base_pair: bool):
        while queue and not self.stop_event.is_set() and not self.lease_lost.is_set():
            if self.hub is not None and self.hub.paused:
                await asyncio.sleep(0.25)
                continue
            plan = queue.popleft()
            if plan.src_display not in self._started_folders:
                self._started_folders.add(plan.src_display)
                with self.mb._lock:
                    self.mb.folder_index += 1     # once per folder, ever
            try:
                await self._sync_folder_with_reconnect(ctx, plan)
            except StopRun:
                queue.appendleft(plan)
                if base_pair:
                    raise
                return

    async def _sync_folder_with_reconnect(self, ctx: Ctx, plan):
        rc = 0
        kicks = 0
        while True:
            try:
                outcome: FolderOutcome = await FolderSyncer(ctx, plan).run()
                if not outcome.ok:
                    self.failed_folders.append(plan.src_display)
                    self.first_error = self.first_error or outcome.error
                elif self.hub is not None and (outcome.copied or outcome.adopted
                                               or outcome.failed):
                    self.hub.add_history(
                        "Folder migrated", "OK" if not outcome.failed else "WARN",
                        self.spec.label,
                        f"{plan.src_display}: {outcome.copied:,} new · "
                        f"{outcome.adopted:,} adopted · {outcome.skipped:,} skipped"
                        + (f" · {outcome.failed:,} failed (registry)"
                           if outcome.failed else ""))
                return
            except StopRun:
                raise
            except AuthFailed:
                raise
            except (QuotaExceeded,) as e:
                raise
            except (ConnectionLost, ProtocolError, TransientError, OSError,
                    CommandFailed) as e:
                if self.lease_lost.is_set():
                    raise LeaseLost("taken over by another worker") from e
                stale_kick = isinstance(e, StaleKick)
                if stale_kick and self.hub is not None \
                        and self.hub.is_stale_failed(self.spec.label):
                    raise StaleFailed(
                        f"recovery exhausted after "
                        f"{self.hub.stale_attempts(self.spec.label)} attempt(s)") from e
                reason = reason_of(e)
                self._note_error(f"[{plan.src_display}] {reason}")
                rec = self.poison.get(plan.src_display) or {}
                isolating = bool(rec.get("ladder"))
                if isolating and rec.get("iso_losses", 0) <= 60:
                    pass        # isolation losses are the ladder doing its job
                elif not stale_kick:
                    rc += 1
                    if rc > max(1, self.cfg.reconnect_attempts):
                        self.log(f"[{plan.src_display}] giving up after "
                                 f"{rc - 1} reconnect(s): {reason}")
                        self.failed_folders.append(plan.src_display)
                        self.first_error = self.first_error or reason
                        return
                else:
                    kicks += 1
                    if kicks > 50:                     # paranoia bound
                        self.failed_folders.append(plan.src_display)
                        self.first_error = self.first_error or reason
                        return
                delay = 1.0 if (stale_kick or isolating) else backoff_delay(2.0, rc, cap=60.0)
                self.log(f"[{plan.src_display}] "
                         + ("stale recovery — reconnecting" if stale_kick
                            else "recovery isolation — reconnecting" if isolating
                            else f"connection trouble ({reason}) — "
                                 f"reconnecting in {delay:.0f}s"))
                with self.mb._lock:
                    self.mb.src.reconnects += 1
                if self.hub is not None and not isolating:
                    self.hub.add_history(
                        "Connection reconnect", "WARN", self.spec.label,
                        f"{plan.src_display}: "
                        + ("stale recovery" if stale_kick else f"attempt {rc} — {reason}"))
                self.mb.set(op=f"RECONNECT in {int(delay)}s", detail=reason[:120])
                await ctx.pair.close(graceful=False)
                if await self._sleep_interruptible(delay):
                    raise StopRun()
                self.mb.set(op="RECONNECT")
                await ctx.pair.open()
