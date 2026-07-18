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

Terminal UX: flicker-free alternate-screen dashboard (differential
repaint, height-fit with detail degradation) + non-TTY status lines +
optional NDJSON progress. Rendering runs on its own thread; the engine
never blocks on it.
"""
from __future__ import annotations

import json
import re
import shutil
import sys
import threading
import time
from datetime import datetime, timedelta
from typing import Dict, List, Optional

from .. import SLOGAN, banner_line
from ..util import fmt_bytes, fmt_dhms, pct
from .stats import RateTracker

ISATTY = sys.stdout.isatty()
ANSI_RE = re.compile(r"\033\[[0-9;]*m")
COLORS = {"reset": "\033[0m", "dim": "\033[2m", "bold": "\033[1m", "red": "\033[31m",
          "green": "\033[32m", "yellow": "\033[33m", "cyan": "\033[36m", "white": "\033[37m"}
STATUS_COLOR = {"QUEUED": "dim", "RUNNING": "yellow", "RETRYING": "yellow",
                "SUCCESS": "green", "PARTIAL": "yellow", "FAILED": "red",
                "CANCELLED": "yellow", "SKIPPED": "cyan", "WARNINGS": "yellow",
                "STALE": "red", "RECOVER": "yellow", "REMOTE": "cyan"}
MIN_COLS, MIN_ROWS = 80, 20
_SPINNER = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"


def visible_len(s: str) -> int:
    return len(ANSI_RE.sub("", s))


def c(text: str, color: str) -> str:
    if not ISATTY:
        return text
    return f"{COLORS.get(color, '')}{text}{COLORS['reset']}"


def cpad(text: str, width: int, color: str, align="<") -> str:
    return c(f"{text:{align}{width}}", color)


def truncate(s: str, width: int) -> str:
    return s if len(s) <= width else s[: max(0, width - 1)] + "…"


def clip_ansi(s: str, width: int) -> str:
    if visible_len(s) <= width:
        return s
    out, vis, i, coloured = [], 0, 0, False
    while i < len(s) and vis < width:
        m = ANSI_RE.match(s, i)
        if m:
            out.append(m.group(0))
            coloured = True
            i = m.end()
        else:
            out.append(s[i])
            vis += 1
            i += 1
    if coloured or ANSI_RE.search(s, i):
        out.append(COLORS["reset"])
    return "".join(out)


def pack_fragments(frags: List[str], width: int) -> List[str]:
    lines, cur = [], ""
    for f in frags:
        if cur and visible_len(cur) + 3 + visible_len(f) > width:
            lines.append(cur)
            cur = f
        else:
            cur = f"{cur}   {f}" if cur else f
    if cur:
        lines.append(cur)
    return lines


def too_small_notice(snap, cols: int, rows: int) -> List[str]:
    """Friendly guard when the window is too small for the dashboard —
    the migration keeps running in the background regardless."""
    from .. import banner_line
    a = snap["agg"]
    msg = [
        banner_line(),
        "",
        c("Terminal too small to draw the dashboard.", "yellow"),
        f"Please enlarge to at least {MIN_COLS}x{MIN_ROWS} "
        f"(currently {cols}x{rows}).",
        "",
        "The migration is still running in the background —",
        f"{a['msgs_done']:,} msgs / {fmt_bytes(a['bytes_done'])} synced so far.",
        "",
        c("Ctrl+C stops gracefully.", "dim"),
    ]
    lines = [""] * max(0, (rows - len(msg)) // 2)
    for m in msg:
        pad = max(0, (cols - visible_len(m)) // 2)
        lines.append(" " * pad + m)
    return lines[: max(1, rows - 1)]


def shutdown_dialog(hub, cols: int, rows: int) -> List[str]:
    """Centred, branded graceful-shutdown dialog with a soft shadow. Phase
    lines update live as each shutdown task completes."""
    from .. import PRODUCT, SLOGAN
    from ..control import SHUTDOWN_PHASES

    elapsed = int(time.time() - hub.shutdown_started) if hub.shutdown_started else 0
    spin = _SPINNER[(elapsed * 4) % len(_SPINNER)] if not _all_done(hub) else "✓"
    title = (f"Force-stopping {PRODUCT}" if hub.shutdown_forced
             else f"Gracefully Stopping {PRODUCT}")

    # (text, colour, centered?)
    body: List[tuple] = [("", "white", False), (title, "bold", True),
                         (SLOGAN, "dim", True), ("", "white", False)]
    icons = {"pending": (" ", "dim"), "active": (spin, "yellow"),
             "done": ("✓", "green")}
    for key, label in SHUTDOWN_PHASES:
        state = hub.phase_state.get(key, "pending")
        mark, colr = icons[state]
        text = label + ("..." if state == "active" else "")
        body.append((f" {mark}  {text}", colr if state != "pending" else "dim", False))
    body += [("", "white", False),
             ("Please wait. Do not close this terminal.", "yellow", True),
             (f"elapsed {fmt_dhms(elapsed)}", "dim", True), ("", "white", False)]

    inner_w = max(46, min(cols - 8, 60))
    top = "┌" + "─" * (inner_w + 2) + "┐"
    bot = "└" + "─" * (inner_w + 2) + "┘"
    box = [top]
    for text, colr, centered in body:
        vis = visible_len(text)
        left = max(1, (inner_w - vis) // 2) if centered else 1
        right = inner_w - vis - left
        span = c(text, colr) if (colr != "white" and text.strip()) else text
        box.append("│ " + " " * left + span + " " * right + " │")
    box.append(bot)

    # soft shadow: offset duplicate in dim
    shadowed = list(box)
    shadowed.append(" " + c("░" * (inner_w + 4), "dim"))
    box_w = inner_w + 4
    left_pad = max(0, (cols - box_w) // 2)
    top_pad = max(0, (rows - len(shadowed)) // 2)
    out = [""] * top_pad
    for i, line in enumerate(box):
        shadow_tail = c("░", "dim") if i > 0 else ""
        out.append(" " * left_pad + line + shadow_tail)
    out.append(" " * (left_pad + 1) + c("░" * box_w, "dim"))
    return out[: max(1, rows - 1)]


def _all_done(hub) -> bool:
    return all(v == "done" for v in hub.phase_state.values())


def lock_dialog(hub, cols: int, rows: int) -> List[str]:
    """Centred stale-instance-lock dialog. Heartbeat age updates live; if
    the 'dead' instance heartbeats while the dialog is open, the hub
    auto-cancels it (a live instance is never reset)."""
    p = hub.lock_prompt
    if p is None:
        return [""]
    now = time.time()
    age = max(0, int(now - p["ts_seen"]))
    remain = max(0, int(p["deadline"] - now))
    owner = p["owner"]
    bits = owner.split(":", 2) + ["", "", ""]
    host, pid, started = bits[0] or "?", bits[1] or "?", bits[2] or "?"
    labels = p["labels"]
    dead = age >= hub.LOCK_LIVE_GRACE
    assess = ("missed 2+ heartbeats — instance appears DEAD" if dead
              else f"heartbeat only {age}s old — may be ALIVE")

    body: List[tuple] = [
        ("", "white", False),
        ("⚠  Locked by Another MailFerry Instance", "bold", True),
        ("", "white", False),
        (f"Mailbox {labels[0]}" + (f"  (+{len(labels) - 1} more)" if len(labels) > 1 else ""),
         "white", False),
        ("is locked by a previous MailFerry instance:", "white", False),
        ("", "white", False),
        (f"  Host           {host}", "cyan", False),
        (f"  PID            {pid}", "cyan", False),
        (f"  Run started    {started}", "cyan", False),
        (f"  Last heartbeat {age}s ago (expected every 15s)",
         "yellow" if not dead else "red", False),
        (f"  Assessment     {assess}", "red" if dead else "yellow", False),
        ("", "white", False),
        ("This happens after a crash, force-quit or power loss.", "dim", False),
        ("A LIVE instance is never reset — the takeover is verified", "dim", False),
        ("against the heartbeat and applied atomically.", "dim", False),
        ("", "white", False),
    ]
    if p.get("detail"):
        body += [(f"  Instance id    {truncate(owner, 44)}", "dim", False)]
        for lb in labels[:4]:
            body.append((f"  Affected       {truncate(lb, 44)}", "dim", False))
        if len(labels) > 4:
            body.append((f"                 … +{len(labels) - 4} more", "dim", False))
        body.append(("", "white", False))
    if p.get("note"):
        body += [(truncate(p["note"], 56), "yellow", True), ("", "white", False)]
    opts = (("[R] Reset stale lock & continue  " if dead
             else "[R] unavailable (recent)  ")
            + "[D] Details  " + f"[C] Cancel ({remain}s)")
    body += [(opts, "bold" if dead else "dim", True), ("", "white", False)]

    inner_w = max(56, min(cols - 8, 64))
    top = "┌" + "─" * (inner_w + 2) + "┐"
    bot = "└" + "─" * (inner_w + 2) + "┘"
    box = [top]
    for text, colr, centered in body:
        text = truncate(text, inner_w - 2)
        vis = visible_len(text)
        left = max(1, (inner_w - vis) // 2) if centered else 1
        right = max(0, inner_w - vis - left)
        span = c(text, colr) if (colr != "white" and text.strip()) else text
        box.append("│ " + " " * left + span + " " * right + " │")
    box.append(bot)
    box_w = inner_w + 4
    left_pad = max(0, (cols - box_w) // 2)
    top_pad = max(0, (rows - len(box) - 1) // 2)
    out = [""] * top_pad
    for i, line in enumerate(box):
        shadow_tail = c("░", "dim") if i > 0 else ""
        out.append(" " * left_pad + line + shadow_tail)
    out.append(" " * (left_pad + 1) + c("░" * box_w, "dim"))
    return out[: max(1, rows - 1)]


class LiveView:
    """Alternate-screen, per-row differential repaint (wrapper technique)."""

    def __init__(self):
        self.prev_lines: Optional[List[str]] = None
        self.prev_size = None
        self.active = False

    def update(self, lines: List[str], cursor=None):
        if not ISATTY:
            return
        cols, rows = shutil.get_terminal_size(fallback=(100, 24))
        lines = [clip_ansi(l, cols) for l in lines[: max(1, rows - 1)]]
        buf = []
        if not self.active:
            buf.append("\033[?1049h")
            self.active = True
            self.prev_lines = None
            self.prev_size = None
        buf.append("\033[?25l")
        if (cols, rows) != self.prev_size:
            buf.append("\033[2J")
            self.prev_size = (cols, rows)
            self.prev_lines = None
        buf.append("\033[H")
        prev = self.prev_lines
        if prev is not None and len(prev) == len(lines):
            buf.extend("\n" if old == new else new + "\033[K\n"
                       for old, new in zip(prev, lines))
        else:
            buf.extend(line + "\033[K\n" for line in lines)
        buf.append("\033[J")
        if cursor is not None:
            row = min(cursor[0], len(lines))
            col = min(cursor[1], cols)
            buf.append(f"\033[{row};{col}H\033[?25h")
        self.prev_lines = list(lines)
        sys.stdout.write("".join(buf))
        sys.stdout.flush()

    def close(self, final_lines: Optional[List[str]] = None):
        if ISATTY and self.active:
            sys.stdout.write("\033[?1049l\033[?25h")
            self.active = False
        if final_lines:
            sys.stdout.write("\n".join(final_lines) + "\n")
        sys.stdout.flush()


class Renderer(threading.Thread):
    def __init__(self, stats, sysmon=None, json_progress_path: str = "", hub=None):
        super().__init__(daemon=True, name="mf-render")
        self.stats = stats
        self.sysmon = sysmon
        self.hub = hub
        self.console = None                 # legacy line-console (unused with TUI)
        self.tui = None                     # set by the CLI when the TUI is active
        self.wake = threading.Event()       # keystrokes wake the renderer
        self.view = LiveView()
        self._stop = threading.Event()
        self.global_rate = RateTracker()     # payload — ETA accuracy
        self.global_wire = RateTracker()     # wire — live throughput display
        self.mb_rates: Dict[int, RateTracker] = {}
        self.mb_wire: Dict[int, RateTracker] = {}
        self.jsonf = open(json_progress_path, "a", encoding="utf-8") if json_progress_path else None

    def stop(self):
        self._stop.set()
        self.wake.set()

    def run(self):
        interval = 0.25 if ISATTY else 5.0
        last_json = 0.0
        while not self._stop.is_set():
            try:
                if ISATTY and self.tui is not None:
                    lines, cursor = self.tui.frame()
                    self.view.update(lines, cursor)
                elif ISATTY:
                    snap = self.stats.snapshot()
                    self._tick_rates(snap)
                    lines, cursor = self.render(snap, fit=True)
                    self.view.update(lines, cursor)
                else:
                    snap = self.stats.snapshot()
                    self._tick_rates(snap)
                    print(self.status_line(snap), flush=True)
                if self.jsonf and time.time() - last_json >= 1.0:
                    self.jsonf.write(json.dumps(self._json_tick(snap)) + "\n")
                    self.jsonf.flush()
                    last_json = time.time()
            except Exception:
                pass
            iv = self.tui.refresh if (self.tui is not None and ISATTY) else interval
            self.wake.wait(iv)
            self.wake.clear()

    def close(self, final=True):
        self.stop()
        try:
            snap = self.stats.snapshot()
            if final:
                lines, _ = self.render(snap, fit=False, with_console=False)
                self.view.close(lines)
            else:
                self.view.close()
        except Exception:
            self.view.close()
        if self.jsonf:
            try:
                self.jsonf.close()
            except Exception:
                pass

    # ------------------------------------------------------------- data --

    def _tick_rates(self, snap):
        t = snap["ts"]
        agg = snap["agg"]
        self.global_rate.update(t, agg["bytes_done"], agg["msgs_done"])
        self.global_wire.update(t, max(agg["wire_rx"], agg["wire_tx"]), 0)
        for m in snap["mailboxes"]:
            rt = self.mb_rates.setdefault(m["index"], RateTracker())
            rt.update(t, m["bytes_done"], m["msgs_done"])
            wt = self.mb_wire.setdefault(m["index"], RateTracker())
            wt.update(t, max(m["src"]["rx"] + m["src"]["tx"],
                             m["dst"]["rx"] + m["dst"]["tx"]), 0)
        if self.hub is not None:
            self.hub.rates = self._display_rates()
            self.hub.eta = self._eta(snap)

    def _display_rates(self):
        return (max(self.global_wire.rates()[0], self.global_rate.rates()[0]),
                self.global_rate.rates()[1])

    def _eta(self, snap):
        agg = snap["agg"]
        rem_b = max(0, agg["bytes_total"] - agg["bytes_done"])
        eta = self.global_rate.eta(rem_b,
                                   max(0, agg["msgs_total"] - agg["msgs_done"]))
        if eta is None and rem_b > 0:
            wr = self.global_wire.rates()[0]
            if wr > 1024:
                eta = rem_b / wr
        return eta

    def _json_tick(self, snap):
        br, mr = self._display_rates()
        return {"ts": snap["ts"], "agg": snap["agg"], "counts": snap["counts"],
                "rate_bytes_s": round(br), "rate_msgs_s": round(mr, 2),
                "eta_s": self._eta(snap), "mailboxes": snap["mailboxes"]}

    # ---------------------------------------------------------- non-TTY --

    def status_line(self, snap) -> str:
        agg, counts = snap["agg"], snap["counts"]
        total = len(snap["mailboxes"])
        done = sum(counts.get(s, 0) for s in ("SUCCESS", "PARTIAL", "FAILED",
                                              "CANCELLED", "STALE", "WARNINGS"))
        br, mr = self._display_rates()
        eta = self._eta(snap)
        parts = [
            f"running={counts.get('RUNNING', 0)}",
            f"queued={counts.get('QUEUED', 0)}",
            f"done={done}/{total}",
            f"ok={counts.get('SUCCESS', 0)}",
            f"partial={counts.get('PARTIAL', 0)}",
            f"failed={counts.get('FAILED', 0)}",
        ]
        if snap["skipped_prior"]:
            parts.append(f"skipped={snap['skipped_prior']}")
        parts += [
            f"msgs={agg['msgs_done']:,}/{agg['msgs_total']:,}({pct(agg['msgs_done'], agg['msgs_total'])})",
            f"data={fmt_bytes(agg['bytes_done'])}/{fmt_bytes(agg['bytes_total'])}({pct(agg['bytes_done'], agg['bytes_total'])})",
            f"rate={fmt_bytes(br)}/s",
            f"new={agg['appended']}", f"adopted={agg['adopted']}",
            f"rec={agg['reconnects']}",
            f"eta={fmt_dhms(eta) if eta else '-'}",
            f"runtime={fmt_dhms(time.time() - snap['batch_start'])}",
        ]
        return f"[{datetime.now().isoformat(timespec='seconds')}] " + "  ".join(parts)

    # -------------------------------------------------------------- TTY --

    def _mb_rate(self, index) -> float:
        """Per-mailbox live byte rate — wire-based so it ticks continuously
        while data streams. Sourced from the TUI's trackers when the TUI is
        active (they are fed every frame), else from our own."""
        if self.tui is not None:
            return self.tui.mb_rate(index)
        wt = self.mb_wire.get(index)
        rt = self.mb_rates.get(index)
        return max(wt.rates()[0] if wt else 0.0, rt.rates()[0] if rt else 0.0)

    def render(self, snap, fit=True, with_console=True, reserve=0):
        cols, rows = shutil.get_terminal_size(fallback=(110, 30))
        # Graceful-shutdown dialog takes over the screen (dashboard pauses).
        if fit and self.hub is not None and self.hub.shutdown_active:
            return shutdown_dialog(self.hub, cols, rows), None
        # Friendly guard for terminals too small to render the dashboard.
        if fit and (cols < MIN_COLS or rows < MIN_ROWS):
            return too_small_notice(snap, cols, rows), None
        W = max(70, min(cols - 2, 120))
        agg, counts = snap["agg"], snap["counts"]
        mbs = [m for m in snap["mailboxes"]]
        now = time.time()

        # This is the legacy non-TUI dashboard, retained as a fallback frame
        # (e.g. the final static frame printed to scrollback on exit). The
        # interactive experience is the TUI (mailferry/tui/); no command
        # console area is drawn here.
        console_lines: List[str] = []
        cursor_col = 0

        lines = ["╔" + "═" * W + "╗",
                 "║" + banner_line().center(W) + "║",
                 "║" + SLOGAN.center(W) + "║",
                 "╚" + "═" * W + "╝"]
        busy = counts.get("RUNNING", 0) + counts.get("RETRYING", 0)
        mode_val = snap["mode"]
        if self.hub is not None and self.hub.paused:
            mode_val += "  [PAUSED]"
        elif snap["interrupted"]:
            mode_val += "  [STOPPING — finishing current batches]"
        left = [f"{'CSV File':<15}: {snap['csv']}",
                f"{'State Database':<15}: {snap['db']}",
                f"{'Logs':<15}: {snap['logs']}",
                f"{'Mode':<15}: {mode_val}",
                f"{'Workers':<15}: {busy}/{snap['workers']}",
                f"{'Runtime':<15}: {fmt_dhms(now - snap['batch_start'])}"]
        if snap["skipped_prior"]:
            left.insert(1, f"{'Skipped':<15}: {snap['skipped_prior']} previously completed")
        right = []
        if self.sysmon:
            s = self.sysmon.snapshot()
            right.append(f"{'CPU':<12}: " + (f"{s['cpu']:.0f}%" if s.get("cpu") is not None else "N/A"))
            load = s.get("load")
            right.append(f"{'Load Avg':<12}: " + (f"{load[0]:.2f} {load[1]:.2f} {load[2]:.2f}" if load else "N/A"))
            mt, mu = s.get("mem_total"), s.get("mem_used")
            right.append(f"{'Memory':<12}: " + (f"{fmt_bytes(mu)} / {fmt_bytes(mt)}" if mt and mu else
                                                (f"total {fmt_bytes(mt)}" if mt else "N/A")))
            right.append(f"{'Process RSS':<12}: " + (fmt_bytes(s["rss"]) if s.get("rss") else "N/A"))
            right.append(f"{'Wire RX/TX':<12}: {fmt_bytes(agg['wire_rx'])} / {fmt_bytes(agg['wire_tx'])}")
        if right:
            rw = max(visible_len(r) for r in right)
            lw = W + 2 - rw - 3
            if lw >= 32:
                for i in range(max(len(left), len(right))):
                    l = truncate(left[i], lw) if i < len(left) else ""
                    r = right[i] if i < len(right) else ""
                    lines.append(f"{l:<{lw}}   {r}".rstrip())
            else:
                lines.extend(left)
                lines.extend(pack_fragments(right, W + 2))
        else:
            lines.extend(left)
        lines.append("─" * (W + 2))

        IDX_W, ST_W, FL_W, MS_W, PCT_W, SP_W, EL_W = 3, 8, 7, 17, 5, 11, 16
        fixed = IDX_W + 2 + ST_W + 1 + FL_W + 1 + MS_W + 1 + PCT_W + 1 + SP_W + 1 + EL_W
        mw = max(14, W - fixed)
        header = (f"{'#':>{IDX_W}}  {'Mailbox':<{mw}} {'Status':<{ST_W}} {'Fldr':<{FL_W}} "
                  f"{'Msgs':<{MS_W}} {'Pct':>{PCT_W}} {'Speed':>{SP_W}} {'Elapsed':>{EL_W}}")
        lines.append(header)
        lines.append("─" * (W + 2))

        def group(m, level):
            br = self._mb_rate(m["index"])
            done_all = m["status"] in ("SUCCESS", "PARTIAL", "FAILED", "CANCELLED",
                                       "STALE", "WARNINGS")
            active = m["status"] in ("RUNNING", "RETRYING", "REMOTE")
            elapsed = "-"
            if m["start"]:
                elapsed = fmt_dhms((m["end"] or now) - m["start"])
            p = pct(m["bytes_done"], m["bytes_total"]) if m["bytes_total"] else pct(m["msgs_done"], m["msgs_total"])
            fl = f"{m['fi']}/{m['ft']}" if m["ft"] else "-"
            msgs = f"{m['msgs_done']:,}/{m['msgs_total']:,}" if m["msgs_total"] else "-"
            # live throughput while active (0 B/s = connected but idle);
            # "-" before the mailbox starts and after it finishes
            speed = f"{fmt_bytes(br)}/s" if active else "-"
            st_word = ("RECOVER" if m.get("recovering") and m["status"] == "RUNNING"
                       else m["status"])
            g = [(f"{m['index']:>{IDX_W}}  {truncate(m['label'], mw):<{mw}} "
                  f"{cpad(st_word, ST_W, STATUS_COLOR.get(st_word, 'white'))} "
                  f"{fl:<{FL_W}} {msgs:<{MS_W}} {p:>{PCT_W}} {speed:>{SP_W}} {elapsed:>{EL_W}}")]
            if m["status"] == "RETRYING" and level >= 1:
                wait = max(0, int(m["retry_wait_until"] - now))
                g.append("      " + c(truncate(
                    f"retrying in {wait}s (attempt {min(m['attempt'] + 1, m['max_attempts'])}/"
                    f"{m['max_attempts']}) — last error: {m['error']}", W - 6), "yellow"))
                return g
            if m["status"] == "REMOTE":
                if level >= 1:
                    bw = max(10, min(30, W - 60))
                    total = m["bytes_total"] or m["msgs_total"]
                    val = m["bytes_done"] if m["bytes_total"] else m["msgs_done"]
                    frac = min(1.0, val / total) if total else 0.0
                    bar = "█" * int(bw * frac) + "░" * (bw - int(bw * frac))
                    g.append(f"      {'(remote)':<20} {bar} {pct(val, total):>4}  "
                             + c(truncate(m["op"] or "on another worker", W - 60), "cyan"))
                return g
            if m["status"] != "RUNNING":
                if m["status"] == "WARNINGS" and level >= 1:
                    nf = m.get("failed", 0) + m["skipped"]
                    tot = m["msgs_total"] or (m["msgs_done"] + nf)
                    pw = (m["msgs_done"] * 100.0 / tot) if tot else 100.0
                    g.append("      " + c(
                        truncate(f"completed with warnings — {m['msgs_done']:,}/{tot:,} "
                                 f"migrated · {nf} failed ({pw:.2f}% complete) — "
                                 "mailferry failed / retry-failed", W - 6), "yellow"))
                elif m["status"] in ("FAILED", "PARTIAL", "STALE") and m["error"] and level >= 1:
                    g.append("      " + c(truncate(m["error"], W - 6),
                                          "yellow" if m["status"] == "PARTIAL" else "red"))
                return g
            if level >= 1:
                bw = max(10, min(30, W - 60))
                total = m["bytes_total"] or m["msgs_total"]
                val = m["bytes_done"] if m["bytes_total"] else m["msgs_done"]
                frac = min(1.0, val / total) if total else 0.0
                bar = "█" * int(bw * frac) + "░" * (bw - int(bw * frac))
                fol = truncate(m["folder"] or "-", 20)
                g.append(f"      {fol:<20} {bar} {pct(val, total):>4}  {c(truncate(m['op'], W - 60), 'cyan')}")
            if level >= 2:
                sr, ds = m["src"], m["dst"]
                sline = (f"{'Source':<12}{truncate(sr['host'], 24)} [{','.join(sr['caps'])}] "
                         f"{sr['state']}  ↓{fmt_bytes(sr['rx'])}"
                         + (f"  reconnects {sr['reconnects']}" if sr["reconnects"] else ""))
                dline = (f"{'Destination':<12}{truncate(ds['host'], 24)} [{','.join(ds['caps'])}] "
                         f"{ds['state']}  ↑{fmt_bytes(ds['tx'])}  pre-existing {ds['existing']:,}"
                         f"  new {m['appended']:,}  adopted {m['adopted']:,}"
                         + (f"  quota {ds['quota']:.0f}%" if ds["quota"] >= 0 else ""))
                g.append("      " + c(truncate(sline, W - 6), "dim"))
                g.append("      " + c(truncate(dline, W - 6), "dim"))
            if level >= 3 and m["detail"]:
                g.append("      " + c(truncate(m["detail"], W - 6), "dim"))
            stall = ""
            return g

        tail = ["─" * (W + 2)]
        total = len(mbs)
        done = sum(counts.get(s, 0) for s in ("SUCCESS", "PARTIAL", "FAILED",
                                              "CANCELLED", "STALE", "WARNINGS"))
        frag1 = [f"Done {done}/{total}",
                 c(f"{counts.get('SUCCESS', 0)} ok", "green"),
                 c(f"{counts.get('PARTIAL', 0)} partial", "yellow"),
                 c(f"{counts.get('FAILED', 0)} failed", "red")]
        if counts.get("WARNINGS"):
            frag1.append(c(f"{counts.get('WARNINGS')} with warnings", "yellow"))
        if counts.get("STALE"):
            frag1.append(c(f"{counts.get('STALE')} stale", "red"))
        if counts.get("REMOTE"):
            frag1.append(c(f"{counts.get('REMOTE')} on other workers", "cyan"))
        if counts.get("CANCELLED"):
            frag1.append(c(f"{counts.get('CANCELLED')} cancelled", "yellow"))
        if snap["skipped_prior"]:
            frag1.append(c(f"{snap['skipped_prior']} skipped", "cyan"))
        tail.extend(pack_fragments(frag1, W + 2))
        if self.tui is not None:
            br, mr = self.tui.rates()
            eta = self.hub.eta if self.hub is not None else None
        else:
            br, mr = self._display_rates()
            eta = self._eta(snap)
        frag2 = [f"Msgs {agg['msgs_done']:,}/{agg['msgs_total']:,} ({pct(agg['msgs_done'], agg['msgs_total'])})",
                 f"Data {fmt_bytes(agg['bytes_done'])}/{fmt_bytes(agg['bytes_total'])} "
                 f"({pct(agg['bytes_done'], agg['bytes_total'])})",
                 f"Rate {fmt_bytes(br)}/s ({mr:.1f} msg/s)",
                 f"New {agg['appended']:,}", f"Adopted {agg['adopted']:,}"]
        if agg["skipped_msgs"]:
            frag2.append(c(f"MsgSkip {agg['skipped_msgs']:,}", "yellow"))
        if agg.get("failed_msgs"):
            frag2.append(c(f"MsgFail {agg['failed_msgs']:,}", "red"))
        frag2 += [f"Reconn {agg['reconnects']}", f"Retries {agg['retries']}"]
        st = snap.get("stalls") or {}
        if st.get("detected"):
            frag2.append(c(f"Stalls {st['detected']} (rec {st.get('recovered', 0)})",
                           "yellow" if not st.get("failed") else "red"))
        tail.extend(pack_fragments(frag2, W + 2))
        frag3 = [f"Batch ETA: {fmt_dhms(eta) if eta else '-'}"]
        if eta:
            fin = datetime.now() + timedelta(seconds=eta)
            frag3.append("Finish ~" + (fin.strftime("%H:%M") if eta < 86400 else fin.strftime("%d %b %H:%M")))
        tail.extend(pack_fragments(frag3, W + 2))

        level = 3
        groups = [group(m, level) for m in mbs]
        if fit:
            budget = max(8, rows - 1 - reserve - len(console_lines))

            def h():
                return len(lines) + sum(len(g) for g in groups) + len(tail)
            while level > 0 and h() > budget:
                level -= 1
                groups = [group(m, level) for m in mbs]
            if h() > budget:
                # prefer active mailboxes when eliding
                order = sorted(range(len(mbs)),
                               key=lambda i: (0 if mbs[i]["status"] in ("RUNNING", "RETRYING") else 1, i))
                keep, used = set(), len(lines) + len(tail) + 1
                for i in order:
                    if used + len(groups[i]) > budget:
                        continue
                    keep.add(i)
                    used += len(groups[i])
                hidden = len(mbs) - len(keep)
                groups = [groups[i] for i in sorted(keep)]
                if hidden:
                    groups.append([c(f"   … {hidden} more mailbox(es) not shown — "
                                     "enlarge the window to see all", "dim")])
        dash = lines + [l for g in groups for l in g] + tail
        if console_lines:
            all_lines = dash + console_lines
            return all_lines, (len(all_lines), cursor_col)
        return dash, None
