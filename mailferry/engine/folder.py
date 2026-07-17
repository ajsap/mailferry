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

Per-folder sync: scan -> reconcile -> adopt -> stream transfer -> verify.

Idempotency invariants (design §10/§10A/§13):
- no APPEND without an `inflight` intent row
- no `done` without APPENDUID or a positive destination probe
- UIDVALIDITY churn resets rows to re-verification, never blind re-copy
- losing the DB re-enters adoption: duplicates are impossible by design
"""
from __future__ import annotations

import asyncio
from collections import deque
from dataclasses import dataclass
from typing import Dict, List, Optional, Tuple

from ..errors import (CommandFailed, ConnectionLost, MFError, PermanentError,
                      ProtocolError, StopRun)
from ..imap.client import ImapClient
from ..util import (HeaderSniffer, fingerprint_from_headers, fmt_bytes,
                    intervals_count, intervals_diff, monotonic, set_strings)

SYSTEM_FLAGS = {"\\SEEN", "\\ANSWERED", "\\FLAGGED", "\\DELETED", "\\DRAFT"}


@dataclass
class FolderOutcome:
    ok: bool = True
    copied: int = 0
    adopted: int = 0
    skipped: int = 0
    error: str = ""


def _clean_flags(flag_str: str, strip_keywords: bool) -> str:
    out = []
    for f in (flag_str or "").split():
        if f.upper() == "\\RECENT":
            continue
        if strip_keywords and not f.startswith("\\"):
            continue
        if strip_keywords and f.upper() not in SYSTEM_FLAGS:
            continue
        out.append(f)
    return " ".join(out)


def _side_add(mb, side, field, n=1):
    with mb._lock:
        setattr(side, field, getattr(side, field) + n)


def _append_limit(dst: ImapClient) -> Optional[int]:
    for c in dst.caps:
        if c.startswith("APPENDLIMIT="):
            try:
                return int(c.split("=", 1)[1])
            except ValueError:
                return None
    return None


class FolderSyncer:
    def __init__(self, ctx, plan):
        self.ctx = ctx
        self.plan = plan
        self.cfg = ctx.cfg
        self.db = ctx.db
        self.mb = ctx.mb
        self.src: ImapClient = ctx.src
        self.dst: ImapClient = ctx.dst
        self.fid = 0
        self.dst_selected: Optional[bool] = None

    def _log(self, msg: str):
        self.ctx.log(f"[{self.plan.src_display}] {msg}")

    def _check_stop(self):
        if self.ctx.stop_event.is_set():
            raise StopRun()

    def _op(self, verb: str):
        self.mb.set(op=verb, current_folder=self.plan.src_display)

    async def _select_dst(self, readonly=True):
        if self.dst_selected != readonly:
            await self.dst.select(self.plan.dst_wire, readonly=readonly)
            self.dst_selected = readonly

    # ------------------------------------------------------------- main --

    async def run(self) -> FolderOutcome:
        out = FolderOutcome()
        plan, db, mb = self.plan, self.db, self.mb
        self._check_stop()
        self._op("SELECT src")
        row = await db.folder_row(self.ctx.mid, plan.src_display, plan.dst_display)
        self.fid = row["id"]
        sel = await self.src.select(plan.src_wire, readonly=True)
        uv_src = sel.uidvalidity or 0
        if row["uv_src"] and uv_src and row["uv_src"] != uv_src:
            self._log(f"source UIDVALIDITY changed {row['uv_src']} -> {uv_src}: replanning folder")
            await db.reset_folder_messages(self.fid, keep_done_as_planned=False)
            row = await db.folder_row(self.ctx.mid, plan.src_display, plan.dst_display)

        # destination folder: STATUS (create when missing)
        self._op("STATUS dst")
        try:
            st = await self.dst.status(plan.dst_wire)
        except CommandFailed:
            self._op("CREATE dst")
            self._log(f"creating destination folder {plan.dst_display}")
            await self.dst.create(plan.dst_wire)
            if self.cfg.subscribe:
                await self.dst.subscribe(plan.dst_wire)
            st = await self.dst.status(plan.dst_wire)
        uv_dst = st.get("UIDVALIDITY", 0) or 0
        dst_msgs = st.get("MESSAGES", 0) or 0
        dst_uidnext = st.get("UIDNEXT", 0) or 0
        _side_add(mb, mb.dst, "existing_msgs", dst_msgs)
        if row["uv_dst"] and uv_dst and row["uv_dst"] != uv_dst:
            self._log(f"destination UIDVALIDITY changed {row['uv_dst']} -> {uv_dst}: "
                      "re-verifying presence via adoption (no blind re-copy)")
            await db.reset_folder_messages(self.fid, keep_done_as_planned=True)
            row = await db.folder_row(self.ctx.mid, plan.src_display, plan.dst_display)
        prev_bracket = row["last_uidnext_dst"] or 0

        # ---- crash reconciliation (inflight rows) ----
        inflight = await db.rows_by_state(self.fid, "inflight")
        if inflight:
            await self._reconcile_inflight(inflight, prev_bracket, out)

        # ---- source scan (new/unknown UIDs only) ----
        self._op("SCAN src")
        src_iv = await self.src.uid_search_all()
        known_iv = await db.known_uid_intervals(self.fid)
        missing = intervals_diff(src_iv, known_iv)
        total_missing = intervals_count(missing)
        adoption_needed = (dst_msgs > 0 and not self.cfg.no_dedup_scan
                           and (not row["adopt_done"] or self.cfg.rescan_dest))
        scanned = 0
        for ss in set_strings(missing):
            self._check_stop()
            metas = await self.src.uid_fetch_meta(ss, adoption_needed)
            rows = []
            for m in metas:
                fp = fingerprint_from_headers(m["header"], m["size"]) if adoption_needed else None
                rows.append((m["uid"], m["size"], " ".join(m["flags"]), m["date"], fp))
            await db.insert_planned(self.fid, rows)
            scanned += len(metas)
            self._op(f"SCAN src {scanned:,}/{total_missing:,}")
        await db.update_folder(self.fid, uv_src=uv_src,
                               last_uidnext_src=sel.uidnext or 0,
                               highestmodseq=sel.highestmodseq or 0)

        # fingerprints for older planned rows (crash before adoption etc.)
        if adoption_needed:
            planned_rows = await db.rows_by_state(self.fid, "planned")
            nofp = [r["src_uid"] for r in planned_rows if not r["fp"]]
            if nofp:
                from ..util import to_intervals
                got = 0
                for ss in set_strings(to_intervals(nofp)):
                    metas = await self.src.uid_fetch_meta(ss, True)
                    pairs = [(m["uid"], fingerprint_from_headers(m["header"], m["size"]))
                             for m in metas]
                    await db.set_fp(self.fid, pairs)
                    got += len(pairs)
                    self._op(f"SCAN src fp {got:,}/{len(nofp):,}")

        # ---- totals into live stats ----
        counts = await db.folder_counts(self.fid)
        n_all = sum(c for c, _ in counts.values())
        b_all = sum(b for _, b in counts.values())
        n_done, b_done = counts.get("done", (0, 0))
        mb.add(msgs_total=n_all, bytes_total=b_all, msgs_done=n_done, bytes_done=b_done)
        with mb._lock:
            mb.folder_index += 1

        # ---- adoption of pre-synced destination content ----
        if adoption_needed:
            planned_rows = await db.rows_by_state(self.fid, "planned")
            if planned_rows:
                adopted = await self._adopt(planned_rows, dst_msgs)
                out.adopted += adopted
            await db.update_folder(self.fid, adopt_done=1, uv_dst=uv_dst)
        else:
            await db.update_folder(self.fid, uv_dst=uv_dst)

        # record the append bracket for crash reconciliation
        await db.update_folder(self.fid, last_uidnext_dst=dst_uidnext or 0)

        # ---- transfer ----
        to_copy = await db.rows_by_state(self.fid, "planned")
        limit = _append_limit(self.dst)
        if limit:
            oversize = [r for r in to_copy if (r["size"] or 0) > limit]
            if oversize:
                uids = [r["src_uid"] for r in oversize]
                await db.mark_state(self.fid, uids, "skipped",
                                    f"exceeds destination APPENDLIMIT {limit}")
                self._log(f"skipped {len(uids)} message(s) over APPENDLIMIT {limit}")
                mb.add(skipped=len(uids))
                out.skipped += len(uids)
                to_copy = [r for r in to_copy if (r["size"] or 0) <= limit]

        failures: List[Tuple[dict, str]] = []
        if to_copy:
            self._log(f"migrating {len(to_copy)} message(s), "
                      f"{fmt_bytes(sum(r['size'] or 0 for r in to_copy))}")
            rows = to_copy
            for attempt in range(1, self.cfg.msg_retries + 1):
                self._check_stop()
                failures = await self._transfer_pass(rows, strip_keywords=(attempt > 1))
                out.copied += len(rows) - len(failures)
                if not failures:
                    break
                rows = [r for r, _ in failures]
                if attempt < self.cfg.msg_retries:
                    self._log(f"{len(rows)} message(s) failed (pass {attempt}); retrying"
                              + (" without keywords" if attempt == 1 else ""))
                    await asyncio.sleep(min(10, 2 * attempt))
            if failures:
                uids = [r["src_uid"] for r, _ in failures]
                reason = failures[0][1][:200]
                await db.mark_state(self.fid, uids, "skipped", reason)
                mb.add(skipped=len(uids))
                out.skipped += len(uids)
                self._log(f"gave up on {len(uids)} message(s): {reason}")

        # ---- optional flag re-sync (backup mode) ----
        if self.cfg.sync_flags:
            await self._sync_flags()

        # ---- finalize + count verification ----
        counts = await db.folder_counts(self.fid)
        n_done, b_done = counts.get("done", (0, 0))
        n_skip = counts.get("skipped", (0, 0))[0]
        status = "DONE" if not n_skip and not failures else "PARTIAL"
        # after a clean completion every destination message is accounted for
        # in the DB, so future incremental runs can skip the adopt scan.
        await db.update_folder(self.fid, status=status,
                               msgs_total=n_all, bytes_total=b_all,
                               msgs_done=n_done, bytes_done=b_done,
                               adopt_done=1 if status == "DONE" else (1 if adoption_needed else 0),
                               last_error="" if status == "DONE" else "some messages skipped")
        try:
            st2 = await self.dst.status(plan.dst_wire)
            self._log(f"done: src={intervals_count(src_iv)} dst={st2.get('MESSAGES', '?')} "
                      f"synced={n_done} adopted={out.adopted} copied={out.copied} skipped={n_skip}")
        except MFError:
            pass
        out.ok = status == "DONE"
        if not out.ok:
            out.error = "some messages skipped"
        return out

    # ------------------------------------------------------- reconcile --

    async def _reconcile_inflight(self, inflight: List[dict], prev_bracket: int, out: FolderOutcome):
        self._op(f"RECONCILE {len(inflight)}")
        self._log(f"reconciling {len(inflight)} in-flight message(s) from an interrupted run")
        # ensure fingerprints (from SOURCE) for all inflight rows
        nofp = [r["src_uid"] for r in inflight if not r["fp"]]
        if nofp:
            from ..util import to_intervals
            for ss in set_strings(to_intervals(nofp)):
                metas = await self.src.uid_fetch_meta(ss, True)
                pairs = [(m["uid"], fingerprint_from_headers(m["header"], m["size"])) for m in metas]
                await db_set_fp(self.db, self.fid, pairs, inflight)
        inflight = await self.db.rows_by_state(self.fid, "inflight")
        # scan destination tail (uids appended since the recorded bracket)
        await self._select_dst(readonly=True)
        dst_iv = await self.dst.uid_search_all()
        tail = [(max(lo, prev_bracket or 1), hi) for lo, hi in dst_iv if hi >= (prev_bracket or 1)]
        fpmap: Dict[str, deque] = {}
        for ss in set_strings(tail):
            metas = await self.dst.uid_fetch_meta(ss, True)
            for m in metas:
                fpmap.setdefault(fingerprint_from_headers(m["header"], m["size"]),
                                 deque()).append(m["uid"])
        done, back = [], []
        for r in inflight:
            dq = fpmap.get(r["fp"] or "")
            if dq:
                done.append((r["src_uid"], dq.popleft(), None))
            else:
                back.append(r["src_uid"])
        if done:
            await self.db.mark_done(self.fid, done, "adopted")
            self.mb.add(adopted=len(done), dup_skipped=len(done))
            out.adopted += len(done)
            self._log(f"reconcile: {len(done)} were already delivered (adopted), "
                      f"{len(back)} will be re-copied")
        if back:
            await self.db.mark_state(self.fid, back, "planned")

    # ----------------------------------------------------------- adopt --

    async def _adopt(self, planned_rows: List[dict], dst_msgs: int) -> int:
        self._op(f"ADOPT scan dst 0/{dst_msgs:,}")
        self._log(f"destination has {dst_msgs} pre-existing message(s): "
                  "fingerprint-matching to prevent duplicates")
        await self._select_dst(readonly=True)
        dst_iv = await self.dst.uid_search_all()
        fpmap: Dict[str, deque] = {}
        scanned = 0
        for ss in set_strings(dst_iv):
            self._check_stop()
            metas = await self.dst.uid_fetch_meta(ss, True)
            for m in metas:
                fpmap.setdefault(fingerprint_from_headers(m["header"], m["size"]),
                                 deque()).append(m["uid"])
            scanned += len(metas)
            self._op(f"ADOPT scan dst {scanned:,}/{dst_msgs:,}")
        triples = []
        adopted_bytes = 0
        for r in planned_rows:
            dq = fpmap.get(r["fp"] or "")
            if dq:
                triples.append((r["src_uid"], dq.popleft(), None))
                adopted_bytes += r["size"] or 0
        if triples:
            await self.db.mark_done(self.fid, triples, "adopted")
            self.mb.add(adopted=len(triples), dup_skipped=len(triples),
                        msgs_done=len(triples), bytes_done=adopted_bytes)
            self._log(f"adopted {len(triples)} message(s) already present on the Destination "
                      f"Server ({fmt_bytes(adopted_bytes)}) — not migrated again")
        return len(triples)

    # -------------------------------------------------------- transfer --

    async def _transfer_pass(self, rows: List[dict], strip_keywords: bool) -> List[Tuple[dict, str]]:
        cfg, db, mb = self.cfg, self.db, self.mb
        plan = self.plan
        failures: List[Tuple[dict, str]] = []
        pend: deque = deque()          # (row, fp, pending, size)
        done_batch: List[Tuple[int, Optional[int], Optional[str]]] = []
        done_meta: List[Tuple[int, int]] = []      # (uid, size) for stats on flush
        last_flush = monotonic()

        async def flush(force=False):
            nonlocal done_batch, done_meta, last_flush
            if done_batch and (force or len(done_batch) >= 200
                               or monotonic() - last_flush > 1.5):
                await db.mark_done(self.fid, done_batch, "copied")
                done_batch, done_meta = [], []
                last_flush = monotonic()

        async def settle_one():
            row, fp, pending, size = pend.popleft()
            try:
                res = await pending.fut
                duid = ImapClient.appenduid_of(res)
                done_batch.append((row["src_uid"], duid, fp))
                done_meta.append((row["src_uid"], size))
                mb.add(msgs_done=1, bytes_done=size, appended=1)
            except (ConnectionLost, ProtocolError):
                raise
            except (CommandFailed, PermanentError, MFError) as e:
                failures.append((row, str(e)))

        fetch_q: deque = deque()
        it = iter(rows)
        exhausted = False
        newly_windowed: List[int] = []

        def start_more():
            nonlocal exhausted
            while not exhausted and len(fetch_q) < cfg.fetch_window:
                try:
                    r = next(it)
                except StopIteration:
                    exhausted = True
                    return
                fetch_q.append((r, self.src.body_fetch(r["src_uid"])))
                newly_windowed.append(r["src_uid"])

        start_more()
        stopping = False
        try:
            while fetch_q:
                if newly_windowed:
                    # intent rows BEFORE any of their appends can start
                    await db.mark_state(self.fid, newly_windowed, "inflight")
                    newly_windowed = []
                if self.ctx.stop_event.is_set():
                    stopping = True
                    break
                row, bh = fetch_q.popleft()
                start_more()
                size_known = row["size"] or 0
                try:
                    size = await asyncio.wait_for(bh.wait_size(), timeout=cfg.timeout)
                except (ConnectionLost, ProtocolError):
                    raise
                except asyncio.TimeoutError:
                    raise ConnectionLost("timed out waiting for message body")
                except MFError as e:
                    failures.append((row, f"fetch failed: {e}"))
                    continue
                self._op(f"MIGRATE uid {row['src_uid']} ({fmt_bytes(size)})")
                mb.set(detail=f"uid {row['src_uid']} {fmt_bytes(size)} "
                              f"{'!= ' + fmt_bytes(size_known) if size_known and size_known != size else ''}".strip())
                flags = _clean_flags(row["flags"], strip_keywords)
                ap = await self.dst.append_begin(plan.dst_wire, flags, row["internaldate"], size)
                sniffer = HeaderSniffer()
                async for chunk in bh.chunks():
                    sniffer.feed(chunk)
                    await ap.write(chunk)
                pending = await ap.finish()
                fp = row["fp"] or sniffer.fingerprint(size)
                pend.append((row, fp, pending, size))
                while len(pend) >= cfg.append_window:
                    await settle_one()
                await flush()
            while pend:
                await settle_one()
            await flush(force=True)
        except BaseException:
            # flush what we know; inflight rows will reconcile on resume
            try:
                await flush(force=True)
            except Exception:
                pass
            raise
        if stopping:
            await flush(force=True)
            raise StopRun()
        return failures

    # ------------------------------------------------------ flags sync --

    async def _sync_flags(self):
        db, mb = self.db, self.mb
        done = await db.rows_by_state(self.fid, "done")
        with_dst = [r for r in done if r["dst_uid"]]
        if not with_dst:
            return
        self._op(f"FLAGS-SYNC 0/{len(with_dst):,}")
        from ..util import to_intervals
        current: Dict[int, str] = {}
        for ss in set_strings(to_intervals([r["src_uid"] for r in with_dst])):
            metas = await self.src.uid_fetch_meta(ss, False)
            for m in metas:
                current[m["uid"]] = " ".join(
                    f for f in m["flags"] if f.upper() != "\\RECENT")
        changed = []
        for r in with_dst:
            new = current.get(r["src_uid"])
            if new is not None and new != (r["flags"] or ""):
                changed.append((r, new))
        if not changed:
            return
        self._log(f"flag re-sync: {len(changed)} message(s) changed")
        await self._select_dst(readonly=False)
        n = 0
        for r, new in changed:
            self._check_stop()
            try:
                await self.dst.uid_store_flags(r["dst_uid"], _clean_flags(new, False))
                await db.update_flags(self.fid, [(r["src_uid"], new)])
            except (ConnectionLost, ProtocolError):
                raise
            except MFError as e:
                self._log(f"flag sync failed for uid {r['src_uid']}: {e}")
            n += 1
            if n % 25 == 0:
                self._op(f"FLAGS-SYNC {n:,}/{len(changed):,}")


async def db_set_fp(db, fid: int, pairs: List[Tuple[int, str]], only_rows: List[dict]):
    wanted = {r["src_uid"] for r in only_rows}
    await db.set_fp(fid, [(u, fp) for u, fp in pairs if u in wanted])
