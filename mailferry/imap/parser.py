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

Incremental IMAP response tokenizer.

A logical server response is a sequence of *segments*: (text, literal) pairs,
where `text` is the line content with any trailing "{N}" marker stripped and
`literal` is the N raw bytes that followed it (None for the final segment).

Tokens: str atoms / quoted strings (latin-1 decoded, lossless), bytes for
literals, nested lists for parenthesised lists, None for NIL.
Atoms containing '[' consume through the matching ']' (BODY[HEADER.FIELDS
(...)] stays one token).
"""
from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import List, Optional, Tuple

from ..errors import ProtocolError

Segment = Tuple[bytes, Optional[bytes]]

LITERAL_TAIL = re.compile(rb"\{(\d+)(\+)?\}$")
_STATUS_WORDS = {"OK", "NO", "BAD", "BYE", "PREAUTH"}


@dataclass
class Response:
    kind: str                     # 'tagged' | 'untagged' | 'cont'
    tag: str = ""
    status: str = ""              # OK/NO/BAD/BYE/PREAUTH for status responses
    dtype: str = ""               # untagged data type: FETCH/LIST/SEARCH/...
    num: Optional[int] = None     # leading number for EXISTS/FETCH/...
    code: list = field(default_factory=list)   # tokens inside [...]
    text: str = ""
    tokens: list = field(default_factory=list)


class _Cursor:
    """Token reader across segments."""

    def __init__(self, segs: List[Segment], start: int = 0):
        self.segs = segs
        self.si = 0
        self.i = start

    def _text(self) -> bytes:
        return self.segs[self.si][0]

    def at_end_of_seg(self) -> bool:
        return self.i >= len(self._text())

    def read_tokens(self) -> list:
        out: list = []
        stack: List[list] = [out]
        while True:
            # segment exhausted -> emit literal (if any), move on
            if self.at_end_of_seg():
                lit = self.segs[self.si][1]
                if lit is not None:
                    stack[-1].append(lit)
                if self.si + 1 < len(self.segs):
                    self.si += 1
                    self.i = 0
                    continue
                break
            t = self._text()
            c = t[self.i:self.i + 1]
            if c == b" ":
                self.i += 1
            elif c == b"(":
                new: list = []
                stack[-1].append(new)
                stack.append(new)
                self.i += 1
            elif c == b")":
                if len(stack) > 1:
                    stack.pop()
                self.i += 1
            elif c == b'"':
                self.i += 1
                buf = bytearray()
                while True:
                    if self.at_end_of_seg():
                        raise ProtocolError("unterminated quoted string")
                    ch = t[self.i:self.i + 1]
                    if ch == b"\\" and self.i + 1 < len(t):
                        buf += t[self.i + 1:self.i + 2]
                        self.i += 2
                    elif ch == b'"':
                        self.i += 1
                        break
                    else:
                        buf += ch
                        self.i += 1
                stack[-1].append(buf.decode("latin-1"))
            else:
                # atom (may embed [...] with spaces/parens inside)
                start = self.i
                depth = 0
                while not self.at_end_of_seg():
                    ch = t[self.i:self.i + 1]
                    if depth == 0 and ch in b" ()":
                        break
                    if ch == b"[":
                        depth += 1
                    elif ch == b"]":
                        depth = max(0, depth - 1)
                    self.i += 1
                atom = t[start:self.i].decode("latin-1")
                if atom.upper() == "NIL":
                    stack[-1].append(None)
                else:
                    stack[-1].append(atom)
        return out


def _split_code_text(rest: bytes) -> Tuple[list, str]:
    """Peel a leading [response-code] off a status line remainder."""
    if rest.startswith(b"["):
        depth = 0
        for i, b in enumerate(rest):
            ch = bytes((b,))
            if ch == b"[":
                depth += 1
            elif ch == b"]":
                depth -= 1
                if depth == 0:
                    inner = rest[1:i]
                    text = rest[i + 1:].strip()
                    toks = _Cursor([(inner, None)]).read_tokens()
                    return toks, text.decode("latin-1")
        # unbalanced -> treat as text
    return [], rest.decode("latin-1")


def parse_response(segs: List[Segment]) -> Response:
    if not segs:
        raise ProtocolError("empty response")
    first = segs[0][0]
    if first.startswith(b"+"):
        return Response(kind="cont", text=first[1:].strip().decode("latin-1"))

    sp1 = first.find(b" ")
    if sp1 < 0:
        raise ProtocolError(f"malformed response line: {first[:80]!r}")
    tag = first[:sp1].decode("latin-1")
    rest = first[sp1 + 1:]
    sp2 = rest.find(b" ")
    word = (rest if sp2 < 0 else rest[:sp2]).decode("latin-1").upper()
    tail = b"" if sp2 < 0 else rest[sp2 + 1:]

    if tag == "*":
        if word.isdigit():
            num = int(word)
            sp3 = tail.find(b" ")
            dtype = (tail if sp3 < 0 else tail[:sp3]).decode("latin-1").upper()
            after = b"" if sp3 < 0 else tail[sp3 + 1:]
            r = Response(kind="untagged", dtype=dtype, num=num)
            if dtype == "FETCH":
                segs2 = [(after, segs[0][1])] + segs[1:]
                r.tokens = _Cursor(segs2).read_tokens()
            return r
        if word in _STATUS_WORDS:
            code, text = _split_code_text(tail)
            return Response(kind="untagged", dtype=word, status=word, code=code, text=text)
        segs2 = [(tail, segs[0][1])] + segs[1:]
        r = Response(kind="untagged", dtype=word)
        r.tokens = _Cursor(segs2).read_tokens()
        return r

    # tagged completion
    if word not in _STATUS_WORDS:
        raise ProtocolError(f"unexpected tagged word {word!r}")
    code, text = _split_code_text(tail)
    return Response(kind="tagged", tag=tag, status=word, code=code, text=text)


# --------------------------------------------------------------------------
# FETCH helpers
# --------------------------------------------------------------------------

def fetch_pairs(tokens: list) -> dict:
    """Flatten a FETCH att-list into {NAME: value}; BODY[...] -> 'BODY'."""
    if len(tokens) == 1 and isinstance(tokens[0], list):
        tokens = tokens[0]
    out = {}
    i = 0
    while i + 1 <= len(tokens) - 1:
        name = tokens[i]
        if not isinstance(name, str):
            i += 1
            continue
        key = name.upper()
        if key.startswith("BODY[") or key.startswith("BODY.PEEK["):
            key = "BODY"
        out[key] = tokens[i + 1]
        i += 2
    return out


def as_bytes(v) -> bytes:
    if isinstance(v, bytes):
        return v
    if isinstance(v, str):
        return v.encode("latin-1")
    if v is None:
        return b""
    raise ProtocolError(f"expected string/literal, got {type(v).__name__}")


def as_int(v, default=None):
    try:
        return int(v)
    except (TypeError, ValueError):
        return default


def flags_of(v) -> List[str]:
    if isinstance(v, list):
        return [x for x in v if isinstance(x, str)]
    return []
