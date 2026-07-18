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

The TUI application shell: owns the view registry, active view, popups,
search state and key routing; composes each frame (header, global status
bar, navigation bar, active view, footer) for the single-writer renderer.

Threading contract: the renderer thread calls frame(); the key thread calls
handle_key(). Both take the app lock. Neither ever touches the engine
directly — migration actions are marshalled onto the event loop via the
ControlHub. The engine never waits on the UI.
"""
from __future__ import annotations

import threading
import time
from collections import deque
from typing import List, Optional, Tuple

from .. import SLOGAN, __version__, banner_line
from ..progress.dashboard import (MIN_COLS, MIN_ROWS, c, clip_ansi, lock_dialog,
                                   shutdown_dialog, too_small_notice, truncate,
                                   visible_len)
from ..progress.stats import RateTracker
from ..util import fmt_bytes, fmt_dhms, pct
from . import views as V
from .widgets import overlay


class TuiApp:
    def __init__(self, cfg, stats, hub, sysmon):
        self.cfg = cfg
        self.stats = stats
        self.hub = hub
        self.sysmon = sysmon
        self.renderer = None                # legacy classic renderer (set by CLI)
        self.lock = threading.Lock()
        self.view_order: List[V.View] = V.all_views()
        self.by_key = {v.key: v for v in self.view_order}
        self.active = self.view_order[0]
        self.popup: Optional[Tuple[str, object]] = None
        self.searching = False
        self.search = ""
        self.search_view: Optional[V.View] = None
        self.frozen = False
        self.refresh = 0.25
        self.detail_mode = "auto"
        self.show_activity = True
        self.wake: Optional[threading.Event] = None
        # live data caches
        self.snap = stats.snapshot()
        self.sysmon_snap = {}
        self._grate = RateTracker()          # payload (committed bytes) — ETA
        self._gwire = RateTracker()          # wire bytes — live throughput
        self._mb_rt = {}
        self._mb_wire = {}
        self.history = {"rate": deque(maxlen=60), "msgs": deque(maxlen=60),
                        "cpu": deque(maxlen=60)}
        self._last_sample = 0.0
        self._cache: Optional[Tuple[List[str], object]] = None

    # -------------------------------------------------------- data pull --
    def _refresh_data(self):
        self.snap = self.stats.snapshot()
        self.sysmon_snap = self.sysmon.snapshot() if self.sysmon else {}
        agg = self.snap["agg"]
        now = self.snap["ts"]
        self._grate.update(now, agg["bytes_done"], agg["msgs_done"])
        self._gwire.update(now, max(agg["wire_rx"], agg["wire_tx"]), 0)
        for m in self.snap["mailboxes"]:
            rt = self._mb_rt.setdefault(m["index"], RateTracker())
            rt.update(now, m["bytes_done"], m["msgs_done"])
            wt = self._mb_wire.setdefault(m["index"], RateTracker())
            wt.update(now, max(m["src"]["rx"] + m["src"]["tx"],
                               m["dst"]["rx"] + m["dst"]["tx"]), 0)
        if now - self._last_sample >= 1.0:
            self._last_sample = now
            br, mr = self.rates()
            self.history["rate"].append(br)
            self.history["msgs"].append(mr)
            cpu = self.sysmon_snap.get("cpu")
            self.history["cpu"].append(cpu if cpu is not None else 0.0)
        if self.hub is not None:
            self.hub.rates = self._grate.rates()
            self.hub.eta = self._eta()

    def rates(self):
        """(bytes/s, msgs/s) for display. Bytes ride the wire counters — they
        tick on every socket read/write, so throughput is live even while a
        single huge message streams; messages/s stays payload-based."""
        return max(self._gwire.rates()[0], self._grate.rates()[0]), self._grate.rates()[1]

    def mb_rate(self, index) -> float:
        """Live per-mailbox throughput: wire rate when transferring locally,
        mirrored payload rate for REMOTE rows — whichever is meaningful."""
        wt = self._mb_wire.get(index)
        rt = self._mb_rt.get(index)
        return max(wt.rates()[0] if wt else 0.0, rt.rates()[0] if rt else 0.0)

    def _eta(self):
        """Payload rate first (accurate against remaining payload bytes);
        wire rate as fallback so ETA stays live during long streams."""
        agg = self.snap["agg"]
        rem_b = max(0, agg["bytes_total"] - agg["bytes_done"])
        rem_m = max(0, agg["msgs_total"] - agg["msgs_done"])
        eta = self._grate.eta(rem_b, rem_m)
        if eta is None and rem_b > 0:
            wr = self._gwire.rates()[0]
            if wr > 1024:
                eta = rem_b / wr
        return eta

    # ------------------------------------------------------ search glue --
    def search_for(self, view) -> bool:
        return bool(self.search) and self.search_view is view

    # ----------------------------------------------------------- actions --
    def retry(self, all_failed=False, label=None):
        self.hub.retry_failed(label=label, all_failed=all_failed)

    def reload_csv(self):
        self.hub.do_reload()

    def open_popup(self, kind, data):
        self.popup = (kind, data)

    def close_popup(self):
        self.popup = None

    # -------------------------------------------------------- key input --
    def handle_key(self, tok: str):
        with self.lock:
            self._handle(tok)
        if self.wake is not None:
            self.wake.set()

    def _handle(self, tok: str):
        # stale-lock dialog captures all input while open
        if self.hub is not None and self.hub.lock_prompt is not None:
            self.hub.lock_prompt_key(tok)
            return
        # search capture mode
        if self.searching:
            if tok == "enter":
                self.searching = False
            elif tok == "esc":
                self.searching = False
                self.search = ""
                self.search_view = None
            elif tok == "backspace":
                self.search = self.search[:-1]
            elif len(tok) == 1 and tok >= " ":
                self.search += tok
            return
        if tok == "k":
            tok = "up"                       # vim-style navigation
        elif tok == "j":
            tok = "down"
        if self.popup is not None:
            if tok in ("esc", "enter", "q"):
                self.close_popup()
            return
        # global keys
        if tok in self.by_key:
            self.active = self.by_key[tok]
            return
        if tok.startswith("f") and tok[1:].isdigit():
            idx = int(tok[1:]) - 1
            if 0 <= idx < len(self.view_order):
                self.active = self.view_order[idx]
            return
        if tok == "space":
            self.frozen = not self.frozen
            return
        if tok == "ctrl_l":
            self._cache = None
            return
        if tok in ("?",):
            self.active = self.by_key["0"]
            return
        if tok in ("esc", "q"):
            if self.search:
                self.search = ""
                self.search_view = None
            else:
                self.active = self.by_key["1"]   # back to the Dashboard
            return
        if tok == "/" and getattr(self.active, "supports_search", False):
            self.searching = True
            self.search = ""
            self.search_view = self.active
            return
        if tok == "p":
            self.hub.paused = not self.hub.paused
            return
        # delegate to active view
        self.active.handle(self, tok)

    # ----------------------------------------------------------- render --
    def frame(self) -> Tuple[List[str], object]:
        import shutil
        cols, rows = shutil.get_terminal_size(fallback=(100, 30))
        with self.lock:
            if self.hub is not None and self.hub.shutdown_active:
                return shutdown_dialog(self.hub, cols, rows), None
            if self.hub is not None and self.hub.lock_prompt is not None:
                return lock_dialog(self.hub, cols, rows), None
            if cols < MIN_COLS or rows < MIN_ROWS:
                self._refresh_data()
                return too_small_notice(self.snap, cols, rows), None
            if self.frozen and self._cache is not None:
                return self._cache
            self._refresh_data()
            try:
                if self.active.key == "1" and self.renderer is not None:
                    lines = self._classic_frame(cols, rows)
                else:
                    lines = self._compose(cols, rows)
            except Exception as e:                    # a view bug must never
                lines = self._error_frame(cols, rows, e)   # freeze navigation
            if self.popup is not None:
                lines = overlay(lines, self._popup_box(cols), cols, rows)
            self._cache = (lines, None)
            return lines, None

    def _classic_frame(self, cols, rows) -> List[str]:
        """F1: the familiar classic dashboard, verbatim (same layout and
        colours), with the function-key navigation bar under the banner."""
        dash, _ = self.renderer.render(self.snap, fit=True, with_console=False,
                                       reserve=2)
        cut = 4 if len(dash) >= 4 and dash[0].startswith("╔") else 0
        out = dash[:cut] + [self._nav_bar(cols)] + dash[cut:]
        out.append(self._footer(cols))
        return out

    def _error_frame(self, cols, rows, exc) -> List[str]:
        if self.hub is not None:
            msg = f"view '{self.active.title}' failed to render: {exc!r}"
            if msg != getattr(self, "_last_render_err", ""):
                self._last_render_err = msg
                self.hub.log_event("ERROR", "tui", msg)
        body = [c(banner_line(), "bold"), "",
                c(f"The {self.active.title} view failed to render.", "red"),
                truncate(repr(exc), cols - 6), "",
                "The migration continues unaffected.",
                "Press 1 for the Dashboard, or 8 to check the Logs."]
        pad = [""] * max(0, (rows - len(body)) // 2)
        out = pad + [(" " * max(0, (cols - visible_len(l)) // 2)) + l for l in body]
        return out[: max(1, rows - 1)]

    def _compose(self, cols, rows) -> List[str]:
        target = rows - 1
        W = max(70, min(cols - 2, 120))
        out = ["╔" + "═" * W + "╗",
               "║" + banner_line().center(W) + "║",
               "║" + SLOGAN.center(W) + "║",
               "╚" + "═" * W + "╝",
               self._nav_bar(cols),
               "─" * min(cols, W + 2)]
        body_h = max(1, target - len(out) - 1)
        body = self.active.render(self, cols - 1, body_h)
        for i in range(body_h):
            line = body[i] if i < len(body) else ""
            out.append(" " + clip_ansi(line, cols - 1))
        out.append(self._footer(cols))
        return out

    def _nav_bar(self, cols) -> str:
        parts = []
        for i, v in enumerate(self.view_order, start=1):
            label = f"F{i} {v.nav or v.title}"
            if v is self.active:
                parts.append(c(f"▐{label}▌", "bold"))
            else:
                parts.append(label)
        badge = ""
        if self.hub is not None and self.hub.paused:
            badge = c(" ▮▮PAUSED", "yellow")
        elif self.frozen:
            badge = c(" ❄FROZEN", "cyan")
        bar = " " + " ".join(parts)
        return clip_ansi(bar, max(10, cols - visible_len(badge))) + badge

    def _footer(self, cols) -> str:
        if self.searching:
            return clip_ansi(c(f" search: /{self.search}▏  (⏎ keep · Esc clear)", "cyan"), cols)
        hint = self.active.footer(self)
        return clip_ansi(c(" " + hint, "dim"), cols)

    def _popup_box(self, cols) -> List[str]:
        kind, data = self.popup
        inner = min(cols - 8, 66)
        if kind == "mailbox":
            body = self._mailbox_detail(data, inner)
            title = f"Mailbox — {data['label']}"
        elif kind == "history":
            body = self._history_detail(data, inner)
            title = "Activity detail"
        else:
            body = self._error_detail(data, inner)
            title = "Error detail"
        box = ["┌─ " + c(truncate(title, inner - 4), "bold") + " "
               + "─" * (inner - visible_len(truncate(title, inner - 4)) - 3) + "┐"]
        for line in body:
            box.append("│ " + _fit(line, inner - 2) + " │")
        box.append("│ " + " " * (inner - 2) + " │")
        box.append("│ " + c(_fit("Esc / ⏎ to close", inner - 2), "dim") + " │")
        box.append("└" + "─" * inner + "┘")
        return [b + "░" if i else b for i, b in enumerate(box)] + ["  " + "░" * (inner - 1)]

    def _mailbox_detail(self, m, w) -> List[str]:
        sr, ds = m["src"], m["dst"]
        el = fmt_dhms((m["end"] or time.time()) - m["start"]) if m["start"] else "-"
        return [
            f"{m['label']}  →  {m.get('label2') or '?'}",
            f"Status {m['status']} (attempt {m['attempt']}/{m['max_attempts']})   elapsed {el}",
            f"Folders {m['fi']}/{m['ft']}   msgs {m['msgs_done']:,}/{m['msgs_total']:,} "
            f"({pct(m['msgs_done'], m['msgs_total'])})",
            f"Data {fmt_bytes(m['bytes_done'])}/{fmt_bytes(m['bytes_total'])}   "
            f"new {m['appended']:,} · adopted {m['adopted']:,} · skipped {m['skipped']:,}",
            f"Operation {m['op'] or '-'}   folder {m['folder'] or '-'}",
            *([f"Worker    {m['detail'].replace('processed by ', '').replace('taken over by ', '')}"]
              if m["status"] == "REMOTE" and m.get("detail") else []),
            f"Source      {sr['host']} [{','.join(sr['caps'])}] {sr['state']} ↓{fmt_bytes(sr['rx'])}",
            f"Destination {ds['host']} [{','.join(ds['caps'])}] {ds['state']} ↑{fmt_bytes(ds['tx'])}"
            + (f" · quota {ds['quota']:.0f}%" if ds["quota"] >= 0 else ""),
            f"Reconnects {sr['reconnects'] + ds['reconnects']} · retries {m['retries']}",
            f"Error {truncate(m['error'], w - 8) if m['error'] else '-'}",
            f"Log {truncate(m['log'] or '-', w - 6)}",
        ]

    def _history_detail(self, e, w) -> List[str]:
        t, event, status, mailbox, details = e
        import textwrap
        colr = {"OK": "green", "WARN": "yellow", "FAIL": "red"}.get(status, "white")
        out = [
            f"{time.strftime('%Y-%m-%d %H:%M:%S', time.localtime(t))}",
            "",
            c(event, "bold") + "   " + c(status, colr),
            f"Mailbox  {mailbox}",
            "",
        ]
        for chunk in textwrap.wrap(details or "-", w - 2) or ["-"]:
            out.append(chunk)
        return out

    def _error_detail(self, e, w) -> List[str]:
        t, mb, msg = e
        import textwrap
        out = [f"{time.strftime('%Y-%m-%d %H:%M:%S', time.localtime(t))}   {mb}", ""]
        for chunk in textwrap.wrap(msg, w - 2) or ["-"]:
            out.append(chunk)
        return out


def _center(s: str, cols: int) -> str:
    pad = max(0, (cols - visible_len(s)) // 2)
    return " " * pad + s


def _fit(s: str, width: int) -> str:
    v = visible_len(s)
    if v > width:
        return clip_ansi(s, width)
    return s + " " * (width - v)
