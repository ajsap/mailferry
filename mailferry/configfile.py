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

mailferry.toml — optional configuration.

Philosophy: MailFerry works perfectly with no configuration at all. On
first launch a fully documented mailferry.toml is generated with the
built-in defaults; editing it is entirely optional. Command-line flags
always override the file. A missing, unreadable or invalid file can never
stop MailFerry from starting: every problem becomes a warning and the
built-in default is used.

Search order:  --config PATH  >  ./mailferry.toml  >  ~/.config/mailferry/mailferry.toml
"""
from __future__ import annotations

import os
from pathlib import Path
from typing import Dict, List, Tuple

try:                     # Python 3.11+
    import tomllib as _toml
except ImportError:      # pure-stdlib fallback parser for the simple subset we generate
    _toml = None

TEMPLATE = """\
# MailFerry Configuration
# High-Performance Native IMAP Migration Engine
# Author: Andy Saputra <andy@saputra.org>
# https://github.com/ajsap/mailferry
#
# Generated automatically on first launch. Every value below is the
# built-in default — MailFerry behaves identically whether or not this
# file exists. Edit what you need; delete anything you don't. Command-line
# flags always override this file. Invalid values fall back to the
# default with a warning; unknown keys are ignored with a warning.

[migration]
# Mailboxes migrated concurrently (CSV rows in flight at once).
parallel_mailboxes = 10
# Extra folder pipelines *inside* one large mailbox (connection pairs).
parallel_folders = 3
# Simultaneous connections allowed per server host (be kind to servers).
per_host_connections = 8
# Every APPEND is confirmed via APPENDUID / destination probe before a
# message is marked done. Always on — recorded here for transparency.
verify_after_copy = true

[retry]
# Attempts per batch level during failure isolation (see [recovery]).
batch_attempts = 3
# Transfer passes per message before it is recorded as failed.
message_attempts = 3
# Reconnect attempts per folder for ordinary connection trouble.
reconnect_attempts = 5
# Whole-mailbox retry attempts after a hard failure.
mailbox_attempts = 2
# Base delay in seconds between mailbox retries (doubles each attempt).
retry_delay = 30
# Backoff strategy: "exponential" (only supported value).
reconnect_backoff = "exponential"

[recovery]
# Progressively isolate messages that repeatedly break the transfer
# (batch -> halves -> single) instead of retrying the same batch forever.
isolate_failed_messages = true
# Skip messages already recorded in the Failed Message Registry on
# future runs ("mailferry retry-failed" re-queues them explicitly).
skip_known_failed_messages = true
# Mark a mailbox stalled after this long without measurable progress
# and recover it automatically (0 disables the supervisor).
stale_timeout_seconds = 300
# Connection-recovery attempts per stall before Recovery Mode.
recovery_retries = 3
# Wait between connection-recovery attempts, in seconds.
recovery_interval_seconds = 30

[logging]
# "info" (default), "debug" (full tracebacks) or "trace" (wire protocol,
# credentials always redacted).
level = "info"
# Delete per-run logs older than this many days at startup (0 = keep all).
keep_days = 30

[dashboard]
# TUI refresh interval in milliseconds (display only, engine unaffected).
refresh_ms = 250
# Show the live wire-throughput Speed column.
show_transfer_speed = true

[database]
# Worker/lease heartbeat interval in seconds.
heartbeat_seconds = 15
# A cluster worker silent for this long is offline; its mailboxes are
# reclaimed automatically by the remaining workers.
worker_timeout_seconds = 60
# Hard cap for any unexplained lease left in the State Database.
lock_timeout_seconds = 300
"""

# TOML key -> (argparse dest, type, validator)
_POS = lambda v: v > 0            # noqa: E731
_NONNEG = lambda v: v >= 0        # noqa: E731
KEYMAP: Dict[str, tuple] = {
    "migration.parallel_mailboxes": ("workers", int, _POS),
    "migration.parallel_folders": ("max_conns_per_mailbox", int, _POS),
    "migration.per_host_connections": ("per_host_conns", int, _POS),
    "migration.verify_after_copy": (None, bool, None),        # always on
    "retry.batch_attempts": ("batch_attempts", int, _POS),
    "retry.message_attempts": ("msg_retries", int, _POS),
    "retry.reconnect_attempts": ("reconnect_attempts", int, _POS),
    "retry.mailbox_attempts": ("retries", int, _NONNEG),
    "retry.retry_delay": ("retry_delay", float, _POS),
    "retry.reconnect_backoff": (None, str, lambda v: v == "exponential"),
    "recovery.isolate_failed_messages": ("isolate_failed", bool, None),
    "recovery.skip_known_failed_messages": ("skip_known_failed", bool, None),
    "recovery.stale_timeout_seconds": ("stale_timeout", float, _NONNEG),
    "recovery.recovery_retries": ("recovery_retries", int, _POS),
    "recovery.recovery_interval_seconds": ("recovery_interval", float, _POS),
    "logging.level": ("log_level", str,
                      lambda v: v in ("info", "debug", "trace")),
    "logging.keep_days": ("log_keep_days", int, _NONNEG),
    "dashboard.refresh_ms": ("refresh_ms", int, lambda v: 50 <= v <= 10000),
    "dashboard.show_transfer_speed": (None, bool, None),      # always shown
    "database.heartbeat_seconds": ("db_heartbeat", float, lambda v: v >= 5),
    "database.worker_timeout_seconds": ("worker_timeout", float, lambda v: v >= 20),
    "database.lock_timeout_seconds": ("lock_timeout", float, lambda v: v >= 30),
}


def default_path() -> Path:
    return Path(os.environ.get("MAILFERRY_CONFIG_DIR",
                               Path.home() / ".config" / "mailferry")) / "mailferry.toml"


def find_config(explicit: str = "") -> Path:
    if explicit:
        return Path(explicit)
    local = Path("mailferry.toml")
    if local.exists():
        return local
    return default_path()


def _mini_toml(text: str) -> dict:
    """Fallback parser (Python < 3.11) for the simple subset MailFerry
    generates: [sections], key = value with strings/ints/floats/bools.
    Anything else raises, so a mangled file is reported instead of being
    silently half-read."""
    out: dict = {}
    section = out
    for n, raw in enumerate(text.splitlines(), start=1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith("[") and line.endswith("]"):
            section = out.setdefault(line[1:-1].strip(), {})
            continue
        if "=" not in line:
            raise ValueError(f"line {n}: expected 'key = value' or '[section]'")
        k, _, v = line.partition("=")
        k, v = k.strip(), v.strip()
        if "#" in v and not (v.startswith('"') or v.startswith("'")):
            v = v.split("#", 1)[0].strip()
        if (v.startswith('"') and v.endswith('"')) or \
                (v.startswith("'") and v.endswith("'")):
            section[k] = v[1:-1]
        elif v in ("true", "false"):
            section[k] = (v == "true")
        else:
            try:
                section[k] = int(v)
            except ValueError:
                try:
                    section[k] = float(v)
                except ValueError:
                    section[k] = v
        continue
    return out


def load_config(explicit: str = "",
                generate: bool = True) -> Tuple[Dict[str, object], List[str], Path, bool]:
    """Returns (values-for-argparse-defaults, warnings, path, created).
    Never raises: every problem becomes a warning + built-in default."""
    warnings: List[str] = []
    path = find_config(explicit)
    created = False
    if not path.exists():
        if explicit:
            warnings.append(f"config file {path} not found — using built-in defaults")
            return {}, warnings, path, False
        if generate:
            try:
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(TEMPLATE, encoding="utf-8")
                created = True
            except OSError as e:
                warnings.append(f"could not create {path} ({e}) — using built-in defaults")
        return {}, warnings, path, created

    try:
        text = path.read_text(encoding="utf-8")
        if _toml is not None:
            data = _toml.loads(text)
        else:
            data = _mini_toml(text)
    except Exception as e:
        warnings.append(f"config {path}: could not parse ({e}) — using built-in defaults")
        return {}, warnings, path, False

    values: Dict[str, object] = {}
    for sect, body in data.items():
        if not isinstance(body, dict):
            warnings.append(f"config: ignoring top-level key '{sect}' (expected a [section])")
            continue
        for key, val in body.items():
            full = f"{sect}.{key}"
            spec = KEYMAP.get(full)
            if spec is None:
                warnings.append(f"config: unknown setting '{full}' ignored "
                                "(kept for forward compatibility)")
                continue
            dest, typ, check = spec
            try:
                if typ is bool:
                    if not isinstance(val, bool):
                        raise ValueError("expected true/false")
                    cast = val
                elif typ is float:
                    cast = float(val)
                elif typ is int:
                    if isinstance(val, bool):
                        raise ValueError("expected a number")
                    cast = int(val)
                else:
                    cast = str(val)
                if check is not None and not check(cast):
                    raise ValueError("out of range / unsupported value")
            except (TypeError, ValueError) as e:
                warnings.append(f"config: {full} = {val!r} invalid ({e}) — using the default")
                continue
            if dest is not None:
                values[dest] = cast
    return values, warnings, path, created
