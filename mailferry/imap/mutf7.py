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

Modified UTF-7 (RFC 3501 §5.1.3) codec for IMAP mailbox names.
"""
from __future__ import annotations

import base64


def encode(name: str) -> str:
    """UTF-8 mailbox name -> mUTF-7 wire form (ASCII str)."""
    out = []
    buf = []

    def flush():
        if buf:
            b = "".join(buf).encode("utf-16-be")
            s = base64.b64encode(b).decode("ascii").rstrip("=").replace("/", ",")
            out.append("&" + s + "-")
            buf.clear()

    for ch in name:
        o = ord(ch)
        if 0x20 <= o <= 0x7E:
            flush()
            out.append("&-" if ch == "&" else ch)
        else:
            buf.append(ch)
    flush()
    return "".join(out)


def decode(wire: str) -> str:
    """mUTF-7 wire form -> UTF-8 mailbox name. Malformed segments pass through."""
    out = []
    i = 0
    n = len(wire)
    while i < n:
        c = wire[i]
        if c == "&":
            j = wire.find("-", i + 1)
            if j < 0:
                out.append(wire[i:])
                break
            seg = wire[i + 1:j]
            if seg == "":
                out.append("&")
            else:
                s = seg.replace(",", "/")
                s += "=" * ((4 - len(s) % 4) % 4)
                try:
                    out.append(base64.b64decode(s).decode("utf-16-be"))
                except Exception:
                    out.append(wire[i:j + 1])
            i = j + 1
        else:
            out.append(c)
            i += 1
    return "".join(out)
