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

Raw-mode keyboard reader and escape-sequence parser. Runs on its own thread
and normalises xterm / vt / linux-console / tmux / screen sequences into
stable key tokens. It never writes to the terminal — only the renderer does,
so keystrokes can never corrupt the display — and it never blocks the engine.

Ctrl+C is deliberately NOT captured here: ISIG stays enabled so the terminal
delivers SIGINT to the process (handled as graceful shutdown in the CLI).
"""
from __future__ import annotations

import os
import sys
import threading
from typing import Callable, Optional, Tuple

# Normalised tokens: 'up' 'down' 'left' 'right' 'pgup' 'pgdn' 'home' 'end'
# 'enter' 'esc' 'tab' 'shift_tab' 'space' 'backspace' 'delete' 'ctrl_l'
# 'f1'..'f12', or a single printable character (str of length 1).

_CSI_LETTER = {b"A": "up", b"B": "down", b"C": "right", b"D": "left",
               b"H": "home", b"F": "end", b"Z": "shift_tab"}
_CSI_TILDE = {b"1": "home", b"7": "home", b"4": "end", b"8": "end",
              b"5": "pgup", b"6": "pgdn", b"3": "delete", b"2": "insert",
              b"15": "f5", b"17": "f6", b"18": "f7", b"19": "f8",
              b"20": "f9", b"21": "f10", b"23": "f11", b"24": "f12"}
_SS3 = {b"P": "f1", b"Q": "f2", b"R": "f3", b"S": "f4",
        b"H": "home", b"F": "end",
        b"A": "up", b"B": "down", b"C": "right", b"D": "left"}
# linux console: ESC [ [ A..E -> F1..F5
_CSI_BRACKET = {b"A": "f1", b"B": "f2", b"C": "f3", b"D": "f4", b"E": "f5"}


def parse_key(data: bytes) -> Tuple[Optional[str], int]:
    """Parse one key from the head of `data`.

    Returns (token, bytes_consumed). token is None with consumed==0 when the
    buffer holds an incomplete escape sequence (caller should read more).
    """
    if not data:
        return None, 0
    b0 = data[0]
    if b0 != 0x1B:
        # non-escape byte(s)
        if b0 in (0x0D, 0x0A):
            return "enter", 1
        if b0 == 0x09:
            return "tab", 1
        if b0 == 0x7F or b0 == 0x08:
            return "backspace", 1
        if b0 == 0x0C:
            return "ctrl_l", 1
        if b0 == 0x20:
            return "space", 1
        if b0 < 0x20:
            return f"ctrl_{chr(b0 + 96)}", 1        # other control chars
        # printable — decode a full UTF-8 rune
        n = 1
        if b0 >= 0xF0:
            n = 4
        elif b0 >= 0xE0:
            n = 3
        elif b0 >= 0xC0:
            n = 2
        if len(data) < n:
            return None, 0
        try:
            return data[:n].decode("utf-8"), n
        except UnicodeDecodeError:
            return None, 1                          # skip the bad byte
    # --- escape sequences ---
    if len(data) == 1:
        return "esc", 1                             # lone ESC (best effort)
    b1 = data[1]
    if b1 == ord("["):
        # CSI
        if len(data) >= 3 and data[2] == ord("["):
            # linux-console F1..F5
            if len(data) < 4:
                return None, 0
            return _CSI_BRACKET.get(data[3:4], ""), 4
        j = 2
        while j < len(data) and (0x30 <= data[j] <= 0x3F):   # params 0-9 ; < = > ?
            j += 1
        if j >= len(data):
            return None, 0                          # incomplete
        final = data[j:j + 1]
        params = data[2:j].split(b";")[0]
        if final == b"~":
            return _CSI_TILDE.get(params, ""), j + 1
        tok = _CSI_LETTER.get(final)
        if tok:
            # CSI 1;2 A etc. (modified arrows) — treat as base key
            return tok, j + 1
        return "", j + 1                            # unknown CSI: swallow
    if b1 == ord("O"):
        if len(data) < 3:
            return None, 0
        return _SS3.get(data[2:3], ""), 3
    if b1 == 0x1B:
        return "esc", 1                             # ESC ESC -> one esc
    # ESC followed by a normal char (Alt+key): ignore the ESC, keep char
    return "esc", 1


class KeyReader(threading.Thread):
    """Reads stdin in raw mode and delivers normalised key tokens to a
    callback. Terminal state is always restored on stop()."""

    def __init__(self, on_key: Callable[[str], None]):
        super().__init__(daemon=True, name="mf-keys")
        self.on_key = on_key
        self._stop = False
        self._fd: Optional[int] = None
        self._saved = None

    def start(self) -> bool:
        if not sys.stdin.isatty():
            return False
        import termios
        self._fd = sys.stdin.fileno()
        try:
            self._saved = termios.tcgetattr(self._fd)
            mode = termios.tcgetattr(self._fd)
            # raw-ish: no echo, no canonical; KEEP ISIG so Ctrl+C -> SIGINT
            mode[3] &= ~(termios.ECHO | termios.ICANON)
            mode[6][termios.VMIN] = 1
            mode[6][termios.VTIME] = 0
            termios.tcsetattr(self._fd, termios.TCSADRAIN, mode)
        except termios.error:
            return False
        super().start()
        return True

    def run(self):
        pend = b""
        while not self._stop:
            try:
                chunk = os.read(self._fd, 128)
            except OSError:
                return
            if not chunk:
                return
            pend += chunk
            while pend and not self._stop:
                tok, used = parse_key(pend)
                if used == 0:
                    break                           # incomplete: wait for more
                pend = pend[used:]
                if tok:
                    try:
                        self.on_key(tok)
                    except Exception:
                        pass

    def stop(self):
        self._stop = True
        self.restore()

    def restore(self):
        if self._saved is not None and self._fd is not None:
            try:
                import termios
                termios.tcsetattr(self._fd, termios.TCSADRAIN, self._saved)
            except Exception:
                pass
            self._saved = None
