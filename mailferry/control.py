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

The runtime control surface shared by the engine, the TUI and any future
remote front-end (e.g. `mailferry attach`). Views and key handlers call
these methods; migration-affecting actions are marshalled onto the event
loop so the engine is never touched from the UI threads.
"""
from __future__ import annotations

import time
from collections import deque
from typing import Dict

# Ordered graceful-shutdown phases shown in the shutdown dialog.
SHUTDOWN_PHASES = [
    ("scheduler", "Stopping the scheduler"),
    ("workers", "Waiting for active workers"),
    ("state", "Saving migration state"),
    ("logs", "Flushing logs"),
    ("conns", "Closing IMAP connections"),
    ("resources", "Releasing resources"),
    ("done", "Shutdown complete"),
]


class ControlHub:
    """Shared control + telemetry surface. All mutation is thread-safe or
    marshalled onto the event loop via call_soon()."""

    def __init__(self, cfg, stats, session):
        self.cfg = cfg
        self.stats = stats
        self.session = session
        self.loop = None                     # asyncio loop (set at startup)
        self.stop_event = None               # asyncio.Event (graceful stop)
        self.hard_abort = None               # callable (immediate stop)
        self.db = None
        self.paused = False
        self.errors: deque = deque(maxlen=200)      # (ts, mailbox, reason)
        self.events: deque = deque(maxlen=5000)     # (ts, sev, mailbox, message)
        self.specs_by_label: Dict[str, object] = {}
        self.active_labels: set = set()
        self.spawn = None                    # callable(spec) -> requeue a mailbox
        self.reload_csv = None               # callable() -> (added, known)
        self.rates = (0.0, 0.0)              # (bytes/s, msgs/s) fed by the renderer
        self.eta = None
        self.live_clients = set()            # open ImapClient handles (for fast stop)
        self.history: deque = deque(maxlen=2000)    # (ts, event, status, mailbox, details)
        self.stale_failed: set = set()       # labels whose auto-recovery exhausted
        self.stale_attempts_by: Dict[str, int] = {}
        self.recovery_hint: set = set()      # labels told to enter Recovery Mode
        self.failed_registry: list = []      # outstanding rows at run end (report)
        self.tui_attached = False            # set by the CLI when the TUI is live
        self.lock_prompt = None              # active stale-lock dialog state (dict)
        self.lock_decisions: Dict[str, str] = {}    # foreign owner -> reset|cancel
        self.worker_id = ""                  # this instance's cluster Worker ID
        self.cluster: list = []              # workers roster (cluster monitor cache)
        self.remote_watch: Dict[str, dict] = {}     # label -> {spec, mid, owner}
        # graceful-shutdown dialog state (read by the renderer)
        self.shutdown_active = False
        self.shutdown_started = 0.0
        self.shutdown_forced = False
        self.phase_state: Dict[str, str] = {k: "pending" for k, _ in SHUTDOWN_PHASES}

    def watch_remote(self, spec, mid: int, owner: str):
        """Track a mailbox processed by another worker: mirror its progress
        and reclaim it automatically if that worker goes offline."""
        self.remote_watch[spec.label] = {"spec": spec, "mid": mid, "owner": owner}

    # -- telemetry -----------------------------------------------------------
    def note_error(self, mailbox: str, reason: str):
        self.errors.append((time.time(), mailbox, reason))
        self.log_event("WARN", mailbox, reason)

    def log_event(self, sev: str, mailbox: str, message: str):
        self.events.append((time.time(), sev, mailbox or "-", message))

    def add_history(self, event: str, status: str = "OK", mailbox: str = "-",
                    details: str = ""):
        """Milestone feed for the History / Activity view (F5)."""
        self.history.append((time.time(), event, status, mailbox or "-", details))

    # -- loop marshalling ----------------------------------------------------
    def call_soon(self, fn):
        """Run fn on the event loop thread (safe from the input/render threads)."""
        if self.loop is not None:
            try:
                self.loop.call_soon_threadsafe(fn)
                return
            except RuntimeError:
                pass
        fn()

    # -- connection registry (fast, bounded shutdown) -----------------------
    def register_client(self, client):
        self.live_clients.add(client)

    def unregister_client(self, client):
        self.live_clients.discard(client)

    def abort_all_connections(self):
        """Abort every open IMAP connection immediately. Awaits stalled on a
        dead/slow socket unwind at once with ConnectionLost, so a graceful
        stop always completes promptly even when the network is hung. The
        per-message commit protocol keeps state consistent; the next run
        resumes cleanly."""
        n = 0
        for cli in list(self.live_clients):
            try:
                cli.abort()
                n += 1
            except Exception:
                pass
        self.live_clients.clear()
        if n:
            self.session.log(f"shutdown: closed {n} IMAP connection(s) to stop promptly")
        return n

    # -- stale detection & self-healing --------------------------------------
    def clients_of(self, label: str) -> list:
        return [cli for cli in self.live_clients
                if getattr(cli, "owner_label", None) == label]

    def kick_stale(self, label: str, reason: str) -> int:
        """Force-close a stalled mailbox's connections so its awaits unwind
        with StaleKick and the runner reconnects and resumes from the last
        confirmed checkpoint. Never consumes the retry budget."""
        from .errors import StaleKick
        n = 0
        for cli in self.clients_of(label):
            try:
                cli.abort(StaleKick(reason))
                n += 1
            except Exception:
                pass
        return n

    def is_stale_failed(self, label: str) -> bool:
        return label in self.stale_failed

    def stale_attempts(self, label: str) -> int:
        return self.stale_attempts_by.get(label, 0)

    # -- graceful shutdown dialog -------------------------------------------
    def begin_shutdown(self):
        if not self.shutdown_active:
            self.shutdown_active = True
            self.shutdown_started = time.time()
            self.set_phase("scheduler", "active")

    def set_phase(self, key: str, state: str):
        """Mark earlier phases done and the next pending phase active, so the
        dialog always shows forward motion."""
        if key not in self.phase_state:
            return
        keys = [k for k, _ in SHUTDOWN_PHASES]
        idx = keys.index(key)
        if state == "done":
            for k in keys[:idx + 1]:
                self.phase_state[k] = "done"
            nxt = idx + 1
            if nxt < len(keys) and keys[nxt] != "done":
                self.phase_state[keys[nxt]] = "active"
        else:
            for k in keys[:idx]:
                if self.phase_state[k] != "done":
                    self.phase_state[k] = "done"
            self.phase_state[key] = state

    # -- actions -------------------------------------------------------------
    def request_stop(self):
        self.stats.interrupted = True
        self.begin_shutdown()

        def _set():
            if self.stop_event is not None and not self.stop_event.is_set():
                self.stop_event.set()
        self.call_soon(_set)
        self.session.log("graceful stop requested")

    def request_abort(self):
        self.session.log("hard abort requested")
        self.shutdown_forced = True
        self.begin_shutdown()
        if self.hard_abort is not None:
            self.hard_abort()

    def toggle_pause(self) -> bool:
        self.paused = not self.paused
        self.log_event("INFO", "-", "migration paused" if self.paused else "migration resumed")
        return self.paused

    def retry_failed(self, label=None, all_failed=False):
        """Re-queue failed/partial mailboxes (marshalled onto the loop)."""
        def _do():
            if self.spawn is None:
                return
            rows = [m for m in self.stats.snapshot()["mailboxes"]
                    if m["status"] in ("FAILED", "PARTIAL", "STALE")]
            if not all_failed and label is not None:
                rows = [m for m in rows if m["label"] == label]
            for m in rows:
                spec = self.specs_by_label.get(m["label"])
                if spec is not None and m["label"] not in self.active_labels:
                    self.stale_failed.discard(m["label"])
                    self.stale_attempts_by.pop(m["label"], None)
                    self.spawn(spec)
                    self.log_event("INFO", m["label"], "re-queued for retry")
                    self.add_history("Mailbox re-queued", "OK", m["label"],
                                     "manual retry from the console")
        self.call_soon(_do)

    def do_reload(self):
        if self.reload_csv is not None:
            self.call_soon(self.reload_csv)

    # -- stale instance locks -------------------------------------------------
    # A crashed / force-killed instance leaves its lease behind. Leases are
    # heartbeat-refreshed every StateDB.HEARTBEAT seconds while an instance is
    # alive, so a heartbeat older than LOCK_LIVE_GRACE means the instance is
    # almost certainly dead. Takeover is verified: only offered/performed when
    # the heartbeat is old, auto-abandoned if the heartbeat advances, and the
    # actual reset is an atomic compare-and-swap in the State Database — a
    # live instance can never be dispossessed, even across racing machines.
    LOCK_LIVE_GRACE = 90.0          # 1.5x the 60s heartbeat
    LOCK_PROMPT_TIMEOUT = 90.0      # dialog auto-cancels after this

    async def resolve_lock_conflict(self, label: str, mid: int, other: str,
                                    age: float, db) -> str:
        """Called by a mailbox runner that hit a foreign lease younger than
        --lock-timeout. Returns 'reset' (verified stale — take over) or
        'cancel'. One decision per foreign instance is shared by every
        mailbox that instance had locked."""
        import asyncio
        prior = self.lock_decisions.get(other)
        if prior is not None:
            return prior
        if not self.tui_attached:
            verdict = await self._headless_lock_verdict(mid, other, age, db)
            self.lock_decisions[other] = verdict
            return verdict
        # interactive: one dialog per foreign instance, shared by all runners
        if self.lock_prompt is not None and self.lock_prompt["owner"] == other:
            self.lock_prompt["labels"].append(label)
            return await self.lock_prompt["future"]
        loop = asyncio.get_event_loop()
        prompt = {
            "owner": other, "labels": [label], "mid": mid, "db": db,
            "ts_seen": time.time() - age, "created": time.time(),
            "deadline": time.time() + self.LOCK_PROMPT_TIMEOUT,
            "detail": False, "note": "", "future": loop.create_future(),
        }
        self.lock_prompt = prompt
        self.add_history("Stale lock detected", "WARN", label,
                         f"held by {other} — heartbeat {int(age)}s ago")
        self.log_event("WARN", label, f"lock held by {other} (heartbeat {int(age)}s ago) "
                                      "— operator decision requested")
        poll = loop.create_task(self._lock_prompt_poll(prompt))
        try:
            verdict = await prompt["future"]
        finally:
            poll.cancel()
            self.lock_prompt = None
        self.lock_decisions[other] = verdict
        return verdict

    def lock_prompt_key(self, tok: str):
        """R / D / C handling from the key thread (marshalled to the loop)."""
        p = self.lock_prompt
        if p is None:
            return
        if tok in ("d", "D"):
            p["detail"] = not p["detail"]
            return
        if tok in ("c", "C", "esc", "q"):
            self._resolve_lock(p, "cancel", "cancelled by the operator")
            return
        if tok in ("r", "R"):
            age = time.time() - p["ts_seen"]
            if age < self.LOCK_LIVE_GRACE:
                p["note"] = (f"heartbeat is only {int(age)}s old (< {int(self.LOCK_LIVE_GRACE)}s) "
                             "— the instance appears ALIVE; reset refused")
                return
            self._resolve_lock(p, "reset", "operator confirmed takeover")

    def _resolve_lock(self, prompt: dict, verdict: str, why: str):
        def _do():
            fut = prompt["future"]
            if not fut.done():
                fut.set_result(verdict)
                word = "RESET" if verdict == "reset" else "kept"
                self.session.log(f"stale lock {word} ({why}) — instance {prompt['owner']}")
                self.add_history("Stale lock reset" if verdict == "reset" else "Lock kept",
                                 "OK" if verdict == "reset" else "WARN",
                                 prompt["labels"][0], why)
        self.call_soon(_do)

    async def _lock_prompt_poll(self, prompt: dict):
        """Re-reads the lease heartbeat while the dialog is open: if the
        'dead' instance heartbeats, the dialog resolves to cancel on its own
        (never reset a live instance); at the deadline it auto-cancels."""
        import asyncio
        while not prompt["future"].done():
            await asyncio.sleep(2.0)
            try:
                owner, ts = await prompt["db"].read_lease(prompt["mid"])
            except Exception:
                continue
            if owner != prompt["owner"] or ts <= 0:
                self._resolve_lock(prompt, "cancel", "lock changed hands / was released")
                return
            if ts > prompt["ts_seen"] + 0.5:
                prompt["ts_seen"] = ts
                self._resolve_lock(prompt, "cancel",
                                   "instance heartbeat advanced — it is ALIVE")
                return
            if time.time() >= prompt["deadline"]:
                self._resolve_lock(prompt, "cancel", "no operator decision (timeout)")
                return

    async def _headless_lock_verdict(self, mid: int, other: str, age: float, db) -> str:
        """--no-tui path. Without --reset-stale-locks: keep today's refusal.
        With it: verify the heartbeat is genuinely static, then allow the
        CAS takeover."""
        import asyncio
        if not self.cfg.reset_stale_locks:
            return "cancel"
        if age < self.LOCK_LIVE_GRACE:
            # young heartbeat: watch it across one full heartbeat interval
            watch = 70.0
            self.session.log(f"lock verification: watching heartbeat of {other} "
                             f"for up to {int(watch)}s")
            t0 = time.time()
            _, ts0 = await db.read_lease(mid)
            while time.time() - t0 < watch:
                await asyncio.sleep(5.0)
                if self.stop_event is not None and self.stop_event.is_set():
                    return "cancel"
                _, ts = await db.read_lease(mid)
                if ts > ts0 + 0.5:
                    self.session.log(f"lock verification: {other} is ALIVE — lock kept")
                    return "cancel"
        self.session.log(f"lock verification: {other} shows no heartbeat — "
                         "taking over (--reset-stale-locks)")
        return "reset"
