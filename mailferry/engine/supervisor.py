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

Stale-sync detection with automatic self-healing.

The IMAP client's inactivity watchdog (--timeout) already catches a totally
silent socket. This supervisor sits one layer above and watches *meaningful
progress* per mailbox: messages committed, bytes delivered, folders
advancing, or a meaningful amount of wire traffic. A mailbox that is
RUNNING but has delivered nothing for --stale-timeout — through reconnect
cycles, keepalive noise or a wedged transfer — is verified stale and
recovered automatically:

  1. detect   — no hard progress and no meaningful wire traffic for
                stale_timeout while connections are open (a mailbox in a
                deliberate retry backoff is excluded — that is not a stall)
  2. recover  — force-close the mailbox's connections (StaleKick); the
                runner reconnects and resumes from the last confirmed
                checkpoint in the State Database (never duplicates: every
                message is committed per-UID before it counts)
  3. verify   — hard progress within --recovery-interval closes the episode
                as recovered; otherwise another attempt, up to
                --recovery-retries
  4. escalate — attempts exhausted: the mailbox is marked STALE, the
                operator is notified (Errors panel, History, session log)
                and manual actions remain available (r retry / rerun)

Everything is observational until a kick is due, so the supervisor can
never slow migration down.
"""
from __future__ import annotations

import asyncio
import time

from ..util import fmt_dhms

HARD_IO_BYTES = 64 * 1024      # wire traffic that counts as real progress


def _hard_sig(mb) -> tuple:
    """Progress signals that mean actual migration work is landing."""
    return (mb.msgs_done, mb.appended, mb.adopted, mb.dup_skipped, mb.skipped,
            mb.bytes_done, mb.folder_index, mb.status, mb.attempt)


def _io_total(mb) -> int:
    return (mb.src.rx_bytes + mb.src.tx_bytes
            + mb.dst.rx_bytes + mb.dst.tx_bytes)


class _Watch:
    __slots__ = ("sig", "io_mark", "last_progress", "attempts", "next_kick",
                 "episode_started", "episode", "recovery_tried")

    def __init__(self, sig, io, now):
        self.sig = sig
        self.io_mark = io
        self.last_progress = now
        self.attempts = 0
        self.next_kick = 0.0
        self.episode_started = 0.0
        self.episode = False
        self.recovery_tried = False


async def stale_supervisor(cfg, stats, hub, session, stop_event):
    """Runs beside the scheduler for the whole migration."""
    if hub is None or cfg.stale_timeout <= 0:
        return
    tick = min(2.0, max(0.5, cfg.stale_timeout / 10.0))
    spacing = max(1.0, float(cfg.recovery_interval))
    retries = max(1, int(cfg.recovery_retries))
    watches: dict = {}

    while True:
        try:
            await asyncio.wait_for(stop_event.wait(), timeout=tick)
            return                                    # graceful stop
        except asyncio.TimeoutError:
            pass
        now = time.time()
        if hub.paused or hub.shutdown_active:
            for w in watches.values():                # paused time is not stalled time
                w.last_progress = now
                if w.episode:
                    w.next_kick = now + spacing
            continue

        with stats.lock:
            mbs = list(stats.mailboxes.values())
        seen = set()
        for mb in mbs:
            label = mb.label
            seen.add(label)
            if mb.status != "RUNNING":
                # terminal, queued or deliberate retry backoff — not a stall.
                # (An episode that ends because the mailbox *finished* is
                # credited by the runner itself — see MailboxRunner.run —
                # so nothing is lost if this task is cancelled first.)
                w = watches.pop(label, None)
                if w is not None and w.episode:
                    mb.set(recovering=0)
                continue

            sig = _hard_sig(mb)
            io = _io_total(mb)
            w = watches.get(label)
            if w is None:
                watches[label] = _Watch(sig, io, now)
                continue

            progressed = sig != w.sig or (io - w.io_mark) >= HARD_IO_BYTES
            if progressed:
                w.sig = sig
                w.io_mark = io
                w.last_progress = now
                if w.episode:                          # recovery succeeded
                    dur = fmt_dhms(now - w.episode_started)
                    stats.bump("stalls_recovered")
                    hub.stale_failed.discard(label)
                    hub.stale_attempts_by.pop(label, None)
                    mb.set(recovering=0)
                    session.log(f"transfer recovered: {label} — resumed after "
                                f"reconnect {w.attempts} ({dur})")
                    hub.log_event("INFO", label,
                                  f"transfer recovered — resumed (reconnect {w.attempts})")
                    hub.add_history("Transfer recovered", "OK", label,
                                    f"transfer resumed after reconnect {w.attempts} ({dur})")
                    w.episode = False
                    w.attempts = 0
                continue

            frozen = now - w.last_progress
            if not w.episode:
                if frozen < cfg.stale_timeout:
                    continue
                # verification: RUNNING + zero hard progress + negligible
                # wire traffic for the whole window. A slow-but-moving
                # server resets the clock via the io threshold above, so
                # reaching here means genuinely stalled — but only act if
                # there are connections to recover (a mailbox waiting in
                # backoff has none, and that wait is deliberate).
                if not hub.clients_of(label):
                    w.last_progress = now
                    continue
                w.episode = True
                w.episode_started = w.last_progress
                w.attempts = 0
                w.next_kick = now
                stats.bump("stalls_detected")
                where = f"folder {mb.current_folder or '-'} · op {mb.op or '-'}"
                session.log(f"stalled transfer detected: {label} — no progress for "
                            f"{fmt_dhms(frozen)} ({where})")
                hub.log_event("WARN", label,
                              f"stalled transfer — no progress for {fmt_dhms(frozen)} ({where})")
                hub.add_history("Stalled transfer detected", "WARN", label,
                                f"no progress for {fmt_dhms(frozen)} — {where}")

            if now < w.next_kick:
                continue
            if w.attempts >= retries:
                if cfg.isolate_failed and not w.recovery_tried:
                    # reconnecting alone did not restart the transfer: switch
                    # strategy — Recovery Mode isolates problem messages while
                    # a fresh round of connection recovery runs underneath
                    w.recovery_tried = True
                    w.attempts = 0
                    hub.recovery_hint.add(label)
                    session.log(f"Recovery Mode: {label} — repeated connection "
                                "failures; isolating problematic messages")
                    hub.log_event("INFO", label,
                                  "Entering Recovery Mode — isolating problematic messages")
                    hub.add_history("Entering Recovery Mode", "OK", label,
                                    "repeated failures — isolating problematic messages")
                    hub.kick_stale(label, "recovery mode — isolate problematic messages")
                    w.next_kick = now + spacing
                    continue
                # nothing left to try — escalate to the operator
                w.episode = False
                stats.bump("stalls_failed")
                hub.stale_failed.add(label)
                hub.stale_attempts_by[label] = w.attempts
                mb.set(recovering=0)
                msg = (f"recovery exhausted after {w.attempts} reconnect(s) and "
                       "message isolation — marked STALE; press r to retry, or rerun")
                session.log(f"RECOVERY EXHAUSTED: {label} — {msg}")
                hub.note_error(label, f"RECOVERY EXHAUSTED — {msg}")
                hub.add_history("Recovery exhausted", "FAIL", label, msg)
                hub.kick_stale(label, "stale — recovery exhausted")
                continue
            w.attempts += 1
            hub.stale_attempts_by[label] = w.attempts
            mb.set(recovering=w.attempts,
                   op=f"RECOVERY #{w.attempts}/{retries}",
                   detail=f"stalled — reconnecting (no progress for "
                          f"{fmt_dhms(now - w.episode_started)})")
            session.log(f"connection recovery: {label} reconnect {w.attempts}/{retries} — "
                        "resume from the last checkpoint")
            hub.log_event("INFO", label,
                          f"connection recovery — reconnect {w.attempts}/{retries}")
            hub.add_history("Connection recovery", "OK", label,
                            f"reconnect {w.attempts}/{retries} — resume from last checkpoint")
            hub.kick_stale(label, f"stalled — connection recovery {w.attempts}/{retries}")
            w.next_kick = now + spacing

        for label in [k for k in watches if k not in seen]:
            del watches[label]
