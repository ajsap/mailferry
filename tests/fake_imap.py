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

Minimal in-memory IMAP server for end-to-end tests (LOGIN, LIST, STATUS,
SELECT/EXAMINE, UID SEARCH/FETCH/STORE, APPEND with literals + APPENDUID,
CREATE, NAMESPACE, ID). Runs on its own thread + event loop.
"""
from __future__ import annotations

import asyncio
import re
import threading
from typing import Dict, List, Optional

QUOTED_OR_ATOM = re.compile(r'"((?:[^"\\]|\\.)*)"|(\S+)')
LIT = re.compile(rb"\{(\d+)(\+)?\}$")


class Msg:
    def __init__(self, uid: int, body: bytes, flags=None, internaldate='17-Jul-2026 10:00:00 +0000'):
        self.uid = uid
        self.body = body
        self.flags = set(flags or [])
        self.internaldate = internaldate

    def headers(self) -> bytes:
        i = self.body.find(b"\r\n\r\n")
        return self.body[:i + 2] if i >= 0 else self.body

    def header_fields(self, wanted: List[str]) -> bytes:
        out = []
        take = False
        for line in self.headers().split(b"\r\n"):
            if not line:
                break
            if line[:1] in (b" ", b"\t"):
                if take:
                    out.append(line)
                continue
            name = line.split(b":", 1)[0].decode("latin-1").strip().lower()
            take = name in wanted
            if take:
                out.append(line)
        return b"\r\n".join(out) + (b"\r\n\r\n" if out else b"\r\n")


class Folder:
    def __init__(self, name: str, attrs=None, uidvalidity=1111):
        self.name = name
        self.attrs = list(attrs or [])
        self.uidvalidity = uidvalidity
        self.uidnext = 1
        self.msgs: List[Msg] = []

    def add(self, body: bytes, flags=None, internaldate='17-Jul-2026 10:00:00 +0000') -> Msg:
        m = Msg(self.uidnext, body, flags, internaldate)
        self.uidnext += 1
        self.msgs.append(m)
        return m

    def by_set(self, setstr: str) -> List[Msg]:
        out = []
        for part in setstr.split(","):
            if ":" in part:
                a, b = part.split(":", 1)
                lo = int(a)
                hi = self.uidnext if b == "*" else int(b)
                out.extend(m for m in self.msgs if lo <= m.uid <= hi)
            elif part.strip():
                u = int(part)
                out.extend(m for m in self.msgs if m.uid == u)
    # keep uid order
        out.sort(key=lambda m: m.uid)
        return out


class Account:
    def __init__(self, user: str, password: str):
        self.user = user
        self.password = password
        self.folders: Dict[str, Folder] = {"INBOX": Folder("INBOX")}

    def folder(self, name: str) -> Optional[Folder]:
        for k, f in self.folders.items():
            if k.upper() == name.upper() if name.upper() == "INBOX" else k == name:
                return f
        return self.folders.get(name)


def toks(s: str) -> List[str]:
    out = []
    for m in QUOTED_OR_ATOM.finditer(s):
        if m.group(1) is not None:
            out.append(m.group(1).replace('\\"', '"').replace("\\\\", "\\"))
        else:
            out.append(m.group(2))
    return out


class FakeIMAPServer:
    def __init__(self, accounts: Dict[str, Account], caps=None):
        self.accounts = accounts
        self.caps = caps or ["IMAP4rev1", "UIDPLUS", "LITERAL+", "NAMESPACE",
                             "SPECIAL-USE", "ID", "STATUS=SIZE", "UNSELECT"]
        self.port = 0
        self._server = None
        self.append_count = 0
        self.fetch_body_count = 0

    async def _start(self):
        self._server = await asyncio.start_server(self._client, "127.0.0.1", 0)
        self.port = self._server.sockets[0].getsockname()[1]

    async def _client(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter):
        acct: Optional[Account] = None
        selected: Optional[Folder] = None

        def send(line: str):
            writer.write(line.encode("latin-1") + b"\r\n")

        def sendb(data: bytes):
            writer.write(data)

        send(f"* OK [CAPABILITY {' '.join(self.caps)}] fake server ready")
        await writer.drain()
        try:
            while True:
                raw = await reader.readline()
                if not raw:
                    return
                raw = raw.rstrip(b"\r\n")
                lits: List[bytes] = []
                while True:
                    m = LIT.search(raw)
                    if not m:
                        break
                    n, plus = int(m.group(1)), m.group(2)
                    if not plus:
                        send("+ OK")
                        await writer.drain()
                    data = await reader.readexactly(n)
                    lits.append(data)
                    rest = (await reader.readline()).rstrip(b"\r\n")
                    raw = raw[:m.start()] + b"\x00LIT\x00" + rest
                text = raw.decode("latin-1")
                parts = text.split(" ", 2)
                if len(parts) < 2:
                    continue
                tag, verb = parts[0], parts[1].upper()
                rest = parts[2] if len(parts) > 2 else ""
                if verb == "UID" and rest:
                    sub, _, rest2 = rest.partition(" ")
                    verb = "UID " + sub.upper()
                    rest = rest2

                if verb == "CAPABILITY":
                    send(f"* CAPABILITY {' '.join(self.caps)}")
                    send(f"{tag} OK done")
                elif verb == "NOOP":
                    send(f"{tag} OK done")
                elif verb == "ID":
                    send("* ID NIL")
                    send(f"{tag} OK done")
                elif verb == "NAMESPACE":
                    send('* NAMESPACE (("" "/")) NIL NIL')
                    send(f"{tag} OK done")
                elif verb == "LOGIN":
                    t = toks(rest)
                    a = self.accounts.get(t[0])
                    if a and a.password == t[1]:
                        acct = a
                        send(f"{tag} OK [CAPABILITY {' '.join(self.caps)}] logged in")
                    else:
                        send(f"{tag} NO [AUTHENTICATIONFAILED] bad credentials")
                elif verb == "LOGOUT":
                    send("* BYE bye")
                    send(f"{tag} OK done")
                    await writer.drain()
                    return
                elif acct is None:
                    send(f"{tag} NO not authenticated")
                elif verb == "LIST":
                    for name, f in sorted(acct.folders.items()):
                        attrs = " ".join(f.attrs)
                        send(f'* LIST ({attrs}) "/" "{name}"')
                    send(f"{tag} OK done")
                elif verb == "SUBSCRIBE":
                    send(f"{tag} OK done")
                elif verb == "CREATE":
                    name = toks(rest)[0]
                    if acct.folder(name):
                        send(f"{tag} NO [ALREADYEXISTS] exists")
                    else:
                        acct.folders[name] = Folder(name, uidvalidity=2222 + len(acct.folders))
                        send(f"{tag} OK created")
                elif verb == "STATUS":
                    name = toks(rest)[0]
                    f = acct.folder(name)
                    if not f:
                        send(f"{tag} NO no such folder")
                    else:
                        items = []
                        raw_items = rest.upper()
                        if "MESSAGES" in raw_items:
                            items += ["MESSAGES", str(len(f.msgs))]
                        if "UIDNEXT" in raw_items:
                            items += ["UIDNEXT", str(f.uidnext)]
                        if "UIDVALIDITY" in raw_items:
                            items += ["UIDVALIDITY", str(f.uidvalidity)]
                        if "SIZE" in raw_items:
                            items += ["SIZE", str(sum(len(m.body) for m in f.msgs))]
                        send(f'* STATUS "{name}" ({" ".join(items)})')
                        send(f"{tag} OK done")
                elif verb in ("SELECT", "EXAMINE"):
                    name = toks(rest)[0]
                    f = acct.folder(name)
                    if not f:
                        send(f"{tag} NO no such folder")
                    else:
                        selected = f
                        send(f"* {len(f.msgs)} EXISTS")
                        send("* 0 RECENT")
                        send(r"* FLAGS (\Answered \Flagged \Deleted \Seen \Draft)")
                        send(r"* OK [PERMANENTFLAGS (\Answered \Flagged \Deleted \Seen \Draft \*)] ok")
                        send(f"* OK [UIDVALIDITY {f.uidvalidity}] ok")
                        send(f"* OK [UIDNEXT {f.uidnext}] ok")
                        ro = "READ-ONLY" if verb == "EXAMINE" else "READ-WRITE"
                        send(f"{tag} OK [{ro}] done")
                elif verb == "UID SEARCH":
                    if selected is None:
                        send(f"{tag} NO nothing selected")
                    else:
                        uids = " ".join(str(m.uid) for m in selected.msgs)
                        send(f"* SEARCH{(' ' + uids) if uids else ''}")
                        send(f"{tag} OK done")
                elif verb == "UID FETCH":
                    if selected is None:
                        send(f"{tag} NO nothing selected")
                    else:
                        setstr, _, items = rest.partition(" ")
                        items_up = items.upper()
                        want_hdr = "HEADER.FIELDS" in items_up
                        want_body = "BODY.PEEK[]" in items_up or "BODY[]" in items_up
                        fields = []
                        if want_hdr:
                            mm = re.search(r"HEADER\.FIELDS \(([^)]*)\)", items, re.I)
                            fields = [x.lower() for x in (mm.group(1) if mm else "").split()]
                        for seq, msg in enumerate(selected.by_set(setstr), start=1):
                            attrs = [f"UID {msg.uid}"]
                            if "FLAGS" in items_up:
                                attrs.append(f"FLAGS ({' '.join(sorted(msg.flags))})")
                            if "INTERNALDATE" in items_up:
                                attrs.append(f'INTERNALDATE "{msg.internaldate}"')
                            if "RFC822.SIZE" in items_up:
                                attrs.append(f"RFC822.SIZE {len(msg.body)}")
                            if want_hdr:
                                data = msg.header_fields(fields)
                                spec = re.search(r"BODY\.PEEK\[[^\]]*\]", items, re.I).group(0)
                                spec = spec.replace(".PEEK", "")
                                sendb(f"* {seq} FETCH ({' '.join(attrs)} {spec} "
                                      f"{{{len(data)}}}\r\n".encode("latin-1"))
                                sendb(data)
                                sendb(b")\r\n")
                            elif want_body:
                                self.fetch_body_count += 1
                                sendb(f"* {seq} FETCH ({' '.join(attrs)} BODY[] "
                                      f"{{{len(msg.body)}}}\r\n".encode("latin-1"))
                                sendb(msg.body)
                                sendb(b")\r\n")
                            else:
                                send(f"* {seq} FETCH ({' '.join(attrs)})")
                            await writer.drain()
                        send(f"{tag} OK fetch done")
                elif verb == "UID STORE":
                    if selected is None:
                        send(f"{tag} NO nothing selected")
                    else:
                        setstr, _, items = rest.partition(" ")
                        mm = re.search(r"\(([^)]*)\)", items)
                        new = set((mm.group(1) if mm else "").split())
                        for msg in selected.by_set(setstr):
                            msg.flags = set(new)
                        send(f"{tag} OK stored")
                elif verb == "APPEND":
                    t = toks(rest.split("\x00LIT\x00")[0])
                    name = t[0]
                    f = acct.folder(name)
                    if not f:
                        send(f"{tag} NO [TRYCREATE] no such folder")
                    else:
                        mm = re.search(r"\(([^)]*)\)", rest)
                        flags = set((mm.group(1) if mm else "").split())
                        dates = re.findall(r'"((?:[^"\\]|\\.)*)"', rest)
                        date = dates[-1] if dates else "17-Jul-2026 10:00:00 +0000"
                        body = lits[0] if lits else b""
                        msg = f.add(body, flags - {"\\Recent"}, date)
                        self.append_count += 1
                        send(f"{tag} OK [APPENDUID {f.uidvalidity} {msg.uid}] append done"
                             if "UIDPLUS" in self.caps else f"{tag} OK append done")
                elif verb == "UNSELECT":
                    selected = None
                    send(f"{tag} OK done")
                elif verb == "COMPRESS":
                    send(f"{tag} NO not supported")
                else:
                    send(f"{tag} BAD unknown command {verb}")
                await writer.drain()
        except (ConnectionResetError, asyncio.IncompleteReadError, BrokenPipeError):
            return
        finally:
            try:
                writer.close()
            except Exception:
                pass


class ServerThread:
    """Hosts one or more FakeIMAPServers on a private loop thread."""

    def __init__(self, *servers: FakeIMAPServer):
        self.servers = servers
        self.loop = asyncio.new_event_loop()
        self.thread = threading.Thread(target=self._run, daemon=True)

    def _run(self):
        asyncio.set_event_loop(self.loop)
        self.loop.run_forever()

    def start(self):
        self.thread.start()
        for s in self.servers:
            fut = asyncio.run_coroutine_threadsafe(s._start(), self.loop)
            fut.result(timeout=10)

    def stop(self):
        self.loop.call_soon_threadsafe(self.loop.stop)
