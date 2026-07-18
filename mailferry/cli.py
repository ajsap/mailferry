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

mailferry CLI — run / init / check / import-state / capabilities / verify / compact.

Wrapper-compatible ergonomics: `mailferry mailboxes.csv --workers 6` works,
exit codes are 0 (ok) / 1 (failures) / 130 (interrupted) / 141 (broken pipe).
"""
from __future__ import annotations

import argparse
import asyncio
import os
import signal
import sys
import time
from datetime import datetime
from pathlib import Path

from . import (PRODUCT, REPOSITORY, SLOGAN, SUPPORT_URL, __version__,
               about_text, banner_line, version_text)
from .config import Endpoint, RunConfig, parse_csv, write_template
from .control import ControlHub
from .tui.app import TuiApp
from .tui.keys import KeyReader
from .errors import MFError, reason_of
from .progress.dashboard import ISATTY, Renderer, c
from .progress.stats import Stats
from .report import (Session, logger_factory, print_failed_section,
                     print_summary, write_failed_report, write_results_csv)
from .sysmon import SysMon
from .util import fmt_bytes, fmt_dhms

COMMANDS = {"run", "resume", "validate", "check", "doctor", "benchmark",
            "init", "import-state", "capabilities", "verify", "compact",
            "changelog", "roadmap", "failed", "retry-failed", "config"}


def _add_run_opts(p: argparse.ArgumentParser, check=False):
    p.add_argument("csv_file", help="CSV file with migration rows (wrapper-compatible columns)")
    p.add_argument("--workers", type=int, default=10, help="Concurrent mailboxes (default 10)")
    p.add_argument("--logs-dir", default="./logs", help="Directory for logs (default ./logs)")
    p.add_argument("--db", default="./migration.db", help="State database (default ./migration.db)")
    p.add_argument("--ephemeral", action="store_true",
                   help="In-memory state only: no resume data is persisted (old --no-state)")
    p.add_argument("--force", action="store_true",
                   help="Re-verify everything: full replan + destination rescan (never blind re-copy)")
    p.add_argument("--skip-completed", action="store_true",
                   help="Skip mailboxes recorded SUCCESS instead of giving them an incremental pass")
    p.add_argument("--retries", type=int, default=2,
                   help="Automatic retries per mailbox for transient failures (default 2). "
                        "Authentication failures are never auto-retried.")
    p.add_argument("--retry-delay", type=float, default=30.0, metavar="SECONDS",
                   help="Base backoff before the first retry; doubles each time (default 30)")
    p.add_argument("--no-retry", action="store_true", help="Same as --retries 0")
    p.add_argument("--order", choices=("csv", "size"), default="csv",
                   help="Mailbox admission order (size = largest known first)")
    p.add_argument("--max-conns-per-mailbox", type=int, default=3,
                   help="Max parallel folder connection pairs per mailbox (default 3)")
    p.add_argument("--per-host-conns", type=int, default=8,
                   help="Max simultaneous connections per server host (default 8)")
    p.add_argument("--timeout", type=float, default=120.0,
                   help="Inactivity watchdog per connection, seconds (default 120)")
    p.add_argument("--stale-timeout", type=float, default=300.0, metavar="SECONDS",
                   help="Mark a mailbox stale after this long without measurable "
                        "progress and recover it automatically (default 300 = 5 min; 0 disables)")
    p.add_argument("--recovery-retries", type=int, default=3,
                   help="Automatic recovery attempts per stall before marking the "
                        "mailbox STALE (default 3)")
    p.add_argument("--recovery-interval", type=float, default=30.0, metavar="SECONDS",
                   help="Wait between automatic recovery attempts (default 30)")
    p.add_argument("--lock-timeout", type=float, default=300.0, metavar="SECONDS",
                   help="An instance lock with no heartbeat for this long is stale "
                        "and reset automatically (default 300 = 5 min)")
    p.add_argument("--reset-stale-locks", action="store_true",
                   help="Compatibility no-op: the cluster now reclaims jobs from dead "
                        "workers automatically (see --worker-timeout)")
    p.add_argument("--worker-timeout", type=float, default=60.0, metavar="SECONDS",
                   help="A cluster worker with no heartbeat for this long is offline; "
                        "its mailboxes are reclaimed automatically (default 60)")
    p.add_argument("--compress", choices=("auto", "off"), default="auto",
                   help="COMPRESS=DEFLATE when the server offers it (default auto)")
    p.add_argument("--baseline", action="store_true",
                   help="RFC-3501-only conservative mode (no LITERAL+/COMPRESS extensions)")
    p.add_argument("--tls-no-verify", action="store_true",
                   help="Disable TLS certificate verification (verification is ON by default)")
    p.add_argument("--include", action="append", default=[], metavar="GLOB",
                   help="Only sync folders matching GLOB (repeatable)")
    p.add_argument("--exclude", action="append", default=[], metavar="GLOB",
                   help="Skip folders matching GLOB (repeatable)")
    p.add_argument("--map", default="", metavar="FILE", dest="map_file",
                   help="Folder mapping file: lines of 'Source Name = Dest Name'")
    p.add_argument("--gmail-all-mail", action="store_true",
                   help="Include Gmail [Gmail]/All Mail and Important (skipped by default)")
    p.add_argument("--subscribe", action="store_true", help="SUBSCRIBE created folders")
    p.add_argument("--sync-flags", action="store_true",
                   help="Re-apply changed flags to already-synced messages (backup mode)")
    p.add_argument("--rescan-dest", action="store_true",
                   help="Force a fresh destination fingerprint scan (re-adoption)")
    p.add_argument("--no-dedup-scan", action="store_true",
                   help="Skip destination duplicate scan (only for guaranteed-empty destinations)")
    p.add_argument("--json-logs", action="store_true", help="Also write NDJSON event log")
    p.add_argument("--json-progress", action="store_true", help="Write NDJSON progress snapshots")
    p.add_argument("--no-tui", "--no-console", action="store_true", dest="no_tui",
                   help="Disable the Terminal User Interface; print plain status lines instead")
    p.add_argument("--trace", action="store_true", help="Protocol-level trace in per-mailbox logs")
    p.add_argument("--debug", action="store_true", help="Full tracebacks")
    if not check:
        p.add_argument("--check", "--dry-run", action="store_true", dest="check",
                       help="Preflight only: connect, authenticate, list, estimate — write nothing")
    # loudly rejected wrapper flags
    for flag, why in (("--imapsync-path", "MailFerry speaks IMAP natively — no imapsync needed"),
                      ("--extra-args", "MailFerry has first-class options for everything — see --help"),
                      ("--split-size", "MailFerry's adaptive batching replaced --split-size"),
                      ("--skip-duplicate-check", "the State Database makes duplicate checks O(1); "
                                                 "see --no-dedup-scan for empty destinations"),
                      ("--darwinfix", "no Perl process to patch"),
                      ("--state-file", "state moved to the State Database: use --db (see import-state)"),
                      ("--no-state", "renamed: use --ephemeral")):
        p.add_argument(flag, action="store", nargs="?", help=argparse.SUPPRESS,
                       dest=f"_obsolete_{flag.strip('-').replace('-', '_')}", metavar="",
                       default=None)
        p.set_defaults(**{f"_obsolete_why_{flag.strip('-').replace('-', '_')}": why})


def _cfg_from(args) -> RunConfig:
    for k, v in vars(args).items():
        if k.startswith("_obsolete_") and not k.startswith("_obsolete_why_") and v is not None:
            flag = "--" + k[len("_obsolete_"):].replace("_", "-")
            why = getattr(args, f"_obsolete_why_{k[len('_obsolete_'):]}", "")
            raise SystemExit(f"{flag} is obsolete in MailFerry: {why}")
    return RunConfig(
        csv_file=args.csv_file, workers=max(1, args.workers), logs_dir=args.logs_dir,
        db_path=args.db, ephemeral=args.ephemeral, force=args.force,
        skip_completed=args.skip_completed,
        retries=0 if args.no_retry else max(0, args.retries),
        retry_delay=max(1.0, args.retry_delay), order=args.order,
        max_conns_per_mailbox=max(1, args.max_conns_per_mailbox),
        per_host_conns=max(2, args.per_host_conns), timeout=max(20.0, args.timeout),
        stale_timeout=max(0.0, args.stale_timeout),
        recovery_retries=max(1, args.recovery_retries),
        recovery_interval=max(1.0, args.recovery_interval),
        lock_timeout=max(30.0, args.lock_timeout),
        reset_stale_locks=args.reset_stale_locks,
        worker_timeout=max(20.0, args.worker_timeout),
        batch_attempts=max(1, int(getattr(args, "batch_attempts", 3))),
        reconnect_attempts=max(1, int(getattr(args, "reconnect_attempts", 5))),
        isolate_failed=bool(getattr(args, "isolate_failed", True)),
        skip_known_failed=bool(getattr(args, "skip_known_failed", True)),
        log_keep_days=max(0, int(getattr(args, "log_keep_days", 30))),
        db_heartbeat=max(5.0, float(getattr(args, "db_heartbeat", 15.0))),
        compress=args.compress, baseline=args.baseline, tls_verify=not args.tls_no_verify,
        include=args.include, exclude=args.exclude, map_file=args.map_file,
        gmail_all_mail=args.gmail_all_mail, subscribe=args.subscribe,
        sync_flags=args.sync_flags,
        rescan_dest=args.rescan_dest or args.force,
        no_dedup_scan=args.no_dedup_scan and not args.force,
        json_logs=args.json_logs, json_progress=args.json_progress,
        no_tui=getattr(args, "no_tui", False),
        trace=args.trace or getattr(args, "log_level", "info") == "trace",
        debug=args.debug or getattr(args, "log_level", "info") == "debug",
        check_only=getattr(args, "check", False),
        run_id=datetime.now().strftime("%Y%m%d-%H%M%S"),
    )


# ---------------------------------------------------------------- run ----

def _quiet_async_handler(session):
    """Route stray asyncio errors to the session log — never spray the
    Dashboard or the terminal with tracebacks."""
    def handler(loop, context):
        exc = context.get("exception")
        msg = context.get("message") or ""
        try:
            session.log(f"async: {msg}" + (f" ({exc!r})" if exc else ""))
        except Exception:
            pass
    return handler


def _finish_shutdown_dialog(hub, renderer, session):
    """Walk the remaining shutdown phases to completion so the dialog shows
    every task finishing, then hold the completed dialog briefly. The engine
    has already closed connections and the State Database by this point; these
    steps are the final flush/release, surfaced truthfully at a coarse grain."""
    import time as _t
    for key in ("scheduler", "workers", "state", "logs", "conns", "resources"):
        hub.set_phase(key, "done")
        renderer.wake.set()
        _t.sleep(0.28)
    session.log("shutdown complete — resources released")
    hub.set_phase("done", "done")
    renderer.wake.set()
    _t.sleep(0.7)                               # let the completed dialog register


def cmd_run(args) -> int:
    cfg = _cfg_from(args)
    specs = parse_csv(cfg.csv_file)
    if cfg.check_only:
        return _run_check(cfg, specs)
    logs = Path(cfg.logs_dir)
    logs.mkdir(parents=True, exist_ok=True)
    if cfg.log_keep_days > 0:
        cutoff = time.time() - cfg.log_keep_days * 86400
        for old in list(logs.glob("*.log")) + list(logs.glob("*.ndjson")):
            try:
                if old.stat().st_mtime < cutoff:
                    old.unlink()
            except OSError:
                pass
    stats = Stats()
    stats.csv_file = Path(cfg.csv_file).name
    stats.db_path = "(ephemeral)" if cfg.ephemeral else cfg.db_path
    stats.logs_dir = str(logs)
    stats.mode = "Migration & Sync"
    session = Session(logs / "session.log",
                      str(logs / f"events-{cfg.run_id}.ndjson") if cfg.json_logs else "")
    if cfg.workers > 20:
        print("Note: >20 concurrent mailboxes — some servers throttle or cap simultaneous "
              "IMAP logins per account/IP. Lower --workers if you see auth/connection errors.")
    print(banner_line())
    print(SLOGAN)
    print(f"\nStarting migration: {len(specs)} mailbox(es), "
          f"{min(cfg.workers, len(specs))} worker(s), State Database {stats.db_path}")
    session.log(f"=== {banner_line()} — run {cfg.run_id} start: csv={stats.csv_file} "
                f"rows={len(specs)} workers={cfg.workers} db={stats.db_path}"
                + (" force" if cfg.force else "") + (" ephemeral" if cfg.ephemeral else ""))

    sysmon = SysMon()
    sysmon.start()
    hub = ControlHub(cfg, stats, session)
    session.hub = hub                       # session log lines feed the Logs view
    renderer = Renderer(stats, sysmon,
                        str(logs / f"progress-{cfg.run_id}.ndjson") if cfg.json_progress else "",
                        hub=hub)
    tui = None
    keys = None
    if ISATTY and sys.stdin.isatty() and not cfg.no_tui:
        tui = TuiApp(cfg, stats, hub, sysmon)
        tui.refresh = max(0.05, float(getattr(args, "refresh_ms", 250)) / 1000.0)
        tui.wake = renderer.wake
        tui.renderer = renderer             # classic dashboard renders via it
        renderer.tui = tui
        keys = KeyReader(tui.handle_key)
        hub.tui_attached = True             # stale-lock conflicts open a dialog
    renderer.start()
    if keys is not None:
        keys.start()
    batch_start = time.time()
    interrupted = {"n": 0}
    snap = None
    closed = {"done": False}

    def shutdown_ui(final=True):
        """Exactly-once UI teardown: restore the terminal no matter what."""
        if closed["done"]:
            return
        closed["done"] = True
        if keys is not None:
            keys.stop()
        sysmon.stop()
        renderer.close(final=final)

    loop = asyncio.new_event_loop()
    asyncio.set_event_loop(loop)
    holder = {}

    async def root():
        stop_event = asyncio.Event()
        holder["stop"] = stop_event
        rl = asyncio.get_event_loop()
        rl.set_exception_handler(_quiet_async_handler(session))
        hub.loop = rl
        hub.stop_event = stop_event
        from .engine.scheduler import run_migration
        return await run_migration(cfg, specs, stats, session, logger_factory(logs),
                                   stop_event, hub=hub)

    task = loop.create_task(root())
    hub.hard_abort = task.cancel

    GRACE_SECONDS = 6.0

    def on_signal():
        interrupted["n"] += 1
        stats.interrupted = True
        if interrupted["n"] == 1 and "stop" in holder:
            hub.begin_shutdown()               # show the graceful shutdown dialog
            holder["stop"].set()
            session.log("interrupt received (Ctrl+C / SIGTERM) — graceful stop: no new work, "
                        "active Workers finish the current message, then connections close")
            # Bounded escalation: if workers are stuck on a stalled socket,
            # close all connections after a short grace so shutdown never
            # hangs waiting on the network. State stays consistent (resume).
            def _escalate():
                if not task.done():
                    hub.abort_all_connections()
            loop.call_later(GRACE_SECONDS, _escalate)
        else:
            hub.shutdown_forced = True
            session.log("second interrupt — immediate abort (state stays consistent)")
            hub.abort_all_connections()
            task.cancel()

    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(sig, on_signal)
        except (NotImplementedError, RuntimeError):
            pass
    try:
        snap = loop.run_until_complete(task)
    except asyncio.CancelledError:
        pass
    except KeyboardInterrupt:
        stats.interrupted = True
    except BaseException:
        shutdown_ui(final=False)
        raise
    finally:
        for sig in (signal.SIGINT, signal.SIGTERM):
            try:
                loop.remove_signal_handler(sig)
            except Exception:
                pass
        try:
            pending = [t for t in asyncio.all_tasks(loop) if not t.done()]
            for t in pending:
                t.cancel()
            if pending:
                loop.run_until_complete(asyncio.gather(*pending, return_exceptions=True))
        except Exception:
            pass
        loop.close()

    was_interrupted = stats.interrupted
    with stats.lock:
        for mb in stats.mailboxes.values():
            if mb.status in ("QUEUED", "RUNNING", "RETRYING"):
                mb.status = "CANCELLED"
                mb.end_time = mb.end_time or time.time()
    if snap is None:
        snap = stats.snapshot()
    if hub.shutdown_active:
        _finish_shutdown_dialog(hub, renderer, session)
    shutdown_ui(final=True)
    if was_interrupted:
        print(c("\nInterrupted — per-UID state is committed; re-run the same command to resume "
                "exactly where this stopped.", "yellow"))
    results = write_results_csv(snap, logs)
    runtime = time.time() - batch_start
    counts = snap["counts"]
    session.log(f"=== {PRODUCT} v{__version__} run {cfg.run_id} end: runtime={fmt_dhms(runtime)} "
                f"ok={counts.get('SUCCESS', 0)} warnings={counts.get('WARNINGS', 0)} "
                f"partial={counts.get('PARTIAL', 0)} "
                f"failed={counts.get('FAILED', 0)} stale={counts.get('STALE', 0)} "
                f"cancelled={counts.get('CANCELLED', 0)}"
                + (" (interrupted)" if was_interrupted else ""))
    print_summary(snap, results, cfg, runtime, was_interrupted)
    freg = getattr(hub, "failed_registry", []) or []
    fpath = write_failed_report(freg, logs)
    print_failed_section(freg, fpath)
    if was_interrupted:
        return 130
    if counts.get("FAILED", 0) or counts.get("PARTIAL", 0) or counts.get("STALE", 0):
        return 1
    return 0    # WARNINGS completes the run: failures are recorded, not fatal


# -------------------------------------------------------------- check ----

def _run_check(cfg: RunConfig, specs) -> int:
    from .engine.planner import build_plan
    from .imap.client import ImapClient

    async def probe(spec):
        src = ImapClient(spec.src, cfg, role="src")
        dst = ImapClient(spec.dst, cfg, role="dst")
        try:
            await src.connect()
            await src.login()
            await dst.connect()
            await dst.login()
            plans = await build_plan(src, dst, cfg, cfg.folder_map())
            msgs = sum(p.est_msgs for p in plans)
            size = sum(p.est_bytes for p in plans)
            caps = f"src[{'COMPRESS ' if src.has('COMPRESS=DEFLATE') else ''}" \
                   f"{'UIDPLUS ' if src.has('UIDPLUS') else ''}" \
                   f"{'LIT+ ' if src.literal_plus else ''}]".replace(" ]", "]")
            dcaps = f"dst[{'UIDPLUS ' if dst.has('UIDPLUS') else ''}" \
                    f"{'LIT+ ' if dst.literal_plus else ''}]".replace(" ]", "]")
            print(f"  OK  {spec.src.label:<38} -> {spec.dst.label:<38} "
                  f"folders={len(plans):<4} msgs={msgs:<8} size={fmt_bytes(size) if size else '?':<10} "
                  f"{caps} {dcaps}")
            return True
        except BaseException as e:
            print(f"  FAIL {spec.src.label:<37} -> {spec.dst.label:<38} {reason_of(e)}")
            return False
        finally:
            for cli in (src, dst):
                try:
                    await cli.logout()
                except Exception:
                    cli.abort()

    async def main():
        print(f"MailFerry preflight check — {len(specs)} mailbox(es); nothing will be written.")
        sem = asyncio.Semaphore(4)

        async def one(s):
            async with sem:
                return await probe(s)
        results = await asyncio.gather(*(one(s) for s in specs))
        okn = sum(1 for r in results if r)
        print(f"\nCheck complete: {okn}/{len(specs)} mailbox(es) ready.")
        return 0 if okn == len(specs) else 1

    return asyncio.new_event_loop().run_until_complete(main())


# ------------------------------------------------------------- others ----

def cmd_capabilities(args) -> int:
    cfg = RunConfig(tls_verify=not args.tls_no_verify, timeout=30.0)
    ep = Endpoint(args.host, args.port, args.security, args.user or "", args.password or "")
    from .imap.client import ImapClient

    async def main():
        cli = ImapClient(ep, cfg)
        await cli.connect()
        print(f"Greeting : {cli.server_greeting}")
        print(f"Pre-auth : {' '.join(sorted(cli.caps))}")
        if ep.user:
            await cli.login()
            print(f"Post-auth: {' '.join(sorted(cli.caps))}")
        plan = []
        for cap, use in (("UIDPLUS", "exact UID mapping (APPENDUID)"),
                         ("LITERAL+", "non-blocking uploads"),
                         ("COMPRESS=DEFLATE", "wire compression"),
                         ("CONDSTORE", "MODSEQ delta sync"),
                         ("QRESYNC", "quick resync"),
                         ("MULTIAPPEND", "batched appends (reserved)"),
                         ("SPECIAL-USE", "role-based folder mapping"),
                         ("STATUS=SIZE", "byte-accurate ETAs"),
                         ("APPENDLIMIT", "oversize preflight"),
                         ("NAMESPACE", "prefix detection"),
                         ("ID", "client identification")):
            has = any(c0 == cap or c0.startswith(cap + "=") for c0 in cli.caps)
            plan.append(f"  {'✓' if has else '·'} {cap:<18} {use}")
        print("Optimisation plan:")
        print("\n".join(plan))
        await cli.logout()
        return 0

    return asyncio.new_event_loop().run_until_complete(main())


def cmd_import_state(args) -> int:
    from .state.db import StateDB
    db = StateDB(args.db)
    n = db.import_wrapper_state(args.statefile)
    db.close()
    print(f"Imported {n} completed mailbox record(s) from {args.statefile} into {args.db}.")
    print("Those mailboxes will get a cheap incremental pass (or be skipped with --skip-completed).")
    return 0


def cmd_compact(args) -> int:
    from .state.db import StateDB
    db = StateDB(args.db)
    n = db.compact()
    db.close()
    print(f"Removed {n} per-message rows for completed folders (aggregates kept).")
    return 0


def cmd_changelog(args) -> int:
    from .meta import changelog_latest, read_changelog
    if getattr(args, "full", False):
        print(read_changelog().rstrip("\n"))
        return 0
    version = (args.version or __version__).lstrip("v")
    print(f"{banner_line()}\n")
    print(f"Changelog for v{version} "
          f"(full history: {REPOSITORY}/blob/main/CHANGELOG.md):\n")
    print(changelog_latest(version).rstrip("\n"))
    return 0


def cmd_roadmap(args) -> int:
    from .roadmap import roadmap_lines
    print(f"{banner_line()}\n{SLOGAN}\n")
    print(f"Project roadmap (aspirational — see {REPOSITORY}):\n")
    print("\n".join(roadmap_lines()))
    return 0


def _registry_db(args):
    from .state.db import StateDB
    if not Path(args.db).exists():
        print(c(f"State Database not found: {args.db}", "red"), file=sys.stderr)
        return None
    return StateDB(args.db)


def _registry_mid(db, mailbox: str):
    if not mailbox:
        return None
    row = db.run_sync(lambda: db._con.execute(
        "SELECT id FROM mailboxes WHERE src_user=? OR key LIKE ?",
        (mailbox, f"%{mailbox}%")).fetchone())
    return row["id"] if row else -1


def cmd_failed(args) -> int:
    """List / export / ignore Failed Message Registry entries."""
    import asyncio as aio
    import json as jsonmod
    db = _registry_db(args)
    if db is None:
        return 1
    loop = aio.new_event_loop()
    try:
        mid = _registry_mid(db, args.mailbox)
        if mid == -1:
            print(c(f"mailbox not found: {args.mailbox}", "red"), file=sys.stderr)
            return 1
        statuses = () if args.all else ("FAILED", "RETRY_PENDING", "RETRYING")
        rows = loop.run_until_complete(db.failed_rows(mid, statuses))
        if args.ignore:
            n = loop.run_until_complete(db.set_failed_status(
                "IGNORED", mid, "", None))
            print(f"marked {n} registry entr{'y' if n == 1 else 'ies'} IGNORED "
                  "(still skipped; no longer counted as outstanding)")
            return 0
        if args.json:
            print(jsonmod.dumps(rows, indent=2, default=str))
            return 0
        if args.csv:
            from .report import write_failed_report
            out = write_failed_report(rows, Path(args.csv).parent or Path("."))
            if out:
                final = Path(args.csv)
                out.replace(final)
                print(f"exported {len(rows)} entr{'y' if len(rows) == 1 else 'ies'} to {final}")
            else:
                print("nothing to export — the registry is clean")
            return 0
        print(f"{banner_line()}\n{SLOGAN}\n")
        if not rows:
            print(c("Failed Message Registry is clean — no outstanding failures.", "green"))
            return 0
        print(f"Failed Message Registry — {len(rows)} entr{'y' if len(rows) == 1 else 'ies'}"
              + (f" for {args.mailbox}" if args.mailbox else "") + ":\n")
        hdr = f"{'Mailbox':<26} {'Folder':<14} {'UID':>6} {'Size':>9} {'Type':<16} {'Status':<13} Subject"
        print(c(hdr, "bold"))
        for r in rows:
            print(f"{r.get('mailbox', '?')[:25]:<26} {r['folder'][:13]:<14} "
                  f"{r['src_uid']:>6} {fmt_bytes(r['size']):>9} {r['ftype']:<16} "
                  f"{r['status']:<13} {(r['subject'] or '-')[:38]}")
            print(c(f"{'':<26} {(r['sender'] or '-')[:40]} · {(r['date'] or '-')[:26]} · "
                    f"failed {r['fail_count']}x · {(r['reason'] or '-')[:70]}", "dim"))
        print(c("\nRetry: mailferry retry-failed [--mailbox USER] [--folder F --uid N] "
                "· Export: --csv FILE / --json · Silence: --ignore", "cyan"))
        return 0
    finally:
        loop.close()
        db.close()


def cmd_retry_failed(args) -> int:
    """Re-queue registry entries: status RETRY_PENDING + rows re-planned."""
    import asyncio as aio
    db = _registry_db(args)
    if db is None:
        return 1
    loop = aio.new_event_loop()
    try:
        mid = _registry_mid(db, args.mailbox)
        if mid == -1:
            print(c(f"mailbox not found: {args.mailbox}", "red"), file=sys.stderr)
            return 1
        n = loop.run_until_complete(db.set_failed_status(
            "RETRY_PENDING", mid, args.folder, args.uid))
        if not n:
            print("no matching registry entries to retry")
            return 0
        print(f"{n} failed message(s) re-queued (RETRY_PENDING).")
        print("Run the migration again — they will be retried; successes become "
              "RECOVERED, repeat failures return to the registry.")
        return 0
    finally:
        loop.close()
        db.close()


def cmd_config(args) -> int:
    """Show (and create if missing) the mailferry.toml configuration."""
    from .configfile import default_path, find_config, load_config
    values, warnings, path, created = load_config("", generate=True)
    if args.path:
        print(path)
        return 0
    print(f"{banner_line()}\n{SLOGAN}\n")
    print(f"Configuration file : {path}"
          + ("   (created just now)" if created else
             ("" if path.exists() else "   (missing — using built-in defaults)")))
    print(f"Default location   : {default_path()}")
    print("Search order       : --config PATH > ./mailferry.toml > default location")
    for w in warnings:
        print(c(f"warning: {w}", "yellow"))
    if values:
        print("\nSettings overriding the built-in defaults:")
        for k, v in sorted(values.items()):
            print(f"  {k:<24} = {v}")
    else:
        print("\nAll settings are at their built-in defaults.")
    print(c("\nEvery option is documented inside the file itself. CLI flags "
            "always override it; deleting it is always safe.", "cyan"))
    return 0


def cmd_doctor(args) -> int:
    """Local environment self-test — no servers contacted."""
    import platform
    import ssl as _ssl
    import tempfile
    print(f"{banner_line()}\n{SLOGAN}\n")
    print("Environment self-test:")
    ok = True

    def line(label, good, detail, advisory=False):
        nonlocal ok
        if not advisory:
            ok = ok and good
        mark = ("✓" if good else "✗") if not advisory else ("✓" if good else "•")
        print(f"  {mark} {label:<24} {detail}")

    pyok = sys.version_info >= (3, 9)
    line("Python", pyok, platform.python_version() + ("" if pyok else "  (need 3.9+)"))
    line("Platform", True, f"{platform.system()} {platform.release()}")
    line("TTY / interactive", sys.stdout.isatty(),
         "yes" if sys.stdout.isatty() else "no — TUI disabled here (advisory)", advisory=True)
    enc = (sys.stdout.encoding or "").lower()
    line("UTF-8 output", "utf" in enc, sys.stdout.encoding or "unknown", advisory=True)
    try:
        d = tempfile.mkdtemp()
        import sqlite3
        con = sqlite3.connect(str(Path(d) / "t.db"))
        con.execute("CREATE TABLE t(x)")
        con.close()
        line("State Database", True, "SQLite writable")
    except Exception as e:
        line("State Database", False, f"SQLite error: {e}")
    try:
        ctx = _ssl.create_default_context()
        n = len(ctx.get_ca_certs())
        line("TLS CA store", n > 0, f"{n} trusted roots")
    except Exception as e:
        line("TLS CA store", False, str(e))
    print("\nAll good — ready to migrate." if ok else "\nSome checks failed — see above.")
    return 0 if ok else 1


def cmd_benchmark(args) -> int:
    """Run the loopback benchmark from a source checkout; from the packaged
    .pyz (no dev harness bundled) point at the source and release notes."""
    import importlib.util
    for parent in Path(__file__).resolve().parents:
        cand = parent / "tools" / "benchmark.py"
        if cand.is_file():
            spec = importlib.util.spec_from_file_location("mf_benchmark", cand)
            mod = importlib.util.module_from_spec(spec)
            sys.path.insert(0, str(parent))
            spec.loader.exec_module(mod)
            return mod.main()
    print(f"{banner_line()}\nThe loopback benchmark ships with the source tree "
          f"(tools/benchmark.py):\n  git clone {REPOSITORY}\n  python3 tools/benchmark.py\n"
          f"Baseline numbers are published in the release notes: {REPOSITORY}/releases")
    return 0


def cmd_verify(args) -> int:
    cfg = RunConfig(tls_verify=not args.tls_no_verify, timeout=60.0, db_path=args.db)
    specs = parse_csv(args.csv_file)
    from .imap.client import ImapClient
    from .imap import mutf7
    from .state.db import StateDB

    async def main():
        db = StateDB(args.db)
        bad = 0
        for spec in specs:
            row = await db.upsert_mailbox(spec)

            def q(mid):
                def f():
                    return [dict(r) for r in db._con.execute(
                        "SELECT * FROM folders WHERE mailbox_id=?", (mid,)).fetchall()]
                return f
            folders = await db.run(q(row["id"]))
            if not folders:
                print(f"{spec.src.label}: no state recorded yet")
                continue
            src = ImapClient(spec.src, cfg)
            dst = ImapClient(spec.dst, cfg)
            try:
                await src.connect(); await src.login()
                await dst.connect(); await dst.login()
                for fo in folders:
                    try:
                        s = await src.status(mutf7.encode(fo["src_name"]))
                        d = await dst.status(mutf7.encode(fo["dst_name"]))
                    except MFError as e:
                        print(f"  {spec.src.label} [{fo['src_name']}]: STATUS failed ({reason_of(e)})")
                        bad += 1
                        continue
                    sm, dm = s.get("MESSAGES", 0), d.get("MESSAGES", 0)
                    okc = "OK " if dm >= fo["msgs_done"] else "MISMATCH"
                    if okc != "OK ":
                        bad += 1
                    print(f"  {okc} {spec.src.label} [{fo['src_name']}]: src={sm} dst={dm} "
                          f"synced={fo['msgs_done']}")
            except BaseException as e:
                print(f"{spec.src.label}: verify failed ({reason_of(e)})")
                bad += 1
            finally:
                for cli0 in (src, dst):
                    try:
                        await cli0.logout()
                    except Exception:
                        cli0.abort()
        db.close()
        return 1 if bad else 0

    return asyncio.new_event_loop().run_until_complete(main())


# --------------------------------------------------------------- main ----

def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="mailferry",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        description=f"{banner_line()}\n{SLOGAN}\n\n"
                    "MailFerry migrates, synchronises and backs up IMAP mailboxes natively —\n"
                    "no imapsync, no external tools, no third-party dependencies.",
        epilog=f"Repository: {REPOSITORY}\nSupport:    {SUPPORT_URL}\n"
               f"License:    GNU AGPL v3.0 — run 'mailferry --about' for details.")
    p.add_argument("--version", action="version", version=version_text())
    p.add_argument("--about", action="store_true", help="Show author, license and project information")
    sub = p.add_subparsers(dest="cmd")
    pr = sub.add_parser("run", help="Migrate / sync mailboxes from a CSV (default command)")
    _add_run_opts(pr)
    pres = sub.add_parser("resume", help="Resume a migration (alias of run)")
    _add_run_opts(pres)
    pc = sub.add_parser("check", help="Preflight: connect, authenticate, list, estimate — no writes")
    _add_run_opts(pc, check=True)
    pval = sub.add_parser("validate", help="Preflight validation (alias of check)")
    _add_run_opts(pval, check=True)
    sub.add_parser("doctor", help="Local environment self-test (no servers contacted)")
    sub.add_parser("benchmark", help="Run the loopback throughput benchmark")
    pi = sub.add_parser("init", help="Write a sample CSV template")
    pi.add_argument("file")
    pm = sub.add_parser("import-state", help="Import the old wrapper's migration.state")
    pm.add_argument("statefile")
    pm.add_argument("--db", default="./migration.db")
    pk = sub.add_parser("capabilities", help="Probe a server's capabilities and optimisation plan")
    pk.add_argument("host")
    pk.add_argument("port", type=int)
    pk.add_argument("--security", choices=("ssl", "tls", "none"), default="ssl")
    pk.add_argument("--user", default="")
    pk.add_argument("--password", default="")
    pk.add_argument("--tls-no-verify", action="store_true")
    pv = sub.add_parser("verify", help="Compare per-folder counts: source vs destination vs state DB")
    pv.add_argument("csv_file")
    pv.add_argument("--db", default="./migration.db")
    pv.add_argument("--tls-no-verify", action="store_true")
    pz = sub.add_parser("compact", help="Prune per-message rows for completed folders")
    pz.add_argument("--db", default="./migration.db")
    pcl = sub.add_parser("changelog", help="Show release history in the terminal")
    pcl.add_argument("version", nargs="?", help="Version to show (default: current)")
    pcl.add_argument("--full", action="store_true", help="Show the entire changelog")
    sub.add_parser("roadmap", help="Show the project roadmap in the terminal")
    pf = sub.add_parser("failed", help="List / export the Failed Message Registry")
    pf.add_argument("--db", default="./migration.db")
    pf.add_argument("--mailbox", default="", help="Only this mailbox (source user)")
    pf.add_argument("--all", action="store_true",
                    help="Include RECOVERED and IGNORED entries")
    pf.add_argument("--json", action="store_true", help="Emit JSON to stdout")
    pf.add_argument("--csv", metavar="FILE", default="", help="Export to a CSV file")
    pf.add_argument("--ignore", action="store_true",
                    help="Mark the selection IGNORED (still skipped, no longer outstanding)")
    prf = sub.add_parser("retry-failed",
                         help="Re-queue Failed Message Registry entries for the next run")
    prf.add_argument("--db", default="./migration.db")
    prf.add_argument("--mailbox", default="", help="Only this mailbox (source user)")
    prf.add_argument("--folder", default="", help="Only this folder")
    prf.add_argument("--uid", type=int, default=None, help="Only this source UID")
    pcf = sub.add_parser("config", help="Show / create the mailferry.toml configuration")
    pcf.add_argument("--path", action="store_true", help="Print only the active config path")
    return p


def main(argv=None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    if "--about" in argv[:2]:
        print(about_text(), end="")
        return 0
    if argv and argv[0] == "--init" and len(argv) > 1:      # wrapper muscle memory
        argv = ["init", argv[1]]
    if argv and argv[0] not in COMMANDS and not argv[0].startswith("-"):
        argv = ["run"] + argv                               # `mailferry mailboxes.csv`

    # mailferry.toml: sensible defaults that just work; the file only exists
    # so advanced users can tune behaviour. CLI flags always win. A missing
    # or broken file can never stop MailFerry from starting.
    explicit_cfg = ""
    if "--config" in argv:
        i = argv.index("--config")
        if i + 1 < len(argv):
            explicit_cfg = argv[i + 1]
            del argv[i:i + 2]
    from .configfile import load_config
    file_values, cfg_warnings, cfg_path, cfg_created = load_config(explicit_cfg)
    globals()["_CONFIG_INFO"] = (cfg_path, cfg_created, file_values, cfg_warnings)

    parser = build_parser()
    if not argv:                                            # bare invocation -> help
        parser.print_help()
        return 0
    if file_values:
        parser.set_defaults(**{k: v for k, v in file_values.items()})
    args = parser.parse_args(argv)
    for wmsg in cfg_warnings:
        print(c(f"note: {wmsg}", "yellow"), file=sys.stderr)
    if cfg_created:
        print(c(f"note: wrote a documented default configuration to {cfg_path} "
                "(optional — MailFerry runs fine without it)", "cyan"), file=sys.stderr)
    if args.cmd is None:
        parser.print_help()
        return 0
    debug = getattr(args, "debug", False) or "--debug" in argv
    try:
        if args.cmd in ("run", "resume"):
            return cmd_run(args)
        if args.cmd in ("check", "validate"):
            args.check = True
            return cmd_run(args)
        if args.cmd == "doctor":
            return cmd_doctor(args)
        if args.cmd == "benchmark":
            return cmd_benchmark(args)
        if args.cmd == "init":
            write_template(args.file)
            print(f"Sample CSV written to {args.file}")
            return 0
        if args.cmd == "import-state":
            return cmd_import_state(args)
        if args.cmd == "capabilities":
            return cmd_capabilities(args)
        if args.cmd == "verify":
            return cmd_verify(args)
        if args.cmd == "compact":
            return cmd_compact(args)
        if args.cmd == "changelog":
            return cmd_changelog(args)
        if args.cmd == "roadmap":
            return cmd_roadmap(args)
        if args.cmd == "failed":
            return cmd_failed(args)
        if args.cmd == "retry-failed":
            return cmd_retry_failed(args)
        if args.cmd == "config":
            return cmd_config(args)
        parser.print_help()
        return 2
    except KeyboardInterrupt:
        print("\nInterrupted.")
        return 130
    except BrokenPipeError:
        try:
            os.dup2(os.open(os.devnull, os.O_WRONLY), sys.stdout.fileno())
        except Exception:
            pass
        return 141
    except SystemExit as e:
        raise
    except Exception as e:
        if debug:
            raise
        print(c(f"ERROR: {e}", "red"), file=sys.stderr)
        print("Run again with --debug for a full traceback.", file=sys.stderr)
        return 1


def console():
    sys.exit(main())
