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
    failed: int = 0            # permanently failed (Failed Message Registry)
    error: str = ""


def _hdr_field(header: bytes, name: str) -> str:
    """Extract one header field (unfolded, best effort) for the registry."""
    want = name.lower().encode()
    out: List[bytes] = []
    taking = False
    for line in (header or b"").split(b"\r\n"):
        if line[:1] in (b" ", b"\t"):
            if taking:
                out.append(line.strip())
            continue
        taking = line.lower().startswith(want + b":")
        if taking:
            out.append(line.split(b":", 1)[1].strip())
    return b" ".join(out).decode("utf-8", "replace")[:300]


def classify_failure(reason: str) -> str:
    low = (reason or "").lower()
    if "appendlimit" in low or "oversize" in low or "too large" in low or "exceeds" in low:
        return "OVERSIZE"
    if "timed out" in low or "timeout" in low:
        return "TIMEOUT"
    if "eof" in low or "reset" in low or "dropped" in low or "closed" in low \
            or "connection" in low:
        return "CONNECTION_RESET"
    if "parse" in low or "malformed" in low or "invalid" in low or "mime" in low \
            or "encoding" in low:
        return "MALFORMED_MIME"
    if "append" in low and ("no " in low or ": no" in low or "failed" in low):
        return "APPEND_NO"
    return "UNKNOWN"


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
        # Failure-isolation state lives on the RUNNER so it survives folder
        # reconnect re-entries — this is what breaks the poison-batch loop.
        self.rec = ctx.runner.poison_rec(plan.src_display)
        self._registry: Dict[int, str] = {}
        self._copied_uids: List[int] = []
        self._last_append_uid: Optional[int] = None
        self._loss_phase = ""      # "fetch" (source) | "append" (destination)

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

    async def _refresh_totals(self):
        """Resync mailbox-level counters from the State Database — the
        authoritative source. Keeps reconnect re-entries from double-counting."""
        t = await self.db.mailbox_totals(self.ctx.mid)
        self.mb.set(msgs_total=t["msgs_total"], bytes_total=t["bytes_total"],
                    msgs_done=t["msgs_done"], bytes_done=t["bytes_done"])

    async def _pause_gate(self):
        hub = getattr(self.ctx, "hub", None)
        if hub is None or not hub.paused:
            return
        self._op("PAUSED")
        while hub.paused and not self.ctx.stop_event.is_set():
            await asyncio.sleep(0.25)

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
        self.ctx.set_existing(self.plan.src_display, dst_msgs)
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
            await self._pause_gate()
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

        # ---- totals into live stats (DB-authoritative: reconnect-safe) ----
        counts = await db.folder_counts(self.fid)
        n_all = sum(c for c, _ in counts.values())
        b_all = sum(b for _, b in counts.values())
        await self._refresh_totals()

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
        self._registry = await db.failed_registry_uids(self.ctx.mid, plan.src_display)
        n_reg_skip = (await db.folder_counts(self.fid)).get("failed", (0, 0))[0]
        if n_reg_skip and self.cfg.skip_known_failed:
            self._log(f"skipping {n_reg_skip} previously failed message(s) — "
                      "recorded in the Failed Message Registry "
                      "(mailferry retry-failed re-queues them)")
        to_copy = await db.rows_by_state(self.fid, "planned")
        limit = _append_limit(self.dst)
        if limit:
            oversize = [r for r in to_copy if (r["size"] or 0) > limit]
            if oversize:
                for r in oversize:
                    await self._permanent_fail(
                        r, "OVERSIZE",
                        f"exceeds destination APPENDLIMIT {limit}", out, quiet=True)
                self._log(f"{len(oversize)} message(s) over APPENDLIMIT {limit} — "
                          "recorded in the Failed Message Registry")
                to_copy = [r for r in to_copy if (r["size"] or 0) <= limit]

        # supervisor hand-off: a stalled transfer that reconnects did not fix
        # enters Recovery Mode targeting the current front of the queue
        hub = getattr(self.ctx, "hub", None)
        label = self.ctx.runner.spec.label
        if (hub is not None and label in hub.recovery_hint
                and self.cfg.isolate_failed and to_copy and not self.rec["ladder"]):
            hub.recovery_hint.discard(label)
            window = [r["src_uid"] for r in to_copy[: max(1, self.cfg.append_window)]]
            self.rec["ladder"] = [{"uids": window, "tries": 0}]
            self._announce_recovery(len(window))

        try:
            if self.rec["ladder"] and self.cfg.isolate_failed:
                await self._isolation_phase(out)
                to_copy = await db.rows_by_state(self.fid, "planned")
            failures: List[Tuple[dict, str]] = []
            if to_copy:
                self._log(f"migrating {len(to_copy)} message(s), "
                          f"{fmt_bytes(sum(r['size'] or 0 for r in to_copy))}")
                rows = to_copy
                for attempt in range(1, self.cfg.msg_retries + 1):
                    self._check_stop()
                    failures = await self._transfer_pass(rows, strip_keywords=(attempt > 1))
                    out.copied += len(rows) - len(failures)
                    self._note_pass(rows, failures)
                    if not failures:
                        break
                    rows = [r for r, _ in failures]
                    if attempt < self.cfg.msg_retries:
                        self._log(f"{len(rows)} message(s) failed (pass {attempt}); retrying"
                                  + (" without keywords" if attempt == 1 else ""))
                        await asyncio.sleep(min(10, 2 * attempt))
                for r, err in failures:
                    # deterministic per-message rejections after all passes
                    await self._permanent_fail(r, classify_failure(err), err, out)
        except (ConnectionLost, ProtocolError) as e:
            await self._note_transport_loss(e)
            raise
        await self._mark_recoveries()

        # ---- optional flag re-sync (backup mode) ----
        if self.cfg.sync_flags:
            await self._sync_flags()

        # ---- finalize + count verification ----
        counts = await db.folder_counts(self.fid)
        n_done, b_done = counts.get("done", (0, 0))
        n_skip = counts.get("skipped", (0, 0))[0]
        n_fail = counts.get("failed", (0, 0))[0]
        status = "DONE" if not n_skip and not n_fail else "WARNINGS"
        # after a clean completion every destination message is accounted for
        # in the DB, so future incremental runs can skip the adopt scan.
        await db.update_folder(self.fid, status=status,
                               msgs_total=n_all, bytes_total=b_all,
                               msgs_done=n_done, bytes_done=b_done,
                               adopt_done=1 if status == "DONE" else (1 if adoption_needed else 0),
                               last_error="" if status == "DONE" else
                               f"{n_fail + n_skip} message(s) could not be migrated")
        try:
            st2 = await self.dst.status(plan.dst_wire)
            self._log(f"done: src={intervals_count(src_iv)} dst={st2.get('MESSAGES', '?')} "
                      f"synced={n_done} adopted={out.adopted} copied={out.copied} "
                      f"skipped={n_skip} failed={n_fail}")
        except MFError:
            pass
        out.ok = True          # message-level failures never fail the folder
        return out

    # ------------------------------------ failure isolation (recovery) --

    def _hist(self, event, status, details):
        hub = getattr(self.ctx, "hub", None)
        if hub is not None:
            hub.add_history(event, status, self.ctx.runner.spec.label,
                            f"{self.plan.src_display}: {details}"
                            if details else self.plan.src_display)

    def _announce_recovery(self, n):
        if not self.rec["announced"]:
            self.rec["announced"] = True
            self._log(f"Recovery Mode — investigating repeated failures "
                      f"({n} suspect message(s)); isolating problematic messages")
            self._hist("Entering Recovery Mode", "OK",
                       f"repeated failures — isolating {n} suspect message(s)")

    def _note_pass(self, rows, failures):
        """Bookkeeping after a completed transfer pass (no connection loss):
        deterministic (tagged NO) failures accumulate per UID across
        reconnect re-entries."""
        failed_uids = {r["src_uid"] for r, _ in failures}
        for r in rows:
            u = r["src_uid"]
            if u in failed_uids:
                self.rec["no_counts"][u] = self.rec["no_counts"].get(u, 0) + 1
            else:
                self.rec["no_counts"].pop(u, None)
                self.rec["loss_marks"].pop(u, None)
        if not failures:
            self.rec["losses"] = 0
            self.rec["suspects"] = set()

    async def _note_transport_loss(self, exc):
        """The connection died mid-transfer. Distinguish transport trouble
        (retry the same batch after reconnecting) from a poison message
        (repeated deaths on the same window -> Recovery Mode)."""
        if not self.cfg.isolate_failed:
            return
        if self._loss_phase != "append":
            return          # source-side trouble: transport, never a bad message
        rec = self.rec
        try:
            inflight = await self.db.rows_by_state(self.fid, "inflight")
        except Exception:
            inflight = []
        window = {r["src_uid"] for r in inflight}
        if self._last_append_uid is not None:
            window.add(self._last_append_uid)
            rec["loss_marks"][self._last_append_uid] = \
                rec["loss_marks"].get(self._last_append_uid, 0) + 1
        if not window:
            return
        inter = (rec["suspects"] & window) if rec["suspects"] else window
        if inter:
            rec["losses"] += 1
            rec["suspects"] = inter
        else:
            rec["losses"] = 1              # progress moved on — flaky transport
            rec["suspects"] = window
        # every suspect already fully rejected in an earlier pass? the
        # server is killing the connection on messages it already refuses —
        # that is a message problem, not a network problem: isolate now.
        all_rejected = all(rec["no_counts"].get(u, 0) >= 1 for u in inter) if inter else False
        if rec["ladder"]:
            return                          # isolation already in progress
        if rec["losses"] >= self.cfg.batch_attempts or \
                (all_rejected and rec["losses"] >= 1):
            suspects = sorted(rec["suspects"])
            prime = self._last_append_uid
            ladder = []
            if prime in rec["suspects"] and len(suspects) > 1:
                ladder.append({"uids": [prime], "tries": 0})
                ladder.append({"uids": [u for u in suspects if u != prime], "tries": 0})
            else:
                ladder.append({"uids": suspects, "tries": 0})
            rec["ladder"] = ladder
            self._announce_recovery(len(suspects))
            self._hist("Batch isolation", "OK",
                       f"analysing failed batch ({len(suspects)} message(s))")

    async def _isolation_phase(self, out: FolderOutcome):
        """Work through the isolation ladder: batches get cfg.batch_attempts
        tries each; a batch that keeps killing the connection is split in
        half (poison hints jump straight to singles); a single that still
        fails is recorded permanently and the migration moves on."""
        rec = self.rec
        db = self.db
        while rec["ladder"]:
            self._check_stop()
            entry = rec["ladder"][0]
            uids = [u for u in entry["uids"]]
            want = set(uids)
            pool = {r["src_uid"]: r for r in await db.rows_by_state(self.fid, "planned")
                    if r["src_uid"] in want}
            for r in await db.rows_by_state(self.fid, "inflight"):
                if r["src_uid"] in want:          # NO-failed rows keep their intent row
                    pool.setdefault(r["src_uid"], r)
            rows = [pool[u] for u in uids if u in pool]
            if not rows:
                rec["ladder"].pop(0)
                continue
            self._op(f"RECOVERY isolate {len(rows)} msg(s)")
            try:
                failures = await self._transfer_pass(rows, strip_keywords=True)
            except (ConnectionLost, ProtocolError):
                rec["iso_losses"] += 1
                if self._loss_phase != "append":
                    raise       # source/transport loss: reconnect, try again —
                                # never counts as evidence against a message
                entry["tries"] += 1
                for r in rows:
                    rec["loss_marks"][r["src_uid"]] = \
                        rec["loss_marks"].get(r["src_uid"], 0) + 1
                if len(rows) == 1:
                    if entry["tries"] >= self.cfg.batch_attempts:
                        await self._permanent_fail(
                            rows[0], "CONNECTION_RESET",
                            "server drops the connection on this message "
                            f"({entry['tries']} attempts)", out)
                        rec["ladder"].pop(0)
                elif entry["tries"] >= self.cfg.batch_attempts:
                    half = max(1, len(uids) // 2)
                    rec["ladder"].pop(0)
                    rec["ladder"].insert(0, {"uids": uids[half:], "tries": 0})
                    rec["ladder"].insert(0, {"uids": uids[:half], "tries": 0})
                    self._hist("Batch isolation", "OK",
                               f"reduced batch to {half} message(s)")
                    self._log(f"isolation: splitting batch of {len(uids)} "
                              f"into {half} + {len(uids) - half}")
                else:
                    # a poison hint from the append that killed the line
                    # jumps straight to a single-message probe
                    pu = self._last_append_uid
                    if pu in set(uids) and len(uids) > 1:
                        rest = [u for u in uids if u != pu]
                        rec["ladder"].pop(0)
                        rec["ladder"].insert(0, {"uids": rest, "tries": entry["tries"]})
                        rec["ladder"].insert(0, {"uids": [pu], "tries": 0})
                        self._hist("Batch isolation", "OK",
                                   f"suspect UID {pu} probed individually")
                raise                     # reconnect; the ladder is persistent
            # pass completed without a connection loss
            self._note_pass(rows, failures)
            ok_n = len(rows) - len(failures)
            out.copied += ok_n
            rec["ladder"].pop(0)
            for r, err in failures:
                u = r["src_uid"]
                if len(rows) == 1 or self.rec["no_counts"].get(u, 0) >= self.cfg.msg_retries:
                    await self._permanent_fail(r, classify_failure(err), err, out)
                else:
                    rec["ladder"].append({"uids": [u], "tries": 0})
        rec["losses"] = 0
        rec["suspects"] = set()
        if rec["announced"]:
            rec["announced"] = False
            self._log("Recovery Mode complete — continuing migration")
            self._hist("Migration resumed", "OK",
                       f"continuing remaining messages "
                       f"({out.failed} failed message(s) recorded)")

    async def _permanent_fail(self, row: dict, ftype: str, reason: str,
                              out: FolderOutcome, quiet: bool = False):
        """Record one message in the Failed Message Registry and move on.
        The mailbox migration NEVER stops for a bad message."""
        uid = row["src_uid"]
        meta = {"message_id": "", "subject": "", "sender": "", "date": row.get("internaldate") or ""}
        try:
            got = await self.src.uid_fetch_meta(str(uid), True)
            if got:
                h = got[0].get("header") or b""
                meta["message_id"] = _hdr_field(h, "Message-ID")
                meta["subject"] = _hdr_field(h, "Subject")
                meta["sender"] = _hdr_field(h, "From")
                meta["date"] = _hdr_field(h, "Date") or meta["date"]
        except MFError:
            pass
        await self.db.record_failed(
            self.ctx.mid, self.plan.src_display, uid,
            message_id=meta["message_id"], subject=meta["subject"],
            sender=meta["sender"], date=meta["date"], size=row.get("size") or 0,
            ftype=ftype, reason=reason)
        await self.db.mark_state(self.fid, [uid], "failed", reason[:200])
        self.mb.add(failed_msgs=1)
        out.failed += 1
        self.rec["no_counts"].pop(uid, None)
        self.rec["loss_marks"].pop(uid, None)
        if not quiet:
            self._log("Recovery Mode — message recorded in the Failed Message Registry:\n"
                      f"    Folder: {self.plan.src_display}\n"
                      f"    UID: {uid}\n"
                      f"    Message-ID: {meta['message_id'] or '-'}\n"
                      f"    Subject: {meta['subject'] or '-'}\n"
                      f"    From: {meta['sender'] or '-'}\n"
                      f"    Size: {fmt_bytes(row.get('size') or 0)}\n"
                      f"    Failure: {ftype} — {reason[:160]}\n"
                      "    Continuing mailbox migration...")
            self._hist("Failed message isolated", "WARN",
                       f"UID {uid} · {ftype}"
                       + (f" · {meta['subject'][:40]}" if meta["subject"] else ""))
            self._hist("Message skipped", "WARN",
                       "continuing migration (recorded in the Failed Message Registry)")
        hub = getattr(self.ctx, "hub", None)
        if hub is not None:
            hub.note_error(self.ctx.runner.spec.label,
                           f"[{self.plan.src_display}] UID {uid} failed permanently: "
                           f"{ftype} — {reason[:120]}")

    async def _mark_recoveries(self):
        """A previously failed message that has now been copied is RECOVERED."""
        if not self._registry or not self._copied_uids:
            return
        for uid in self._copied_uids:
            st = self._registry.get(uid)
            if st in ("FAILED", "RETRY_PENDING", "RETRYING"):
                await self.db.mark_recovered(self.ctx.mid, self.plan.src_display, uid)
                self._log(f"previously failed message uid {uid} successfully migrated — "
                          "status updated to RECOVERED")
                self._hist("Failed message recovered", "OK",
                           f"UID {uid} migrated on retry — status RECOVERED")

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
                self._copied_uids.append(row["src_uid"])
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

        def abandon_remaining():
            """Consume abandoned futures/queues so nothing logs noise and the
            reader can't wedge on a full body queue."""
            for _, abh in fetch_q:
                if abh.pending is not None:
                    ImapClient._mark_retrieved(abh.pending.fut)
                asyncio.get_event_loop().create_task(_drain_body(abh))
            for _, _, apend, _ in pend:
                ImapClient._mark_retrieved(apend.fut)

        start_more()
        stopping = False
        try:
            while fetch_q:
                if newly_windowed:
                    # intent rows BEFORE any of their appends can start
                    await db.mark_state(self.fid, newly_windowed, "inflight")
                    newly_windowed = []
                await self._pause_gate()
                if self.ctx.stop_event.is_set():
                    stopping = True
                    break
                row, bh = fetch_q.popleft()
                start_more()
                size_known = row["size"] or 0
                self._loss_phase = "fetch"     # a loss here blames the source
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
                self._last_append_uid = row["src_uid"]      # poison attribution
                self._loss_phase = "append"    # a loss here blames the message
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
            abandon_remaining()
            try:
                await flush(force=True)
            except Exception:
                pass
            raise
        if stopping:
            abandon_remaining()
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


async def _drain_body(bh):
    """Discard an abandoned body stream so the connection reader never wedges."""
    try:
        async for _ in bh.chunks():
            pass
    except Exception:
        pass
