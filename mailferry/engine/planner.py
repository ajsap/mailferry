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

Folder enumeration, mapping, namespace/delimiter translation, filters.
"""
from __future__ import annotations

import fnmatch
from dataclasses import dataclass, field
from typing import Dict, List, Optional

from ..imap import mutf7
from ..imap.client import ImapClient

SPECIAL_USE = ("\\SENT", "\\DRAFTS", "\\TRASH", "\\JUNK", "\\ARCHIVE", "\\FLAGGED")
GMAIL_VIRTUAL = ("\\ALL", "\\IMPORTANT")
NAME_ROLES = {   # heuristic fallback when SPECIAL-USE is absent
    "sent": "\\SENT", "sent items": "\\SENT", "sent messages": "\\SENT",
    "drafts": "\\DRAFTS", "trash": "\\TRASH", "deleted items": "\\TRASH",
    "junk": "\\JUNK", "spam": "\\JUNK", "archive": "\\ARCHIVE",
}


@dataclass
class FolderPlan:
    src_display: str
    src_wire: str
    dst_display: str
    dst_wire: str
    attrs: List[str] = field(default_factory=list)
    est_msgs: int = 0
    est_bytes: int = 0
    uidvalidity: int = 0
    uidnext: int = 0


def _role_of(attrs: List[str], display: str) -> Optional[str]:
    for a in attrs:
        if a.upper() in SPECIAL_USE:
            return a.upper()
    return NAME_ROLES.get(display.lower())


async def build_plan(src: ImapClient, dst: ImapClient, cfg, mapping: Dict[str, str],
                     log=None) -> List[FolderPlan]:
    src_entries = await src.list_all()
    dst_entries = await dst.list_all()
    src_pfx_wire, src_ns_delim = await src.namespace_info()
    dst_pfx_wire, dst_ns_delim = await dst.namespace_info()

    def entry_delim(entries) -> Optional[str]:
        for _, d, _ in entries:
            if d:
                return d
        return None

    src_delim = src_ns_delim or entry_delim(src_entries) or "/"
    dst_delim = dst_ns_delim or entry_delim(dst_entries) or "/"
    src_pfx = mutf7.decode(src_pfx_wire or "")
    dst_pfx = mutf7.decode(dst_pfx_wire or "")
    if src_pfx.lower().rstrip(src_delim) == "inbox" and src_pfx:
        src_pfx = src_pfx if src_pfx.endswith(src_delim) else src_pfx + src_delim
    if dst_pfx and not dst_pfx.endswith(dst_delim):
        dst_pfx = dst_pfx + dst_delim

    # destination role map (special-use aware, localisation-proof)
    dst_roles: Dict[str, str] = {}
    dst_names_lower = set()
    for attrs, _, wname in dst_entries:
        disp = mutf7.decode(wname)
        dst_names_lower.add(disp.lower())
        role = _role_of(attrs, disp.split(dst_delim)[-1] if dst_delim else disp)
        if role and role not in dst_roles:
            dst_roles[role] = wname

    plans: List[FolderPlan] = []
    for attrs, delim, wname in src_entries:
        up_attrs = [a.upper() for a in attrs]
        if "\\NOSELECT" in up_attrs or "\\NONEXISTENT" in up_attrs:
            continue
        display = mutf7.decode(wname)
        leaf = display.split(delim or src_delim)[-1]
        if not cfg.gmail_all_mail and any(a in up_attrs for a in GMAIL_VIRTUAL):
            if log:
                log(f"plan: skipping Gmail virtual folder {display} (use --gmail-all-mail to include)")
            continue
        if cfg.include and not any(fnmatch.fnmatch(display, pat) for pat in cfg.include):
            continue
        if cfg.exclude and any(fnmatch.fnmatch(display, pat) for pat in cfg.exclude):
            if log:
                log(f"plan: excluded {display}")
            continue

        if display.upper() == "INBOX":
            dst_display = "INBOX"
        elif display in mapping:
            dst_display = mapping[display]
        else:
            role = _role_of(up_attrs, leaf)
            if role and role in dst_roles:
                dst_display = mutf7.decode(dst_roles[role])
            else:
                body = display
                if src_pfx and body.startswith(src_pfx):
                    body = body[len(src_pfx):]
                segs = body.split(delim or src_delim)
                joined = dst_delim.join(s for s in segs if s)
                if joined.upper() == "INBOX":
                    dst_display = "INBOX"
                else:
                    dst_display = (dst_pfx + joined) if dst_pfx and joined.lower() != "inbox" else joined
        plans.append(FolderPlan(
            src_display=display, src_wire=wname,
            dst_display=dst_display, dst_wire=mutf7.encode(dst_display),
            attrs=up_attrs))

    # pipelined STATUS estimates
    want_size = src.has("STATUS=SIZE")
    items = "(MESSAGES UIDNEXT UIDVALIDITY" + (" SIZE" if want_size else "") + ")"
    pendings = []
    for p in plans:
        from ..imap.client import quote
        pendings.append((p, await src.cmd_nowait("STATUS", f"{quote(p.src_wire)} {items}",
                                                 types=("STATUS",))))
    for p, pend in pendings:
        try:
            res = await pend.fut
        except Exception:
            continue
        for resp in res.data:
            toks = resp.tokens
            if toks and isinstance(toks[-1], list):
                lst = toks[-1]
                kv = {}
                for i in range(0, len(lst) - 1, 2):
                    if isinstance(lst[i], str):
                        try:
                            kv[lst[i].upper()] = int(lst[i + 1])
                        except (TypeError, ValueError):
                            pass
                p.est_msgs = kv.get("MESSAGES", 0)
                p.est_bytes = kv.get("SIZE", 0)
                p.uidvalidity = kv.get("UIDVALIDITY", 0)
                p.uidnext = kv.get("UIDNEXT", 0)

    # INBOX first, then largest first (finish long poles early)
    plans.sort(key=lambda p: (0 if p.src_display.upper() == "INBOX" else 1,
                              -(p.est_bytes or 0), -(p.est_msgs or 0)))
    # dedupe collisions on destination name
    seen = {}
    for p in plans:
        low = p.dst_display.lower()
        if low in seen and seen[low] != p.src_display:
            p.dst_display = p.dst_display + "-mf"
            p.dst_wire = mutf7.encode(p.dst_display)
        seen[p.dst_display.lower()] = p.src_display
    return plans
