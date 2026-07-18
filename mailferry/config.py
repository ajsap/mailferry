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

Configuration dataclasses + CSV input (wrapper-compatible, FS §3).
"""
from __future__ import annotations

import csv
from dataclasses import dataclass, field
from pathlib import Path
from typing import Dict, List, Optional

SAMPLE_CSV = """oldhost,oldport,oldsecurity,olduser,oldpassword,newhost,newport,newsecurity,newuser,newpassword
imap.example.com,993,ssl,jane@example.com,Secret1,imap.example.org,993,ssl,jane@example.org,Secret2
"""

REQUIRED_COLS = {"oldhost", "oldport", "oldsecurity", "olduser", "oldpassword",
                 "newhost", "newport", "newsecurity", "newuser", "newpassword"}

IDENTITY_COLS = ("oldhost", "olduser", "newhost", "newuser")


def mailbox_key(row: Dict[str, str]) -> str:
    """Wrapper-compatible identity key (FS §8)."""
    return "\x1f".join((row.get(k) or "").strip().lower() for k in IDENTITY_COLS)


def _security(v: str) -> str:
    s = (v or "").strip().lower()
    if s in ("ssl", "imaps"):
        return "ssl"
    if s in ("tls", "starttls"):
        return "tls"
    return "none"


@dataclass(frozen=True)
class Endpoint:
    host: str
    port: int
    security: str          # ssl | tls | none
    user: str
    password: str

    @property
    def label(self) -> str:
        return f"{self.user}@{self.host}"


@dataclass
class MailboxSpec:
    index: int
    total: int
    src: Endpoint
    dst: Endpoint
    row: Dict[str, str]

    @property
    def key(self) -> str:
        return mailbox_key(self.row)

    @property
    def label(self) -> str:
        return self.src.user


@dataclass
class RunConfig:
    csv_file: str = ""
    workers: int = 10
    logs_dir: str = "./logs"
    db_path: str = "./migration.db"
    ephemeral: bool = False
    force: bool = False
    skip_completed: bool = False
    retries: int = 2
    retry_delay: float = 30.0
    order: str = "csv"                      # csv | size
    max_conns_per_mailbox: int = 3
    per_host_conns: int = 8
    timeout: float = 120.0                  # inactivity watchdog seconds
    stale_timeout: float = 300.0            # no-progress stale detection (0 = off)
    recovery_retries: int = 3               # automatic recovery attempts per stall
    recovery_interval: float = 30.0         # spacing between recovery attempts
    lock_timeout: float = 300.0             # legacy lease considered stale after this
    reset_stale_locks: bool = False         # kept for compatibility (cluster reclaims)
    worker_timeout: float = 60.0            # cluster worker offline after this silence
    batch_attempts: int = 3                 # tries per level during failure isolation
    reconnect_attempts: int = 5             # folder reconnects for ordinary trouble
    isolate_failed: bool = True             # progressive poison-message isolation
    skip_known_failed: bool = True          # honour the Failed Message Registry
    log_keep_days: int = 30                 # prune old logs at startup (0 = keep)
    db_heartbeat: float = 15.0              # worker/lease heartbeat interval
    compress: str = "auto"                  # auto | off
    baseline: bool = False
    tls_verify: bool = True
    include: List[str] = field(default_factory=list)
    exclude: List[str] = field(default_factory=list)
    map_file: str = ""
    gmail_all_mail: bool = False
    subscribe: bool = False
    sync_flags: bool = False
    rescan_dest: bool = False
    no_dedup_scan: bool = False
    json_logs: bool = False
    json_progress: bool = False
    no_tui: bool = False
    trace: bool = False
    debug: bool = False
    check_only: bool = False
    msg_retries: int = 3
    batch_bytes: int = 8 * 1024 * 1024      # in-flight window per folder task
    fetch_window: int = 8                   # pipelined body fetches
    append_window: int = 8                  # unacknowledged appends
    run_id: str = ""

    def folder_map(self) -> Dict[str, str]:
        out: Dict[str, str] = {}
        if not self.map_file:
            return out
        for line in Path(self.map_file).read_text(encoding="utf-8").splitlines():
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            a, b = line.split("=", 1)
            out[a.strip()] = b.strip()
        return out


def parse_csv(path: str) -> List[MailboxSpec]:
    p = Path(path)
    if not p.exists():
        raise SystemExit(f"CSV file not found: {p}")
    with open(p, newline="", encoding="utf-8-sig") as f:
        reader = csv.DictReader(f)
        missing = REQUIRED_COLS - set(reader.fieldnames or [])
        if missing:
            raise SystemExit(f"CSV missing columns: {', '.join(sorted(missing))}")
        rows = [r for r in reader if any((v or "").strip() for v in r.values())]
    if not rows:
        raise SystemExit("CSV has no data rows.")
    specs: List[MailboxSpec] = []
    for i, row in enumerate(rows):
        try:
            src = Endpoint(row["oldhost"].strip(), int(row["oldport"]), _security(row["oldsecurity"]),
                           row["olduser"].strip(), row["oldpassword"])
            dst = Endpoint(row["newhost"].strip(), int(row["newport"]), _security(row["newsecurity"]),
                           row["newuser"].strip(), row["newpassword"])
        except (KeyError, ValueError) as e:
            raise SystemExit(f"CSV row {i + 2}: invalid value ({e})")
        specs.append(MailboxSpec(index=i + 1, total=len(rows), src=src, dst=dst, row=row))
    return specs


def write_template(path: str):
    Path(path).write_text(SAMPLE_CSV, encoding="utf-8")
