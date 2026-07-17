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

Async IMAP client: tag-multiplexed commands, pipelining, streamed literals,
TLS / STARTTLS, COMPRESS=DEFLATE, inactivity watchdog. Stdlib only.
"""
from __future__ import annotations

import asyncio
import base64
import ssl as ssl_mod
import zlib
from collections import deque
from dataclasses import dataclass, field
from typing import Deque, Dict, List, Optional, Tuple

from .. import __version__
from ..errors import (AuthFailed, CommandFailed, ConnectionLost, ProtocolError,
                      classify_command_failure)
from ..util import monotonic
from . import mutf7
from .parser import (LITERAL_TAIL, Response, _Cursor, as_bytes, as_int,
                     fetch_pairs, flags_of, parse_response)

CRLF = b"\r\n"
CHUNK = 65536
INLINE_MAX = 8 * 1024 * 1024          # max literal parsed in memory
DRAIN_EVERY = 262144

ROUTED_TYPES = ("LIST", "XLIST", "LSUB", "STATUS", "SEARCH", "ESEARCH", "FETCH",
                "FLAGS", "CAPABILITY", "NAMESPACE", "QUOTA", "QUOTAROOT", "ENABLED", "ID")


def quote(s: str) -> str:
    return '"' + s.replace("\\", "\\\\").replace('"', '\\"') + '"'


def wire_name(display: str) -> str:
    return mutf7.encode(display)


class _NullSide:
    def rx(self, n): pass
    def tx(self, n): pass
    def state(self, s): pass


@dataclass
class Pending:
    tag: str
    name: str
    fut: asyncio.Future
    types: Tuple[str, ...] = ()
    data: List[Response] = field(default_factory=list)


@dataclass
class CmdResult:
    status: str
    text: str
    code: list
    data: List[Response]


class SelectInfo:
    def __init__(self):
        self.uidvalidity: Optional[int] = None
        self.uidnext: Optional[int] = None
        self.exists: int = 0
        self.highestmodseq: Optional[int] = None
        self.permanentflags: List[str] = []
        self.readonly = True


class BodyHandle:
    """One streamed BODY[] fetch. Reader fills; consumer drains FIFO-safely."""

    def __init__(self, uid: int):
        self.uid = uid
        self.size: Optional[int] = None
        self.q: asyncio.Queue = asyncio.Queue(maxsize=64)
        self.started = False
        self.finished = False
        self.error: Optional[BaseException] = None
        self._size_evt = asyncio.Event()
        self.pending: Optional[Pending] = None

    def fail(self, exc: BaseException):
        if self.error is None:
            self.error = exc
        self._size_evt.set()
        try:
            self.q.put_nowait(None)
        except asyncio.QueueFull:
            pass

    async def wait_size(self) -> int:
        await self._size_evt.wait()
        if self.error is not None:
            raise self.error
        return self.size or 0

    async def chunks(self):
        while True:
            c = await self.q.get()
            if c is None:
                if self.error is not None:
                    raise self.error
                return
            yield c


class AppendHandle:
    def __init__(self, client: "ImapClient", pending: Pending, size: int):
        self.client = client
        self.pending = pending
        self.size = size
        self.written = 0

    async def write(self, chunk: bytes):
        self.client._write_raw(chunk)
        self.written += len(chunk)
        if self.client._unflushed >= DRAIN_EVERY:
            await self.client._drain()

    async def finish(self) -> Pending:
        self.client._write_raw(CRLF)
        await self.client._drain()
        return self.pending


class ImapClient:
    """One IMAP connection. All coroutines must run on one event loop."""

    def __init__(self, endpoint, cfg, side=None, log=None, role="src"):
        self.ep = endpoint
        self.cfg = cfg
        self.side = side or _NullSide()
        self.log = log
        self.role = role
        self.caps: set = set()
        self.reader: Optional[asyncio.StreamReader] = None
        self.writer: Optional[asyncio.StreamWriter] = None
        self._buf = bytearray()
        self._eof = False
        self._inf = None            # zlib decompress (COMPRESS)
        self._def = None            # zlib compress
        self._unflushed = 0
        self._tagno = 0
        self._pending: Dict[str, Pending] = {}
        self._want: Dict[str, Deque[Pending]] = {}
        self._body_fifo: Deque[BodyHandle] = deque()
        self._cont_fut: Optional[asyncio.Future] = None
        self._sel: Optional[SelectInfo] = None
        self._reader_task: Optional[asyncio.Task] = None
        self._watch_task: Optional[asyncio.Task] = None
        self._closed = False
        self._close_exc: Optional[BaseException] = None
        self._last_activity = monotonic()
        self._append_lock = asyncio.Lock()
        self.server_greeting = ""
        self.compressed = False

    # ---------------------------------------------------------------- io --

    def _trace(self, direction: str, text: str):
        if self.cfg.trace and self.log:
            self.log(f"{self.role} {direction} {text[:200]}")

    async def _fill(self):
        if self._eof:
            raise ConnectionLost(self._close_exc or "connection closed")
        try:
            data = await self.reader.read(CHUNK)
        except (OSError, ssl_mod.SSLError, asyncio.IncompleteReadError) as e:
            self._eof = True
            raise ConnectionLost(str(e))
        if not data:
            self._eof = True
            raise ConnectionLost("EOF from server")
        self.side.rx(len(data))
        self._last_activity = monotonic()
        if self._inf is not None:
            data = self._inf.decompress(data)
        self._buf += data

    async def _readline(self) -> bytes:
        while True:
            i = self._buf.find(b"\r\n")
            if i >= 0:
                line = bytes(self._buf[:i])
                del self._buf[:i + 2]
                return line
            if len(self._buf) > 1024 * 1024:
                raise ProtocolError("response line too long")
            await self._fill()

    async def _readn(self, n: int) -> bytes:
        if n > INLINE_MAX:
            raise ProtocolError(f"inline literal too large ({n})")
        out = bytearray()
        while len(out) < n:
            if self._buf:
                take = min(n - len(out), len(self._buf))
                out += self._buf[:take]
                del self._buf[:take]
            else:
                await self._fill()
        return bytes(out)

    async def _read_literal_stream(self, n: int, bh: BodyHandle):
        remaining = n
        while remaining > 0:
            if not self._buf:
                await self._fill()
            take = min(remaining, len(self._buf), CHUNK)
            chunk = bytes(self._buf[:take])
            del self._buf[:take]
            remaining -= take
            await bh.q.put(chunk)

    def _write_raw(self, data: bytes):
        if self._closed:
            raise ConnectionLost(self._close_exc or "connection closed")
        if self._def is not None:
            data = self._def.compress(data) + self._def.flush(zlib.Z_SYNC_FLUSH)
        self.side.tx(len(data))
        self._unflushed += len(data)
        self._last_activity = monotonic()
        self.writer.write(data)

    async def _drain(self):
        self._unflushed = 0
        try:
            await self.writer.drain()
        except (OSError, ssl_mod.SSLError) as e:
            raise ConnectionLost(str(e))

    async def _send_line(self, line: str):
        self._trace("C:", line)
        self._write_raw(line.encode("latin-1") + CRLF)
        await self._drain()

    # ---------------------------------------------------------- commands --

    def _next_tag(self) -> str:
        self._tagno += 1
        return f"MF{self._tagno:04d}"

    def _register(self, name: str, types: Tuple[str, ...]) -> Pending:
        tag = self._next_tag()
        fut = asyncio.get_event_loop().create_future()
        p = Pending(tag=tag, name=name, fut=fut, types=tuple(t.upper() for t in types))
        self._pending[tag] = p
        for t in p.types:
            self._want.setdefault(t, deque()).append(p)
        return p

    async def cmd_nowait(self, name: str, args: str = "", types: Tuple[str, ...] = ()) -> Pending:
        p = self._register(name, types)
        line = f"{p.tag} {name}" + (f" {args}" if args else "")
        await self._send_line(line)
        return p

    async def cmd(self, name: str, args: str = "", types: Tuple[str, ...] = ()) -> CmdResult:
        p = await self.cmd_nowait(name, args, types)
        return await p.fut

    # ------------------------------------------------------------ reader --

    async def _read_one(self) -> Optional[Response]:
        line = await self._readline()
        m = LITERAL_TAIL.search(line)
        upper = line.upper()
        if (m and self._body_fifo and upper.startswith(b"* ")
                and b" FETCH " in upper and b"BODY[]" in upper):
            # ---- streamed message body ----
            size = int(m.group(1))
            self._trace("S:", line.decode("latin-1", "replace"))
            bh = self._body_fifo.popleft()
            bh.started = True
            bh.size = size
            # prefix attrs (may or may not contain UID)
            fi = upper.find(b" FETCH ")
            prefix = line[fi + 7:m.start()]
            uid_seen = self._scan_uid(prefix)
            bh._size_evt.set()
            await self._read_literal_stream(size, bh)
            # suffix: remainder of the logical response
            while True:
                l2 = await self._readline()
                m2 = LITERAL_TAIL.search(l2)
                if m2:
                    n2 = int(m2.group(1))
                    if n2 > INLINE_MAX:
                        # drain unexpected big literal
                        left = n2
                        while left > 0:
                            if not self._buf:
                                await self._fill()
                            t = min(left, len(self._buf))
                            del self._buf[:t]
                            left -= t
                    else:
                        await self._readn(n2)
                    continue
                if uid_seen is None:
                    uid_seen = self._scan_uid(l2)
                break
            if uid_seen is not None and uid_seen != bh.uid:
                raise ProtocolError(f"body fetch uid mismatch: wanted {bh.uid} got {uid_seen}")
            bh.finished = True
            await bh.q.put(None)
            return None
        # ---- ordinary response (inline literals) ----
        segs = []
        cur = line
        while True:
            mm = LITERAL_TAIL.search(cur)
            if not mm or cur.startswith(b"+"):
                segs.append((cur, None))
                break
            n = int(mm.group(1))
            lit = await self._readn(n)
            segs.append((cur[:mm.start()], lit))
            cur = await self._readline()
        self._trace("S:", segs[0][0].decode("latin-1", "replace"))
        return parse_response(segs)

    @staticmethod
    def _scan_uid(text: bytes) -> Optional[int]:
        toks = text.upper().replace(b"(", b" ").replace(b")", b" ").split()
        for i, t in enumerate(toks):
            if t == b"UID" and i + 1 < len(toks):
                try:
                    return int(toks[i + 1])
                except ValueError:
                    return None
        return None

    async def _reader_loop(self):
        try:
            while True:
                r = await self._read_one()
                if r is not None:
                    self._dispatch(r)
        except asyncio.CancelledError:
            pass
        except BaseException as e:
            self._shutdown(e if isinstance(e, (ConnectionLost, ProtocolError))
                           else ConnectionLost(str(e)))

    def _dispatch(self, r: Response):
        if r.kind == "cont":
            if self._cont_fut and not self._cont_fut.done():
                self._cont_fut.set_result(r.text)
            return
        if r.kind == "tagged":
            p = self._pending.pop(r.tag, None)
            if p is None:
                return
            for t in p.types:
                dq = self._want.get(t)
                if dq and p in dq:
                    dq.remove(p)
            if p.fut.done():
                return
            if r.status == "OK":
                p.fut.set_result(CmdResult(r.status, r.text, r.code, p.data))
            else:
                p.fut.set_exception(classify_command_failure(
                    CommandFailed(p.name, r.status, r.text, r.code)))
            return
        # untagged
        dt = r.dtype
        if dt in ("OK", "NO", "BAD", "BYE", "PREAUTH"):
            if r.code:
                self._apply_code(r.code)
            if dt == "BYE":
                self._trace("S:", "BYE " + r.text)
            return
        if dt == "EXISTS" and self._sel is not None:
            self._sel.exists = r.num or 0
            return
        if dt in ("RECENT", "EXPUNGE"):
            return
        if dt == "CAPABILITY":
            self.caps = {str(t).upper() for t in r.tokens if isinstance(t, str)}
        if dt == "FLAGS" and self._sel is not None:
            pass  # folder-applicable flags; permanentflags come via code
        dq = self._want.get(dt)
        if dq:
            dq[0].data.append(r)

    def _apply_code(self, code: list):
        key = str(code[0]).upper() if code else ""
        sel = self._sel
        if key == "CAPABILITY":
            self.caps = {str(t).upper() for t in code[1:] if isinstance(t, str)}
        elif sel is not None and key == "UIDVALIDITY":
            sel.uidvalidity = as_int(code[1])
        elif sel is not None and key == "UIDNEXT":
            sel.uidnext = as_int(code[1])
        elif sel is not None and key == "HIGHESTMODSEQ":
            sel.highestmodseq = as_int(code[1])
        elif sel is not None and key == "PERMANENTFLAGS" and len(code) > 1 and isinstance(code[1], list):
            sel.permanentflags = [f for f in code[1] if isinstance(f, str)]

    # ------------------------------------------------------------- setup --

    def _ssl_context(self) -> ssl_mod.SSLContext:
        if self.cfg.tls_verify:
            ctx = ssl_mod.create_default_context()
        else:
            ctx = ssl_mod.SSLContext(ssl_mod.PROTOCOL_TLS_CLIENT)
            ctx.check_hostname = False
            ctx.verify_mode = ssl_mod.CERT_NONE
        try:
            ctx.minimum_version = ssl_mod.TLSVersion.TLSv1_2
        except Exception:
            pass
        return ctx

    async def connect(self):
        self.side.state("connect")
        ctx = self._ssl_context() if self.ep.security == "ssl" else None
        try:
            self.reader, self.writer = await asyncio.wait_for(
                asyncio.open_connection(self.ep.host, self.ep.port, ssl=ctx),
                timeout=min(60.0, self.cfg.timeout))
        except ssl_mod.SSLCertVerificationError as e:
            raise ConnectionLost(
                f"TLS certificate verification failed for {self.ep.host}: {e.verify_message if hasattr(e, 'verify_message') else e}. "
                f"Use --tls-no-verify only if you trust this server.")
        except (OSError, asyncio.TimeoutError) as e:
            raise ConnectionLost(f"cannot connect to {self.ep.host}:{self.ep.port}: {e}")
        greeting = await asyncio.wait_for(self._read_one(), timeout=min(60.0, self.cfg.timeout))
        if greeting is None or greeting.status not in ("OK", "PREAUTH"):
            raise ProtocolError(f"unexpected greeting: {greeting and greeting.text}")
        self.server_greeting = greeting.text
        if greeting.code and str(greeting.code[0]).upper() == "CAPABILITY":
            self.caps = {str(t).upper() for t in greeting.code[1:] if isinstance(t, str)}
        if not self.caps:
            await self._inline_cmd("CAPABILITY")
        if self.ep.security == "tls":
            self.side.state("tls")
            await self._starttls()
        self._reader_task = asyncio.get_event_loop().create_task(self._reader_loop())
        self._watch_task = asyncio.get_event_loop().create_task(self._watchdog())

    async def _inline_cmd(self, name: str, args: str = ""):
        """Command executed before the reader task exists (setup phase)."""
        tag = self._next_tag()
        await self._send_line(f"{tag} {name}" + (f" {args}" if args else ""))
        datas = []
        while True:
            r = await asyncio.wait_for(self._read_one(), timeout=min(60.0, self.cfg.timeout))
            if r is None:
                continue
            if r.kind == "tagged" and r.tag == tag:
                if r.status != "OK":
                    raise classify_command_failure(CommandFailed(name, r.status, r.text, r.code))
                return datas
            if r.kind == "untagged":
                if r.dtype == "CAPABILITY":
                    self.caps = {str(t).upper() for t in r.tokens if isinstance(t, str)}
                elif r.dtype in ("OK", "NO", "BAD") and r.code:
                    self._apply_code(r.code)
                datas.append(r)

    async def _starttls(self):
        if "STARTTLS" not in self.caps:
            raise ConnectionLost(f"{self.ep.host} does not advertise STARTTLS")
        await self._inline_cmd("STARTTLS")
        self._buf.clear()
        ctx = self._ssl_context()
        loop = asyncio.get_event_loop()
        transport = self.writer.transport
        if hasattr(self.writer, "start_tls"):          # Python 3.11+
            await self.writer.start_tls(ctx, server_hostname=self.ep.host)
        else:                                          # 3.9/3.10 fallback
            protocol = transport.get_protocol()
            new_tr = await loop.start_tls(transport, protocol, ctx,
                                          server_hostname=self.ep.host)
            if new_tr is None:
                raise ConnectionLost("STARTTLS upgrade failed")
            self.writer._transport = new_tr            # noqa: SLF001
            if hasattr(protocol, "_stream_reader") or True:
                try:
                    protocol._transport = new_tr        # noqa: SLF001
                except Exception:
                    pass
        self.caps = set()
        await self._inline_cmd("CAPABILITY")

    async def login(self):
        self.side.state("auth")
        if "AUTH=PLAIN" in self.caps:
            ir = base64.b64encode(f"\0{self.ep.user}\0{self.ep.password}".encode("utf-8")).decode()
            try:
                if "SASL-IR" in self.caps:
                    p = self._register("AUTHENTICATE", ())
                    self._write_raw(f"{p.tag} AUTHENTICATE PLAIN {ir}".encode("latin-1") + CRLF)
                    self._trace("C:", f"{p.tag} AUTHENTICATE PLAIN ****")
                    await self._drain()
                    await p.fut
                else:
                    p = self._register("AUTHENTICATE", ())
                    self._cont_fut = asyncio.get_event_loop().create_future()
                    await self._send_line(f"{p.tag} AUTHENTICATE PLAIN")
                    await asyncio.wait_for(self._cont_fut, timeout=self.cfg.timeout)
                    self._write_raw(ir.encode("latin-1") + CRLF)
                    await self._drain()
                    await p.fut
            except CommandFailed as e:
                raise AuthFailed(str(e))
        elif "LOGINDISABLED" in self.caps:
            raise AuthFailed(f"{self.ep.host}: LOGIN disabled and no AUTH=PLAIN offered")
        else:
            p = self._register("LOGIN", ())
            self._write_raw(f"{p.tag} LOGIN {quote(self.ep.user)} {quote(self.ep.password)}".encode("latin-1") + CRLF)
            self._trace("C:", f"{p.tag} LOGIN {quote(self.ep.user)} ****")
            await self._drain()
            try:
                await p.fut
            except CommandFailed as e:
                raise AuthFailed(str(e))
        if len(self.caps) <= 1:
            pass
        try:
            r = await self.cmd("CAPABILITY", types=("CAPABILITY",))
            _ = r
        except CommandFailed:
            pass
        if "ID" in self.caps:
            try:
                await self.cmd("ID", f'("name" "MailFerry" "version" "{__version__}")', types=("ID",))
            except Exception:
                pass
        self.side.state("ready")

    async def maybe_compress(self):
        if self.cfg.baseline or self.cfg.compress == "off":
            return
        if "COMPRESS=DEFLATE" not in self.caps or self.compressed:
            return
        if self._pending:
            return
        try:
            await self.cmd("COMPRESS", "DEFLATE")
        except CommandFailed:
            return
        # everything already buffered after the OK is compressed stream
        self._inf = zlib.decompressobj(-15)
        self._def = zlib.compressobj(6, zlib.DEFLATED, -15)
        if self._buf:
            pending = bytes(self._buf)
            self._buf.clear()
            self._buf += self._inf.decompress(pending)
        self.compressed = True

    def has(self, cap: str) -> bool:
        return cap.upper() in self.caps

    @property
    def literal_plus(self) -> bool:
        return not self.cfg.baseline and ("LITERAL+" in self.caps or "LITERAL-" in self.caps)

    # ----------------------------------------------------------- mailbox --

    async def select(self, name_wire: str, readonly=True) -> SelectInfo:
        self._sel = SelectInfo()
        self._sel.readonly = readonly
        await self.cmd("EXAMINE" if readonly else "SELECT", quote(name_wire), types=("FLAGS",))
        info = self._sel
        self.side.state(f"selected {name_wire[:24]}")
        return info

    async def status(self, name_wire: str, items="(MESSAGES UIDNEXT UIDVALIDITY)") -> dict:
        r = await self.cmd("STATUS", f"{quote(name_wire)} {items}", types=("STATUS",))
        out = {}
        for resp in r.data:
            toks = resp.tokens
            if len(toks) >= 2 and isinstance(toks[-1], list):
                lst = toks[-1]
                for i in range(0, len(lst) - 1, 2):
                    if isinstance(lst[i], str):
                        out[lst[i].upper()] = as_int(lst[i + 1], 0)
        return out

    async def list_all(self) -> List[Tuple[List[str], Optional[str], str]]:
        r = await self.cmd("LIST", '"" "*"', types=("LIST",))
        entries = []
        for resp in r.data:
            t = resp.tokens
            if len(t) < 3:
                continue
            attrs = [a.upper() for a in t[0] if isinstance(a, str)] if isinstance(t[0], list) else []
            delim = t[1] if isinstance(t[1], str) else None
            name = t[2]
            if isinstance(name, bytes):
                name = name.decode("latin-1")
            elif not isinstance(name, str):
                continue
            entries.append((attrs, delim, name))
        return entries

    async def namespace_info(self) -> Tuple[str, Optional[str]]:
        if "NAMESPACE" not in self.caps:
            return "", None
        try:
            r = await self.cmd("NAMESPACE", types=("NAMESPACE",))
        except CommandFailed:
            return "", None
        for resp in r.data:
            t = resp.tokens
            if t and isinstance(t[0], list) and t[0] and isinstance(t[0][0], list):
                pfx = t[0][0][0] or ""
                delim = t[0][0][1] if len(t[0][0]) > 1 else None
                return (pfx if isinstance(pfx, str) else ""), (delim if isinstance(delim, str) else None)
        return "", None

    async def create(self, name_wire: str) -> bool:
        try:
            await self.cmd("CREATE", quote(name_wire))
            return True
        except CommandFailed as e:
            low = (e.text or "").lower()
            code0 = str(e.code[0]).upper() if e.code else ""
            if "exist" in low or code0 == "ALREADYEXISTS":
                return False
            raise

    async def subscribe(self, name_wire: str):
        try:
            await self.cmd("SUBSCRIBE", quote(name_wire))
        except CommandFailed:
            pass

    async def uid_search_all(self) -> List[Tuple[int, int]]:
        from ..util import parse_imap_set, to_intervals
        r = await self.cmd("UID SEARCH", "ALL", types=("SEARCH", "ESEARCH"))
        uids: List[int] = []
        for resp in r.data:
            if resp.dtype == "SEARCH":
                uids.extend(int(t) for t in resp.tokens if isinstance(t, str) and t.isdigit())
            elif resp.dtype == "ESEARCH":
                toks = resp.tokens
                for i, t in enumerate(toks):
                    if isinstance(t, str) and t.upper() == "ALL" and i + 1 < len(toks):
                        return parse_imap_set(str(toks[i + 1]))
        return to_intervals(uids)

    async def uid_fetch_meta(self, setstr: str, with_headers: bool) -> List[dict]:
        items = "(UID FLAGS INTERNALDATE RFC822.SIZE"
        if with_headers:
            items += " BODY.PEEK[HEADER.FIELDS (MESSAGE-ID DATE FROM TO SUBJECT)]"
        items += ")"
        r = await self.cmd("UID FETCH", f"{setstr} {items}", types=("FETCH",))
        out = []
        for resp in r.data:
            pairs = fetch_pairs(resp.tokens)
            uid = as_int(pairs.get("UID"))
            if uid is None:
                continue
            out.append({
                "uid": uid,
                "flags": flags_of(pairs.get("FLAGS")),
                "date": pairs.get("INTERNALDATE") or "",
                "size": as_int(pairs.get("RFC822.SIZE"), 0) or 0,
                "header": as_bytes(pairs.get("BODY")) if with_headers else None,
            })
        return out

    def body_fetch(self, uid: int) -> BodyHandle:
        bh = BodyHandle(uid)
        self._body_fifo.append(bh)

        async def _send():
            try:
                p = await self.cmd_nowait("UID FETCH", f"{uid} (BODY.PEEK[])")
                bh.pending = p

                def on_done(f):
                    exc = f.exception() if not f.cancelled() else asyncio.CancelledError()
                    if not bh.started:
                        try:
                            self._body_fifo.remove(bh)
                        except ValueError:
                            pass
                        bh.fail(exc if exc else ProtocolError(
                            f"uid {uid}: server returned no body (message vanished?)"))
                p.fut.add_done_callback(on_done)
            except BaseException as e:
                try:
                    self._body_fifo.remove(bh)
                except ValueError:
                    pass
                bh.fail(e)
        asyncio.get_event_loop().create_task(_send())
        return bh

    async def append_begin(self, name_wire: str, flags: str, date: str, size: int) -> AppendHandle:
        async with self._append_lock:
            p = self._register("APPEND", ())
            datepart = f' "{date}"' if date else ""
            marker = "{%d+}" % size if self.literal_plus else "{%d}" % size
            line = f"{p.tag} APPEND {quote(name_wire)} ({flags}){datepart} {marker}"
            if self.literal_plus:
                await self._send_line(line)
            else:
                self._cont_fut = asyncio.get_event_loop().create_future()
                await self._send_line(line)
                try:
                    await asyncio.wait_for(self._cont_fut, timeout=self.cfg.timeout)
                except asyncio.TimeoutError:
                    raise ConnectionLost("timeout waiting for APPEND continuation")
            return AppendHandle(self, p, size)

    @staticmethod
    def appenduid_of(res: CmdResult) -> Optional[int]:
        if res.code and str(res.code[0]).upper() == "APPENDUID" and len(res.code) >= 3:
            v = str(res.code[2])
            if v.isdigit():
                return int(v)
        return None

    async def uid_store_flags(self, uid: int, flags: str):
        await self.cmd("UID STORE", f"{uid} FLAGS.SILENT ({flags})", types=("FETCH",))

    async def noop(self):
        await self.cmd("NOOP")

    async def logout(self):
        try:
            await asyncio.wait_for(self.cmd("LOGOUT"), timeout=10)
        except Exception:
            pass
        self.abort()

    # ------------------------------------------------------------ close --

    async def _watchdog(self):
        try:
            while not self._closed:
                await asyncio.sleep(5)
                busy = bool(self._pending) or bool(self._body_fifo)
                if busy and monotonic() - self._last_activity > self.cfg.timeout:
                    self._shutdown(ConnectionLost(
                        f"no socket activity for {int(monotonic() - self._last_activity)}s"))
                    return
        except asyncio.CancelledError:
            pass

    def _shutdown(self, exc: BaseException):
        if self._closed:
            return
        self._closed = True
        self._eof = True
        self._close_exc = exc
        for p in list(self._pending.values()):
            if not p.fut.done():
                p.fut.set_exception(exc)
        self._pending.clear()
        for bh in list(self._body_fifo):
            bh.fail(exc)
        self._body_fifo.clear()
        if self._cont_fut and not self._cont_fut.done():
            self._cont_fut.set_exception(exc)
        try:
            if self.writer is not None:
                self.writer.close()
        except Exception:
            pass
        self.side.state("closed")

    def abort(self, exc: Optional[BaseException] = None):
        self._shutdown(exc or ConnectionLost("closed"))
        for t in (self._reader_task, self._watch_task):
            if t is not None:
                t.cancel()

    @property
    def alive(self) -> bool:
        return not self._closed
