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
                "CANCELLED": "yellow", "SKIPPED": "cyan"}


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


class LiveView:
    """Alternate-screen, per-row differential repaint (wrapper technique)."""

    def __init__(self):
        self.prev_lines: Optional[List[str]] = None
        self.prev_size = None
        self.active = False

    def update(self, lines: List[str]):
        if not ISATTY:
            return
        cols, rows = shutil.get_terminal_size(fallback=(100, 24))
        lines = [clip_ansi(l, cols) for l in lines[: max(1, rows - 1)]]
        buf = []
        if not self.active:
            buf.append("\033[?1049h\033[?25l")
            self.active = True
            self.prev_lines = None
            self.prev_size = None
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
    def __init__(self, stats, sysmon=None, json_progress_path: str = ""):
        super().__init__(daemon=True, name="mf-render")
        self.stats = stats
        self.sysmon = sysmon
        self.view = LiveView()
        self._stop = threading.Event()
        self.global_rate = RateTracker()
        self.mb_rates: Dict[int, RateTracker] = {}
        self.jsonf = open(json_progress_path, "a", encoding="utf-8") if json_progress_path else None

    def stop(self):
        self._stop.set()

    def run(self):
        interval = 0.25 if ISATTY else 5.0
        while not self._stop.is_set():
            try:
                snap = self.stats.snapshot()
                self._tick_rates(snap)
                if ISATTY:
                    self.view.update(self.render(snap, fit=True))
                else:
                    print(self.status_line(snap), flush=True)
                if self.jsonf:
                    self.jsonf.write(json.dumps(self._json_tick(snap)) + "\n")
                    self.jsonf.flush()
            except Exception:
                pass
            self._stop.wait(interval)

    def close(self, final=True):
        self.stop()
        try:
            snap = self.stats.snapshot()
            self.view.close(self.render(snap, fit=False) if final else None)
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
        for m in snap["mailboxes"]:
            rt = self.mb_rates.setdefault(m["index"], RateTracker())
            rt.update(t, m["bytes_done"], m["msgs_done"])

    def _eta(self, snap):
        agg = snap["agg"]
        return self.global_rate.eta(max(0, agg["bytes_total"] - agg["bytes_done"]),
                                    max(0, agg["msgs_total"] - agg["msgs_done"]))

    def _json_tick(self, snap):
        br, mr = self.global_rate.rates()
        return {"ts": snap["ts"], "agg": snap["agg"], "counts": snap["counts"],
                "rate_bytes_s": round(br), "rate_msgs_s": round(mr, 2),
                "eta_s": self._eta(snap), "mailboxes": snap["mailboxes"]}

    # ---------------------------------------------------------- non-TTY --

    def status_line(self, snap) -> str:
        agg, counts = snap["agg"], snap["counts"]
        total = len(snap["mailboxes"])
        done = sum(counts.get(s, 0) for s in ("SUCCESS", "PARTIAL", "FAILED", "CANCELLED"))
        br, mr = self.global_rate.rates()
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

    def render(self, snap, fit=True) -> List[str]:
        cols, rows = shutil.get_terminal_size(fallback=(110, 30))
        W = max(70, min(cols - 2, 120))
        agg, counts = snap["agg"], snap["counts"]
        mbs = [m for m in snap["mailboxes"]]
        now = time.time()

        lines = ["╔" + "═" * W + "╗",
                 "║" + banner_line().center(W) + "║",
                 "║" + SLOGAN.center(W) + "║",
                 "╚" + "═" * W + "╝"]
        busy = counts.get("RUNNING", 0) + counts.get("RETRYING", 0)
        left = [f"{'CSV File':<15}: {snap['csv']}",
                f"{'State Database':<15}: {snap['db']}",
                f"{'Logs':<15}: {snap['logs']}",
                f"{'Mode':<15}: {snap['mode']}",
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
            rt = self.mb_rates.get(m["index"])
            br, _ = rt.rates() if rt else (0.0, 0.0)
            done_all = m["status"] in ("SUCCESS", "PARTIAL", "FAILED", "CANCELLED")
            elapsed = "-"
            if m["start"]:
                elapsed = fmt_dhms((m["end"] or now) - m["start"])
            p = pct(m["bytes_done"], m["bytes_total"]) if m["bytes_total"] else pct(m["msgs_done"], m["msgs_total"])
            fl = f"{m['fi']}/{m['ft']}" if m["ft"] else "-"
            msgs = f"{m['msgs_done']:,}/{m['msgs_total']:,}" if m["msgs_total"] else "-"
            speed = f"{fmt_bytes(br)}/s" if (br > 0 and not done_all) else ("-" if not done_all else "")
            g = [(f"{m['index']:>{IDX_W}}  {truncate(m['label'], mw):<{mw}} "
                  f"{cpad(m['status'], ST_W, STATUS_COLOR.get(m['status'], 'white'))} "
                  f"{fl:<{FL_W}} {msgs:<{MS_W}} {p:>{PCT_W}} {speed:>{SP_W}} {elapsed:>{EL_W}}")]
            if m["status"] == "RETRYING" and level >= 1:
                wait = max(0, int(m["retry_wait_until"] - now))
                g.append("      " + c(truncate(
                    f"retrying in {wait}s (attempt {min(m['attempt'] + 1, m['max_attempts'])}/"
                    f"{m['max_attempts']}) — last error: {m['error']}", W - 6), "yellow"))
                return g
            if m["status"] != "RUNNING":
                if m["status"] in ("FAILED", "PARTIAL") and m["error"] and level >= 1:
                    g.append("      " + c(truncate(m["error"], W - 6), "red" if m["status"] == "FAILED" else "yellow"))
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
        done = sum(counts.get(s, 0) for s in ("SUCCESS", "PARTIAL", "FAILED", "CANCELLED"))
        frag1 = [f"Done {done}/{total}",
                 c(f"{counts.get('SUCCESS', 0)} ok", "green"),
                 c(f"{counts.get('PARTIAL', 0)} partial", "yellow"),
                 c(f"{counts.get('FAILED', 0)} failed", "red")]
        if counts.get("CANCELLED"):
            frag1.append(c(f"{counts.get('CANCELLED')} cancelled", "yellow"))
        if snap["skipped_prior"]:
            frag1.append(c(f"{snap['skipped_prior']} skipped", "cyan"))
        tail.extend(pack_fragments(frag1, W + 2))
        br, mr = self.global_rate.rates()
        eta = self._eta(snap)
        frag2 = [f"Msgs {agg['msgs_done']:,}/{agg['msgs_total']:,} ({pct(agg['msgs_done'], agg['msgs_total'])})",
                 f"Data {fmt_bytes(agg['bytes_done'])}/{fmt_bytes(agg['bytes_total'])} "
                 f"({pct(agg['bytes_done'], agg['bytes_total'])})",
                 f"Rate {fmt_bytes(br)}/s ({mr:.1f} msg/s)",
                 f"New {agg['appended']:,}", f"Adopted {agg['adopted']:,}"]
        if agg["skipped_msgs"]:
            frag2.append(c(f"MsgSkip {agg['skipped_msgs']:,}", "yellow"))
        frag2 += [f"Reconn {agg['reconnects']}", f"Retries {agg['retries']}"]
        tail.extend(pack_fragments(frag2, W + 2))
        frag3 = [f"Batch ETA: {fmt_dhms(eta) if eta else '-'}"]
        if eta:
            fin = datetime.now() + timedelta(seconds=eta)
            frag3.append("Finish ~" + (fin.strftime("%H:%M") if eta < 86400 else fin.strftime("%d %b %H:%M")))
        tail.extend(pack_fragments(frag3, W + 2))

        level = 3
        groups = [group(m, level) for m in mbs]
        if fit:
            budget = max(8, rows - 1)

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
        return lines + [l for g in groups for l in g] + tail
