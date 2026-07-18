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

Reusable TUI widgets: bordered panels, selectable/scrollable tables,
progress bars, sparklines and centred overlays. All rendering is pure
string composition (ANSI colour via the shared dashboard helpers); no
widget ever touches the terminal directly.
"""
from __future__ import annotations

from typing import List, Optional, Sequence, Tuple

from ..progress.dashboard import c, clip_ansi, truncate, visible_len

BAR_FULL = "█"
BAR_EMPTY = "░"
SPARK = "▁▂▃▄▅▆▇█"


def progress_bar(frac: float, width: int) -> str:
    width = max(1, width)
    frac = max(0.0, min(1.0, frac))
    filled = int(round(width * frac))
    return BAR_FULL * filled + BAR_EMPTY * (width - filled)


def sparkline(values: Sequence[float], width: int) -> str:
    if not values or width <= 0:
        return " " * max(0, width)
    vals = list(values)[-width:]
    lo, hi = min(vals), max(vals)
    rng = hi - lo
    out = []
    for v in vals:
        if rng <= 0:
            out.append(SPARK[0])
        else:
            idx = int((v - lo) / rng * (len(SPARK) - 1))
            out.append(SPARK[max(0, min(len(SPARK) - 1, idx))])
    s = "".join(out)
    return s.rjust(width)[:width]


def panel(title: str, body: List[str], width: int, height: Optional[int] = None,
          accent: str = "cyan") -> List[str]:
    """A bordered panel with a title in the top edge. Body lines are clipped
    to the interior width; the panel is padded/truncated to `height` if set."""
    inner = max(4, width - 2)
    tlabel = truncate(title, inner - 2)
    top = "┌─" + c(tlabel, accent) + "─" * (inner - visible_len(tlabel) - 1) + "┐"
    out = [top]
    rows = body if height is None else body[: max(0, height - 2)]
    for line in rows:
        out.append("│" + _fit(line, inner) + "│")
    if height is not None:
        while len(out) < height - 1:
            out.append("│" + " " * inner + "│")
    out.append("└" + "─" * inner + "┘")
    return out


def _fit(s: str, width: int) -> str:
    v = visible_len(s)
    if v > width:
        return clip_ansi(s, width)
    return s + " " * (width - v)


def fit_cell(text: str, width: int, align: str = "<") -> str:
    """ANSI-safe cell fitting: measure by VISIBLE width, truncate without
    ever splitting an escape sequence, pad to exactly `width` columns.
    (A len()-based cut can emit a broken CSI that eats following characters
    on real terminals — the classic missing-first-letter bug.)"""
    v = visible_len(text)
    if v > width:
        text = clip_ansi(text, max(0, width - 1)) + "…"
        v = width
    pad = width - v
    if align == ">":
        return " " * pad + text
    if align == "^":
        left = pad // 2
        return " " * left + text + " " * (pad - left)
    return text + " " * pad


def row(cells: List[Tuple[str, int, str]], selected: bool = False,
        sel_color: str = "cyan") -> str:
    """Compose a table row from (text, width, align) cells. align in <,>,^."""
    line = " ".join(fit_cell(text, w, align) for text, w, align in cells)
    if selected:
        # strip inner colour so the highlight is uniform end to end
        import re
        plain = re.sub(r"\033\[[0-9;]*m", "", line)
        return c("▸ " + plain, sel_color)
    return "  " + line


class ScrollList:
    """Selection + viewport bookkeeping for a list of N items."""

    def __init__(self):
        self.sel = 0
        self.top = 0

    def clamp(self, n: int, view_h: int):
        if n <= 0:
            self.sel = 0
            self.top = 0
            return
        self.sel = max(0, min(self.sel, n - 1))
        if self.sel < self.top:
            self.top = self.sel
        elif self.sel >= self.top + view_h:
            self.top = self.sel - view_h + 1
        self.top = max(0, min(self.top, max(0, n - view_h)))

    def key(self, tok: str, n: int, view_h: int) -> bool:
        """Handle a navigation key; return True if consumed."""
        if tok == "up":
            self.sel -= 1
        elif tok == "down":
            self.sel += 1
        elif tok == "pgup":
            self.sel -= view_h
        elif tok == "pgdn":
            self.sel += view_h
        elif tok == "home":
            self.sel = 0
        elif tok == "end":
            self.sel = n - 1
        else:
            return False
        self.clamp(n, view_h)
        return True


def overlay(base: List[str], box: List[str], cols: int, rows: int) -> List[str]:
    """Composite a centred box (with soft shadow) over base frame lines."""
    out = [list(line) for line in base]
    # ensure canvas is rows x cols of plain chars is not needed; we splice by
    # replacing whole lines for simplicity (popups are full-width-safe).
    box_w = max(visible_len(l) for l in box)
    top = max(0, (rows - len(box)) // 2)
    left = max(0, (cols - box_w) // 2)
    result = list(base)
    # pad base to rows
    while len(result) < rows:
        result.append("")
    for i, bl in enumerate(box):
        r = top + i
        if r >= len(result):
            break
        base_line = result[r]
        # left segment of the base line (plain-trim), then the box line
        left_seg = _plain_left(base_line, left)
        result[r] = left_seg + bl
    return result


def _plain_left(s: str, width: int) -> str:
    """First `width` visible columns of s, padded with spaces (ANSI dropped
    for the spliced prefix to keep widths exact)."""
    import re
    plain = re.sub(r"\033\[[0-9;]*m", "", s)
    if len(plain) >= width:
        return plain[:width]
    return plain + " " * (width - len(plain))
