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

Session log, per-mailbox logs, results.csv, end-of-run summary.
"""
from __future__ import annotations

import csv
import json
import threading
import time
from collections import Counter
from datetime import datetime
from pathlib import Path
from typing import Optional

from . import SLOGAN, banner_line
from .progress.dashboard import c
from .util import fmt_bytes, fmt_dhms, pct, safe_name


class Session:
    """Timestamped, thread-safe master log (appends across runs)."""

    def __init__(self, path, json_path: str = ""):
        self.path = Path(path)
        self.json_path = Path(json_path) if json_path else None
        self._lock = threading.Lock()

    def log(self, msg: str):
        with self._lock:
            ts = datetime.now().isoformat(timespec="seconds")
            try:
                with open(self.path, "a", encoding="utf-8") as f:
                    f.write(f"{ts} {msg}\n")
            except OSError:
                pass
            if self.json_path:
                try:
                    with open(self.json_path, "a", encoding="utf-8") as f:
                        f.write(json.dumps({"ts": ts, "event": msg}) + "\n")
                except OSError:
                    pass


class MailboxLogger:
    """Per-mailbox operational log. Never raises."""

    def __init__(self, path, title: str):
        self.path = Path(path)
        self._lock = threading.Lock()
        self.log(f"# {banner_line()}")
        self.log(f"# {SLOGAN}")
        self.log(f"# Migration log — {title}")

    def log(self, msg: str):
        with self._lock:
            try:
                with open(self.path, "a", encoding="utf-8") as f:
                    f.write(f"{datetime.now().isoformat(timespec='seconds')} {msg}\n")
            except OSError:
                pass


def logger_factory(logs_dir):
    def factory(spec):
        path = Path(logs_dir) / f"{spec.index:03d}_{safe_name(spec.src.user)}.log"
        return MailboxLogger(path, f"{spec.src.label} -> {spec.dst.label}")
    return factory


EXIT_OF = {"SUCCESS": "0", "PARTIAL": "1", "FAILED": "1"}


def write_results_csv(snap, logs_dir) -> Path:
    path = Path(logs_dir) / "results.csv"
    with open(path, "w", newline="", encoding="utf-8") as f:
        w = csv.writer(f)
        w.writerow(["index", "olduser", "newuser", "status", "exit_code", "elapsed_seconds",
                    "log_file", "notes", "error_type", "attempts",
                    "msgs_new", "msgs_adopted", "msgs_skipped", "bytes_done",
                    "folders", "reconnects", "retries"])
        for m in snap["mailboxes"]:
            elapsed = int((m["end"] or time.time()) - m["start"]) if m["start"] else 0
            notes = ""
            if m["status"] == "CANCELLED":
                notes = "interrupted before completion — will be retried on the next run"
            elif m["status"] == "SKIPPED":
                notes = "already completed in a previous run — skipped (--skip-completed)"
            elif m["status"] == "PARTIAL":
                notes = "completed with skipped messages or failed folders — re-run to retry the gaps"
            elif m["status"] == "FAILED":
                notes = m["error"][:300]
            w.writerow([m["index"], m["label"], m["label2"], m["status"], EXIT_OF.get(m["status"], ""),
                        elapsed, m["log"], notes, m["error"][:120],
                        m["attempt"] if m["start"] else "",
                        m["appended"], m["adopted"], m["skipped"], m["bytes_done"],
                        f"{m['fi']}/{m['ft']}" if m["ft"] else "",
                        m["src"]["reconnects"] + m["dst"]["reconnects"], m["retries"]])
    return path


def print_summary(snap, results_path, cfg, runtime_secs: float, interrupted: bool):
    agg, counts = snap["agg"], snap["counts"]
    mbs = snap["mailboxes"]
    ok = [m for m in mbs if m["status"] == "SUCCESS"]
    partial = [m for m in mbs if m["status"] == "PARTIAL"]
    failed = [m for m in mbs if m["status"] == "FAILED"]
    cancelled = [m for m in mbs if m["status"] == "CANCELLED"]
    attempted = len(ok) + len(partial) + len(failed)

    def kv(label, value):
        print(f"{label:<21}: {value}")

    print()
    title = (f"{banner_line()} — run interrupted, partial results" if interrupted
             else f"{banner_line()} — run complete")
    print(c(title, "yellow" if interrupted else "bold"))
    kv("Total runtime", fmt_dhms(runtime_secs))
    kv("Mailboxes in CSV", len(mbs))
    if snap["skipped_prior"]:
        kv("Skipped (done)", f"{snap['skipped_prior']} — previously completed (--skip-completed)")
    kv("Attempted this run", attempted)
    kv("Successful", c(str(len(ok)), "green"))
    if partial:
        kv("Partial", c(str(len(partial)), "yellow"))
    kv("Failed", c(str(len(failed)), "red") if failed else "0")
    if cancelled:
        kv("Cancelled", c(str(len(cancelled)), "yellow"))
    if attempted:
        kv("Success rate", f"{len(ok) * 100 / attempted:.0f}% (of attempted)")
    kv("Messages synced", f"{agg['msgs_done']:,} of {agg['msgs_total']:,} "
                          f"({pct(agg['msgs_done'], agg['msgs_total'])})")
    kv("  copied (new)", f"{agg['appended']:,}")
    if agg["adopted"]:
        kv("  adopted (dup-safe)", f"{agg['adopted']:,} — already on destination, not re-copied")
    if agg["skipped_msgs"]:
        kv("  skipped msgs", c(f"{agg['skipped_msgs']:,} (see per-mailbox logs)", "yellow"))
    kv("Data synced", fmt_bytes(agg["bytes_done"]))
    kv("Wire traffic", f"down {fmt_bytes(agg['wire_rx'])} / up {fmt_bytes(agg['wire_tx'])}")
    if runtime_secs > 0 and agg["wire_tx"]:
        kv("Avg throughput", f"{fmt_bytes(agg['wire_tx'] / runtime_secs)}/s (upload wire)")
    done_timed = [m for m in ok if m["start"] and m["end"]]
    if done_timed:
        durs = [(m["end"] - m["start"], m) for m in done_timed]
        kv("Avg mailbox time", fmt_dhms(sum(d for d, _ in durs) / len(durs)))
        slow = max(durs, key=lambda t: t[0])
        fast = min(durs, key=lambda t: t[0])
        kv("Slowest mailbox", f"{slow[1]['label']} ({fmt_dhms(slow[0])})")
        kv("Fastest mailbox", f"{fast[1]['label']} ({fmt_dhms(fast[0])})")
    if agg["reconnects"]:
        kv("Reconnects", agg["reconnects"])
    if agg["retries"]:
        kv("Retries used", agg["retries"])
    kv("Per-mailbox logs", cfg.logs_dir)
    kv("Session log", str(Path(cfg.logs_dir) / "session.log"))
    kv("Results CSV", str(results_path))
    kv("State Database", "ephemeral (--ephemeral) — nothing persisted" if cfg.ephemeral else cfg.db_path)

    problems = failed + partial
    if problems or cancelled:
        reasons = Counter((m["error"] or "unknown error") for m in problems if m["error"])
        if reasons:
            print()
            print(c("Error summary:", "red"))
            for reason, n in reasons.most_common():
                print(f"  {n} × {reason[:140]}")
        if failed:
            print(c("Failed mailboxes:", "red"))
            for m in failed:
                att = f" (after {m['attempt']} attempts)" if m["attempt"] > 1 else ""
                print(f"  - {m['label']} — {m['error'][:140]}{att}  (log: {m['log'] or '-'})")
        if partial:
            print(c("Partial mailboxes (gaps will be retried next run):", "yellow"))
            for m in partial:
                print(f"  - {m['label']} — skipped {m['skipped']}, error: {m['error'][:120] or '-'}")
        if cancelled:
            print(c(f"Cancelled mailboxes ({len(cancelled)}):", "yellow"))
            for m in cancelled:
                print(f"  - {m['label']}")
        if not cfg.ephemeral:
            print()
            print(c("Resume: re-run the same command — completed messages are never "
                    "re-copied (per-UID state + fingerprint adoption).", "cyan"))
