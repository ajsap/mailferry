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

Formatting, UID-set and fingerprint helpers (stdlib only).
"""
from __future__ import annotations

import email
import hashlib
import random
import re
import time
from typing import Iterable, List, Optional, Tuple

CRLF = b"\r\n"
Interval = Tuple[int, int]


def fmt_dhms(seconds) -> str:
    """Wrapper-compatible elapsed format: 'N day(s) HH:MM:SS'."""
    if seconds is None or seconds < 0:
        return "-"
    seconds = int(seconds)
    days, rem = divmod(seconds, 86400)
    hh, rem = divmod(rem, 3600)
    mm, ss = divmod(rem, 60)
    unit = "days" if days > 1 else "day"
    return f"{days} {unit} {hh:02d}:{mm:02d}:{ss:02d}"


def fmt_bytes(n) -> str:
    """1024-based bytes: B/KB/MB/GB/TB, one decimal for scaled units."""
    if n is None:
        return "-"
    size = float(n)
    if size < 1024.0:
        return f"{size:.0f} B"
    for unit in ("KB", "MB", "GB"):
        size /= 1024.0
        if size < 1024.0:
            return f"{size:.1f} {unit}"
    return f"{size / 1024.0:.1f} TB"


def safe_name(s: str) -> str:
    return re.sub(r"[^A-Za-z0-9_.@-]", "_", s or "")


def pct(done, total) -> str:
    if not total:
        return "-"
    return f"{min(100.0, done * 100.0 / total):.0f}%"


def backoff_delay(base: float, attempt: int, cap: float = 600.0) -> float:
    """Exponential backoff with jitter: base*2^(attempt-1), +-20%."""
    d = min(cap, max(1.0, base) * (2 ** max(0, attempt - 1)))
    return d * random.uniform(0.85, 1.2)


# --------------------------------------------------------------------------
# UID interval sets ([(lo, hi)] inclusive, sorted, non-overlapping)
# --------------------------------------------------------------------------

def to_intervals(uids: Iterable[int]) -> List[Interval]:
    out: List[Interval] = []
    for u in sorted(set(uids)):
        if out and u == out[-1][1] + 1:
            out[-1] = (out[-1][0], u)
        else:
            out.append((u, u))
    return out


def intervals_count(iv: List[Interval]) -> int:
    return sum(hi - lo + 1 for lo, hi in iv)


def intervals_diff(a: List[Interval], b: List[Interval]) -> List[Interval]:
    """Members of a not in b."""
    out: List[Interval] = []
    bi = 0
    for lo, hi in a:
        cur = lo
        while cur <= hi:
            while bi < len(b) and b[bi][1] < cur:
                bi += 1
            if bi >= len(b) or b[bi][0] > hi:
                out.append((cur, hi))
                break
            blo, bhi = b[bi]
            if blo > cur:
                out.append((cur, blo - 1))
            cur = bhi + 1
    # merge adjacents
    merged: List[Interval] = []
    for lo, hi in out:
        if merged and lo <= merged[-1][1] + 1:
            merged[-1] = (merged[-1][0], max(hi, merged[-1][1]))
        else:
            merged.append((lo, hi))
    return merged


def iter_uids(iv: List[Interval]):
    for lo, hi in iv:
        for u in range(lo, hi + 1):
            yield u


def set_strings(iv: List[Interval], max_chars: int = 600, max_uids: int = 1000):
    """Yield IMAP set strings covering the intervals, bounded in size."""
    parts: List[str] = []
    length = 0
    count = 0
    for lo, hi in iv:
        cur = lo
        while cur <= hi:
            take = min(hi, cur + (max_uids - count) - 1)
            frag = f"{cur}" if take == cur else f"{cur}:{take}"
            parts.append(frag)
            length += len(frag) + 1
            count += take - cur + 1
            cur = take + 1
            if length >= max_chars or count >= max_uids:
                yield ",".join(parts)
                parts, length, count = [], 0, 0
    if parts:
        yield ",".join(parts)


def parse_imap_set(s: str) -> List[Interval]:
    out: List[Interval] = []
    for part in s.split(","):
        part = part.strip()
        if not part:
            continue
        if ":" in part:
            a, b = part.split(":", 1)
            lo, hi = int(a), int(b)
            if lo > hi:
                lo, hi = hi, lo
            out.append((lo, hi))
        else:
            out.append((int(part), int(part)))
    out.sort()
    return out


# --------------------------------------------------------------------------
# Message fingerprint (dedup identity) — see design §10A
# --------------------------------------------------------------------------

_MSGID_CLEAN = re.compile(r"[<>\s]")


def fingerprint_from_headers(header_bytes: Optional[bytes], size: int) -> str:
    """fp = 'm:'+Message-ID, else 'h:'+sha256(Date|From|To|Subject|size)."""
    msg = None
    if header_bytes:
        try:
            msg = email.message_from_bytes(header_bytes)
        except Exception:
            msg = None
    if msg is not None:
        mid = (msg.get("Message-ID") or msg.get("Message-Id") or "").strip()
        if mid:
            return "m:" + _MSGID_CLEAN.sub("", mid)
        basis = "\x00".join((msg.get("Date") or "", msg.get("From") or "",
                             msg.get("To") or "", msg.get("Subject") or "", str(size)))
    else:
        basis = "\x00raw\x00" + str(size)
    return "h:" + hashlib.sha256(basis.encode("utf-8", "replace")).hexdigest()[:32]


class HeaderSniffer:
    """Accumulates the first chunks of a streamed message until the blank
    line, so a fingerprint can be computed at zero extra round trips."""

    LIMIT = 65536

    def __init__(self):
        self._buf = bytearray()
        self._done = False

    def feed(self, chunk: bytes):
        if self._done:
            return
        self._buf += chunk[: self.LIMIT - len(self._buf)]
        if b"\r\n\r\n" in self._buf or b"\n\n" in self._buf or len(self._buf) >= self.LIMIT:
            self._done = True

    def fingerprint(self, size: int) -> str:
        buf = bytes(self._buf)
        for sep in (b"\r\n\r\n", b"\n\n"):
            i = buf.find(sep)
            if i >= 0:
                buf = buf[:i]
                break
        return fingerprint_from_headers(buf if buf else None, size)


def now_iso() -> str:
    import datetime
    return datetime.datetime.now().isoformat(timespec="seconds")


def monotonic() -> float:
    return time.monotonic()
