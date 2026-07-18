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

The ten TUI views. Each view is an independent component implementing the
View interface (title, nav key, footer hints, handle(), render()). Adding a
future view is one class plus one registry entry — the navigation framework
never changes.
"""
from __future__ import annotations

import time
from typing import List

from .. import (AUTHOR, AUTHOR_EMAIL, DOCS_URL, LICENSE_SHORT, PRODUCT,
                PROJECT_URL, REPOSITORY, SUPPORT_URL, __version__, about_lines)
from ..progress.dashboard import STATUS_COLOR, c, truncate, visible_len
from ..roadmap import roadmap_lines
from ..util import fmt_bytes, fmt_dhms, pct
from .widgets import ScrollList, panel, progress_bar, row, sparkline

ICON = {"SUCCESS": "✓", "RUNNING": "⟳", "QUEUED": "◌", "FAILED": "✗",
        "PARTIAL": "◑", "CANCELLED": "⏸", "SKIPPED": "○", "RETRYING": "⟳",
        "STALE": "⚠", "REMOTE": "⇄", "WARNINGS": "!"}


class View:
    key = ""
    title = ""
    nav = ""
    supports_search = False

    def footer(self, app) -> str:
        return ""

    def handle(self, app, tok: str) -> bool:
        return False

    def render(self, app, w: int, h: int) -> List[str]:
        return []


def _running(snap):
    return [m for m in snap["mailboxes"] if m["status"] in ("RUNNING", "RETRYING")]


# ----------------------------------------------------------- Dashboard --

class DashboardView(View):
    """F1 — the familiar classic dashboard. Rendering is delegated to the
    proven classic renderer by the app shell (same layout, same colours);
    this class only contributes navigation-bar identity and key handling."""

    key, title, nav = "1", "Dashboard", "Dashboard"

    def footer(self, app):
        return ("⏎ Details   p Pause   Space Freeze   ^L Redraw   "
                "F2–F10 Views   ^C Quit")

    def handle(self, app, tok):
        if tok == "enter":
            run = _running(app.snap)
            if len(run) == 1:
                app.open_popup("mailbox", run[0])
            else:
                app.active = app.by_key["3"]        # pick from the Mailboxes view
            return True
        return False

    def render(self, app, w, h):
        # Fallback only (classic renderer unavailable, e.g. bare unit tests).
        return ["  (classic dashboard renders via the main renderer)"]


# ------------------------------------------------------------- Workers --

class WorkersView(View):
    """F2 — top: the cluster roster (every MailFerry instance sharing this
    State Database, with liveness); below: this instance's active transfer
    slots."""

    key, title, nav, supports_search = "2", "Workers", "Workers", True

    def __init__(self):
        self.list = ScrollList()

    def footer(self, app):
        return "↑↓ Select   ⏎ Details   / Search   p Pause   ^C Quit"

    def _rows(self, app):
        run = _running(app.snap)
        if app.search_for(self):
            q = app.search.lower()
            run = [m for m in run if q in m["label"].lower()]
        return run

    def handle(self, app, tok):
        run = self._rows(app)
        if tok == "enter" and run:
            self.list.clamp(len(run), 10)
            app.open_popup("mailbox", run[self.list.sel])
            return True
        return self.list.key(tok, len(run), 10)

    def _cluster_panel(self, app, w):
        roster = list(app.hub.cluster) if app.hub else []
        me = app.hub.worker_id if app.hub else ""
        out = [c(f" CLUSTER — {len(roster)} worker(s) on {app.snap['db']}", "bold"), ""]
        out.append(c(row([("Worker ID", 30, "<"), ("Host", 18, "<"), ("Status", 8, "<"),
                          ("Mailboxes", 9, ">"), ("Heartbeat", 10, ">"),
                          ("Connected since", 16, ">")]), "dim"))
        scol = {"WORKING": "green", "IDLE": "cyan", "OFFLINE": "red"}
        for wk in roster:
            wid = wk["id"] + (" ◂ this" if wk["id"] == me else "")
            hb = f"{int(wk['hb_age'])}s ago" if wk.get("heartbeat") else "-"
            since = time.strftime("%d %b %H:%M", time.localtime(wk["started"])) \
                if wk.get("started") else "-"
            out.append(row([(truncate(wid, 30), 30, "<"),
                            (truncate(wk["host"] or "-", 18), 18, "<"),
                            (c(wk["status"], scol.get(wk["status"], "white")), 8, "<"),
                            (str(wk["active"]), 9, ">"), (hb, 10, ">"),
                            (since, 16, ">")]))
        if not roster:
            out.append("  (single instance — no other workers have joined this "
                       "State Database)")
        out.append("")
        return out

    def render(self, app, w, h):
        out = self._cluster_panel(app, w)
        run = self._rows(app)
        out.append(c(f" TRANSFERS — {len(run)} active of {app.snap['workers']} local slots"
                     + (f"   /{app.search}" if app.search_for(self) else ""), "bold"))
        out.append("")
        out.append(c(row([("ID", 4, "<"), ("Mailbox", 26, "<"), ("Folder", 14, "<"),
                          ("Operation", 24, "<"), ("Msgs", 10, ">"), ("Speed", 9, ">"),
                          ("Elapsed", 8, ">")]), "dim"))
        vh = max(1, h - len(out) - 1)
        self.list.clamp(len(run), vh)
        for i in range(self.list.top, min(len(run), self.list.top + vh)):
            m = run[i]
            el = fmt_dhms((m["end"] or time.time()) - m["start"]) if m["start"] else "-"
            cells = [(f"W{i + 1}", 4, "<"), (m["label"], 26, "<"),
                     (m["folder"] or "-", 14, "<"), (m["op"] or m["status"], 24, "<"),
                     (f"{m['msgs_done']:,}", 10, ">"),
                     (f"{fmt_bytes(app.mb_rate(m['index']))}/s", 9, ">"), (el, 8, ">")]
            out.append(row(cells, selected=(i == self.list.sel)))
        if not run:
            out.append("  (no active transfers on this instance)")
        return out


# ----------------------------------------------------------- Mailboxes --

class MailboxesView(View):
    key, title, nav, supports_search = "3", "Mailboxes", "Mailboxes", True
    FILTERS = ["ALL", "RUNNING", "FAILED", "QUEUED", "DONE"]
    SORTS = ["#", "status", "msgs", "pct", "elapsed"]

    def __init__(self):
        self.list = ScrollList()
        self.filter = 0
        self.sort = 0
        self.reverse = False

    def footer(self, app):
        return ("↑↓ Select   ⏎ Details   / Search   f Filter   s Sort   "
                "r Retry   ^C Quit")

    def _rows(self, app):
        mbs = list(app.snap["mailboxes"])
        f = self.FILTERS[self.filter]
        if f == "RUNNING":
            mbs = [m for m in mbs if m["status"] in ("RUNNING", "RETRYING")]
        elif f == "FAILED":
            mbs = [m for m in mbs if m["status"] in ("FAILED", "PARTIAL", "STALE")]
        elif f == "QUEUED":
            mbs = [m for m in mbs if m["status"] == "QUEUED"]
        elif f == "DONE":
            mbs = [m for m in mbs if m["status"] in ("SUCCESS", "SKIPPED", "WARNINGS")]
        if app.search_for(self):
            q = app.search.lower()
            mbs = [m for m in mbs if q in m["label"].lower()]
        keyf = {
            "#": lambda m: m["index"], "status": lambda m: m["status"],
            "msgs": lambda m: m["msgs_done"],
            "pct": lambda m: (m["bytes_done"] / m["bytes_total"]) if m["bytes_total"] else 0,
            "elapsed": lambda m: (m["end"] or time.time()) - m["start"] if m["start"] else 0,
        }[self.SORTS[self.sort]]
        mbs.sort(key=keyf, reverse=self.reverse)
        return mbs

    def handle(self, app, tok):
        rows = self._rows(app)
        if tok == "f":
            self.filter = (self.filter + 1) % len(self.FILTERS)
            return True
        if tok == "s":
            self.sort = (self.sort + 1) % len(self.SORTS)
            return True
        if tok == "S":
            self.reverse = not self.reverse
            return True
        if tok == "enter" and rows:
            self.list.clamp(len(rows), 10)
            app.open_popup("mailbox", rows[self.list.sel])
            return True
        if tok in ("r", "R") and rows:
            self.list.clamp(len(rows), 10)
            m = rows[self.list.sel]
            app.retry(all_failed=(tok == "R"), label=m["label"])
            return True
        return self.list.key(tok, len(rows), 10)

    def render(self, app, w, h):
        rows = self._rows(app)
        head = (f" MAILBOXES — {len(app.snap['mailboxes'])} total · filter {self.FILTERS[self.filter]}"
                f" · sort {self.SORTS[self.sort]}{'▲' if self.reverse else '▼'}"
                + (f"   /{app.search}" if app.search_for(self) else ""))
        out = [c(head, "bold"), ""]
        out.append(c(row([("#", 4, ">"), ("St", 3, "<"), ("Mailbox", 26, "<"),
                          ("Fldr", 6, "<"), ("Msgs", 16, "<"), ("Fail", 5, ">"),
                          ("Data", 11, ">"), ("Pct", 5, ">"), ("Elapsed", 8, ">")]), "dim"))
        vh = max(1, h - 3)
        self.list.clamp(len(rows), vh)
        for i in range(self.list.top, min(len(rows), self.list.top + vh)):
            m = rows[i]
            icon = ICON.get(m["status"], "?")
            msgs = f"{m['msgs_done']:,}/{m['msgs_total']:,}" if m["msgs_total"] else "-"
            data = fmt_bytes(m["bytes_done"]) if m["bytes_done"] else "-"
            p = pct(m["bytes_done"], m["bytes_total"]) if m["bytes_total"] else \
                (pct(m["msgs_done"], m["msgs_total"]) if m["msgs_total"] else "-")
            el = fmt_dhms((m["end"] or time.time()) - m["start"]) if m["start"] else "-"
            nfail = m.get("failed", 0) + m["skipped"]
            line = row([(str(m["index"]), 4, ">"),
                        (c(icon, STATUS_COLOR.get(m["status"], "white")), 3, "<"),
                        (m["label"], 26, "<"), (f"{m['fi']}/{m['ft']}" if m["ft"] else "-", 6, "<"),
                        (msgs, 16, "<"),
                        (c(str(nfail), "red") if nfail else "-", 5, ">"),
                        (data, 11, ">"), (p, 5, ">"), (el, 8, ">")],
                       selected=(i == self.list.sel))
            out.append(line)
            if i == self.list.sel and m["status"] in ("FAILED", "PARTIAL", "STALE") and m["error"]:
                out.append(c("      └ " + truncate(m["error"], w - 10), "red"))
            elif i == self.list.sel and m["status"] == "WARNINGS":
                out.append(c("      └ completed with warnings — "
                             f"{m.get('failed', 0) + m['skipped']} message(s) in the "
                             "Failed Message Registry (mailferry failed / retry-failed)",
                             "yellow"))
        if not rows:
            out.append("  (no mailboxes match)")
        return out


# --------------------------------------------------------------- Queue --

class QueueView(View):
    key, title, nav = "4", "Queue", "Queue"

    def footer(self, app):
        return "p Pause/Resume   u Reload CSV   r Retry   R Retry all   ^C Quit"

    def handle(self, app, tok):
        if tok == "u":
            app.reload_csv()
            return True
        if tok == "R":
            app.retry(all_failed=True)
            return True
        return False

    def render(self, app, w, h):
        mbs = app.snap["mailboxes"]
        groups = [
            ("RUNNING", [m for m in mbs if m["status"] in ("RUNNING", "RETRYING")]),
            ("REMOTE", [m for m in mbs if m["status"] == "REMOTE"]),
            ("PENDING", [m for m in mbs if m["status"] == "QUEUED"]),
            ("COMPLETED", [m for m in mbs if m["status"] in ("SUCCESS", "SKIPPED")]),
            ("WARNINGS", [m for m in mbs if m["status"] == "WARNINGS"]),
            ("PARTIAL", [m for m in mbs if m["status"] == "PARTIAL"]),
            ("STALE", [m for m in mbs if m["status"] == "STALE"]),
            ("FAILED", [m for m in mbs if m["status"] == "FAILED"]),
        ]
        out = [c(f" QUEUE — {len(mbs)} mailboxes", "bold"), ""]
        for name, items in groups:
            colr = STATUS_COLOR.get(name if name != "PENDING" else "QUEUED", "white")
            names = " · ".join(f"{m['index']} {m['label']}" for m in items[:6])
            if len(items) > 6:
                names += f"  … +{len(items) - 6}"
            out.append(c(f" {name} ({len(items)})", colr))
            out.append("   " + truncate(names or "—", w - 4))
        return out


# --------------------------------------------------- History / Activity --

class HistoryView(View):
    """F5 — chronological milestone feed (migrations started/completed,
    folders migrated, reconnects, stale detection & recovery, lock events).
    Fully navigable: ↑↓/k j select, Enter opens the detail popup, Esc/q
    returns to the Dashboard. Follows the tail until the operator scrolls."""

    key, title, nav, supports_search = "5", "History / Activity", "History", True
    STATUS_COLR = {"OK": "green", "WARN": "yellow", "FAIL": "red"}

    def __init__(self):
        self.list = ScrollList()
        self.follow = True

    def footer(self, app):
        return ("↑↓/k j Navigate   ⏎ Details   / Search   End Follow   "
                "Esc/q Back   ^C Quit")

    def _rows(self, app):
        evs = list(app.hub.history) if app.hub else []
        if app.search_for(self):
            q = app.search.lower()
            evs = [e for e in evs if q in e[1].lower() or q in e[3].lower()
                   or q in e[4].lower()]
        return evs

    def handle(self, app, tok):
        evs = self._rows(app)
        if tok == "enter" and evs:
            self.list.clamp(len(evs), 10)
            app.open_popup("history", evs[self.list.sel])
            return True
        if tok in ("up", "down", "pgup", "pgdn", "home", "end"):
            self.follow = (tok == "end")
            return self.list.key(tok, len(evs), 10)
        return False

    def render(self, app, w, h):
        evs = self._rows(app)
        head = (f" HISTORY / ACTIVITY — {len(evs)} events"
                + (f"   /{app.search}" if app.search_for(self) else "")
                + ("" if not self.follow else "   · following"))
        out = [c(head, "bold"), ""]
        dw = max(16, w - 66)
        out.append(c(row([("Time", 19, "<"), ("Event", 28, "<"), ("Status", 6, "<"),
                          ("Mailbox / Details", dw + 12, "<")]), "dim"))
        vh = max(1, h - 3)
        if self.follow and evs:
            self.list.sel = len(evs) - 1
        self.list.clamp(len(evs), vh)
        for i in range(self.list.top, min(len(evs), self.list.top + vh)):
            t, event, status, mailbox, details = evs[i]
            stamp = time.strftime("%Y-%m-%d %H:%M:%S", time.localtime(t))
            tail = mailbox if mailbox != "-" else ""
            if details:
                tail = f"{tail} · {details}" if tail else details
            line = row([(stamp, 19, "<"), (event, 28, "<"),
                        (c(status, self.STATUS_COLR.get(status, "white")), 6, "<"),
                        (truncate(tail, dw + 12), dw + 12, "<")],
                       selected=(i == self.list.sel))
            out.append(line)
        if not evs:
            out.append("  (no activity yet)")
        return out


# -------------------------------------------------------------- Errors --

class ErrorsView(View):
    key, title, nav, supports_search = "6", "Errors", "Errors", True

    def __init__(self):
        self.list = ScrollList()

    def footer(self, app):
        return "↑↓ Select   ⏎ Details   / Search   ^C Quit"

    def _rows(self, app):
        errs = list(app.hub.errors)[::-1] if app.hub else []
        if app.search_for(self):
            q = app.search.lower()
            errs = [e for e in errs if q in e[1].lower() or q in e[2].lower()]
        return errs

    def handle(self, app, tok):
        errs = self._rows(app)
        if tok == "enter" and errs:
            self.list.clamp(len(errs), 10)
            app.open_popup("error", errs[self.list.sel])
            return True
        return self.list.key(tok, len(errs), 10)

    def render(self, app, w, h):
        errs = self._rows(app)
        out = [c(f" ERRORS & WARNINGS — {len(errs)} recorded"
                 + (f"   /{app.search}" if app.search_for(self) else ""), "bold"), ""]
        out.append(c(row([("Time", 9, "<"), ("Mailbox", 24, "<"), ("Message", w - 38, "<")]), "dim"))
        vh = max(1, h - 3)
        self.list.clamp(len(errs), vh)
        for i in range(self.list.top, min(len(errs), self.list.top + vh)):
            t, mb, msg = errs[i]
            out.append(row([(time.strftime("%H:%M:%S", time.localtime(t)), 9, "<"),
                            (mb, 24, "<"), (truncate(msg, w - 40), w - 38, "<")],
                           selected=(i == self.list.sel)))
        if not errs:
            out.append(c("  no errors recorded — all clear", "green"))
        return out


# --------------------------------------------------------- Performance --

class PerformanceView(View):
    key, title, nav = "7", "Performance", "Perf"

    def footer(self, app):
        return "Space Freeze   ^L Redraw   ^C Quit"

    def render(self, app, w, h):
        agg = app.snap["agg"]
        s = app.sysmon_snap
        br, mr = app.rates()
        sw = min(40, w - 24)
        out = [c(" PERFORMANCE", "bold"), c("   60-second histories", "dim"), ""]
        out.append(f"  Throughput  {fmt_bytes(br) + '/s':>10}   {sparkline(app.history['rate'], sw)}")
        out.append(f"  Messages    {str(round(mr)) + '/s':>10}   {sparkline(app.history['msgs'], sw)}")
        cpu = s.get("cpu")
        out.append(f"  CPU         {(str(round(cpu)) + '%') if cpu is not None else 'N/A':>10}"
                   f"   {sparkline(app.history['cpu'], sw)}")
        load = s.get("load")
        if load:
            out.append(f"  Load        {load[0]:.2f} {load[1]:.2f} {load[2]:.2f}")
        mt, mu = s.get("mem_total"), s.get("mem_used")
        out.append(f"  Memory      Process RSS {fmt_bytes(s.get('rss'))}"
                   + (f" · system {fmt_bytes(mu)}/{fmt_bytes(mt)}" if mt and mu else ""))
        out.append(f"  Network     wire ↓{fmt_bytes(agg['wire_rx'])} · ↑{fmt_bytes(agg['wire_tx'])}")
        busy = app.snap["counts"].get("RUNNING", 0) + app.snap["counts"].get("RETRYING", 0)
        out.append(f"  Workers     {busy}/{app.snap['workers']} busy · "
                   f"reconnects {agg['reconnects']} · retries {agg['retries']}")
        out.append(f"  Efficiency  adopted {agg['adopted']:,} duplicates prevented · "
                   f"skipped {agg['skipped_msgs']:,}")
        return out


# ---------------------------------------------------------------- Logs --

class LogsView(View):
    """F8 — the live log, tail -f style. Follow mode pins the view to the
    newest entries; ANY manual scroll key switches to browsing (FOLLOW: OFF)
    without the view ever jumping away; F snaps back to the live tail."""

    key, title, nav, supports_search = "8", "Logs", "Logs", True

    def __init__(self):
        self.top = 0                      # viewport offset (first visible row)
        self.follow = True
        self.sev = 0                      # 0 all, 1 warn+, 2 error
        self.SEV = ["ALL", "WARN+", "ERROR"]
        self._vh = 10                     # last viewport height (for paging)

    def footer(self, app):
        return ("↑↓/k j · PgUp/PgDn · Home/End Scroll   F Follow   "
                "/ Search   i/w/e Severity   ^C Quit")

    def _rows(self, app):
        evs = list(app.hub.events) if app.hub else []
        if self.sev == 1:
            evs = [e for e in evs if e[1] in ("WARN", "ERROR")]
        elif self.sev == 2:
            evs = [e for e in evs if e[1] == "ERROR"]
        if app.search_for(self):
            q = app.search.lower()
            evs = [e for e in evs if q in e[3].lower() or q in e[2].lower()]
        return evs

    def handle(self, app, tok):
        if tok == "F":
            self.follow = not self.follow     # F: back to the live tail
            return True
        if tok in ("i", "w", "e"):
            self.sev = {"i": 0, "w": 1, "e": 2}[tok]
            return True
        n = len(self._rows(app))
        vh = max(1, self._vh)
        if tok in ("up", "down", "pgup", "pgdn", "home", "end"):
            if tok == "end":
                self.follow = True            # End = jump to tail + follow
                return True
            self.follow = False               # manual scroll leaves the tail
            if tok == "up":
                self.top -= 1
            elif tok == "down":
                self.top += 1
            elif tok == "pgup":
                self.top -= vh
            elif tok == "pgdn":
                self.top += vh
            elif tok == "home":
                self.top = 0
            self.top = max(0, min(self.top, max(0, n - vh)))
            return True
        return False

    def render(self, app, w, h):
        evs = self._rows(app)
        vh = max(1, h - 2)
        self._vh = vh
        n = len(evs)
        if self.follow:
            self.top = max(0, n - vh)         # pinned to the newest entries
        else:
            self.top = max(0, min(self.top, max(0, n - vh)))
        sevcol = {"INFO": "white", "WARN": "yellow", "ERROR": "red"}
        fol = (c("FOLLOW: ON ▶", "green") if self.follow
               else c("FOLLOW: OFF ⏸ (press F to resume)", "yellow"))
        head = (c(f" LOGS — {n} events · filter {self.SEV[self.sev]} · ", "bold") + fol
                + (c(f"   /{app.search}", "bold") if app.search_for(self) else ""))
        out = [head]
        end = min(n, self.top + vh)
        for i in range(self.top, end):
            t, sev, mb, msg = evs[i]
            stamp = time.strftime("%H:%M:%S", time.localtime(t))
            line = f"  {stamp}  {sev:<5} {truncate(mb, 22):<22} {truncate(msg, w - 42)}"
            out.append(c(line, sevcol.get(sev, "white")))
        if not evs:
            out.append("  (no log events yet)")
        elif not self.follow:
            out.append(c(f"  ── viewing {self.top + 1}–{end} of {n} · "
                         "F returns to the live tail ──", "dim"))
        return out


# ------------------------------------------------------------ Settings --

class SettingsView(View):
    key, title, nav = "9", "Settings", "Cfg"

    def __init__(self):
        self.sel = 0
        self.rates = [0.25, 0.5, 1.0, 2.0]

    def footer(self, app):
        return "↑↓ Select   ←→ Change   ^C Quit"

    def handle(self, app, tok):
        if tok in ("up", "down"):
            self.sel = (self.sel + (1 if tok == "down" else -1)) % 3
            return True
        if tok in ("left", "right"):
            d = 1 if tok == "right" else -1
            if self.sel == 0:
                i = (self.rates.index(app.refresh) + d) % len(self.rates) \
                    if app.refresh in self.rates else 0
                app.refresh = self.rates[i]
            elif self.sel == 1:
                app.detail_mode = {"auto": "always", "always": "compact",
                                   "compact": "auto"}[app.detail_mode]
            elif self.sel == 2:
                app.show_activity = not app.show_activity
            return True
        return False

    def render(self, app, w, h):
        cfg = app.cfg
        out = [c(" SETTINGS", "bold"), ""]
        out.append(f"  Run        {app.snap['csv']} · run {cfg.run_id} · State Database "
                   f"{app.snap['db']} · logs {app.snap['logs']}")
        out.append(f"  Workers    {cfg.workers} · max conns/mailbox {cfg.max_conns_per_mailbox} · "
                   f"per-host cap {cfg.per_host_conns} · order {cfg.order}")
        out.append(f"  Behaviour  retries {cfg.retries} · backoff {int(cfg.retry_delay)}s×2 · "
                   f"timeout {int(cfg.timeout)}s · TLS verify {'ON' if cfg.tls_verify else 'OFF'} · "
                   f"compress {cfg.compress}")
        out.append(f"  Self-heal  stale after {int(cfg.stale_timeout)}s · "
                   f"{cfg.recovery_retries} recovery attempts every {int(cfg.recovery_interval)}s · "
                   f"lock timeout {int(cfg.lock_timeout)}s")
        out.append(f"  Cluster    worker {app.hub.worker_id or '-'} · "
                   f"offline threshold {int(cfg.worker_timeout)}s · "
                   f"{len(app.hub.cluster) or 1} worker(s) on this State Database")
        out.append("")
        out.append(c("  ── live display toggles ──────────────────────────────", "dim"))
        toggles = [f"Refresh rate    ◀ {app.refresh:g} s ▶   (display only — engine unaffected)",
                   f"Detail rows     ◀ {app.detail_mode} ▶",
                   f"Activity feed   ◀ {'on' if app.show_activity else 'off'} ▶"]
        for i, t in enumerate(toggles):
            out.append(("▸ " if i == self.sel else "  ") + t if i != self.sel
                       else c("▸ " + t, "cyan"))
        return out


# ---------------------------------------------------------------- Help --

class HelpView(View):
    key, title, nav = "0", "Help", "Help"

    def __init__(self):
        self.list = ScrollList()

    def footer(self, app):
        return "↑↓ Scroll   Esc Back to Dashboard   ^C Quit"

    def _content(self, app):
        lines = list(about_lines())
        lines += ["", "──────────────────────────────────────", "Keyboard Shortcuts", ""]
        groups = [
            ("Navigation", ["1–0 / F1–F10  switch view", "↑↓ or k/j select", "PgUp/PgDn scroll",
                            "Home/End jump", "⏎ details", "Esc or q back / clear",
                            "Tab cycle panes"]),
            ("Migration Control", ["p pause / resume", "r retry selected", "R retry all failed",
                                   "u reload CSV"]),
            ("Display", ["Space freeze / resume refresh", "/ search", "s sort · f filter",
                         "^L redraw"]),
            ("Shutdown", ["^C graceful shutdown (dialog)", "^C again forces immediate exit"]),
        ]
        for name, keys in groups:
            lines.append(f"  {name}")
            for k in keys:
                lines.append(f"    {k}")
            lines.append("")
        lines += ["Views", ""]
        for v in app.view_order:
            lines.append(f"    {v.key}  {v.title}")
        return lines

    def handle(self, app, tok):
        return self.list.key(tok, self._content_len, max(1, self._last_h))

    _content_len = 1
    _last_h = 10

    def render(self, app, w, h):
        content = self._content(app)
        self._content_len = len(content)
        self._last_h = h
        self.list.clamp(len(content), h)
        out = []
        for i in range(self.list.top, min(len(content), self.list.top + h)):
            line = content[i]
            if line.startswith("About ") or line in ("Keyboard Shortcuts", "Views"):
                out.append(c("  " + line, "bold"))
            elif line == PRODUCT or "──────" in line:
                out.append(c("  " + line, "cyan"))
            else:
                out.append("  " + line)
        return out


def _frac(done, total):
    return (done / total) if total else 0.0


def _sidebyside(left: List[str], right: List[str]) -> List[str]:
    out = []
    n = max(len(left), len(right))
    lw = max((visible_len(x) for x in left), default=0)
    for i in range(n):
        l = left[i] if i < len(left) else " " * lw
        r = right[i] if i < len(right) else ""
        pad = lw - visible_len(l)
        out.append(l + " " * pad + " " + r)
    return out


def all_views() -> List[View]:
    return [DashboardView(), WorkersView(), MailboxesView(), QueueView(),
            HistoryView(), ErrorsView(), PerformanceView(), LogsView(),
            SettingsView(), HelpView()]
