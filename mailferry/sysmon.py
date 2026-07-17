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

System resource sampling — informational only, zero subprocesses.
Linux via /proc; macOS via ctypes (host_statistics / sysctl); else N/A.
"""
from __future__ import annotations

import os
import sys
import threading


class SysMon:
    INTERVAL = 1.5

    def __init__(self):
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._thread = None
        self._cpu_prev = None
        self._mac = None
        self._mac_total = None
        self._vals = {"cpu": None, "load": None, "mem_total": None,
                      "mem_used": None, "rss": None}

    def start(self):
        self._thread = threading.Thread(target=self._loop, daemon=True, name="mf-sysmon")
        self._thread.start()

    def stop(self):
        self._stop.set()

    def snapshot(self) -> dict:
        with self._lock:
            return dict(self._vals)

    def _loop(self):
        while not self._stop.is_set():
            vals = {"cpu": self._try(self._cpu), "load": self._try(self._load),
                    "rss": self._try(self._rss)}
            mem = self._try(self._mem)
            vals["mem_total"], vals["mem_used"] = (mem or (None, None))
            with self._lock:
                self._vals.update(vals)
            self._stop.wait(self.INTERVAL)

    @staticmethod
    def _try(fn):
        try:
            return fn()
        except Exception:
            return None

    def _cpu_delta(self, idle, total):
        prev, self._cpu_prev = self._cpu_prev, (idle, total)
        if prev is None:
            return None
        di, dt = idle - prev[0], total - prev[1]
        if dt <= 0:
            return None
        return max(0.0, min(100.0, 100.0 * (dt - di) / dt))

    def _cpu(self):
        if sys.platform.startswith("linux"):
            with open("/proc/stat") as f:
                parts = f.readline().split()
            ticks = [int(x) for x in parts[1:9]]
            idle = ticks[3] + (ticks[4] if len(ticks) > 4 else 0)
            return self._cpu_delta(idle, sum(ticks))
        if sys.platform == "darwin":
            import ctypes
            if self._mac is None:
                libc = ctypes.CDLL(None, use_errno=True)
                libc.mach_host_self.restype = ctypes.c_uint
                self._mac = (libc, libc.mach_host_self())
            libc, host = self._mac
            info = (ctypes.c_uint32 * 4)()
            count = ctypes.c_uint32(4)
            if libc.host_statistics(host, 3, ctypes.byref(info), ctypes.byref(count)) != 0:
                return None
            user, system, idle, nice = (int(x) for x in info)
            return self._cpu_delta(idle, user + system + idle + nice)
        return None

    @staticmethod
    def _load():
        if hasattr(os, "getloadavg"):
            return os.getloadavg()
        return None

    def _mem(self):
        if sys.platform.startswith("linux"):
            info = {}
            with open("/proc/meminfo") as f:
                for line in f:
                    k, _, rest = line.partition(":")
                    fs = rest.split()
                    if fs:
                        info[k.strip()] = float(fs[0]) * 1024.0
            total = info.get("MemTotal")
            if not total:
                return None
            avail = info.get("MemAvailable")
            if avail is None:
                avail = info.get("MemFree", 0.0) + info.get("Buffers", 0.0) + info.get("Cached", 0.0)
            return total, total - avail
        if sys.platform == "darwin":
            import ctypes
            if self._mac_total is None:
                libc = ctypes.CDLL(None, use_errno=True)
                size = ctypes.c_uint64(0)
                sz = ctypes.c_size_t(ctypes.sizeof(size))
                if libc.sysctlbyname(b"hw.memsize", ctypes.byref(size), ctypes.byref(sz), None, 0) != 0:
                    return None
                self._mac_total = float(size.value)
            return self._mac_total, None
        return None

    @staticmethod
    def _rss():
        if sys.platform.startswith("linux"):
            with open("/proc/self/statm") as f:
                pages = int(f.read().split()[1])
            return pages * os.sysconf("SC_PAGE_SIZE")
        import resource
        ru = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
        return ru * (1024 if sys.platform.startswith("linux") else 1)
