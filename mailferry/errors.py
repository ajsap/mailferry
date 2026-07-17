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

Error taxonomy. Mirrors the wrapper's classification discipline:
transient (retry helps), throttle (back off, don't burn retries),
permanent (fail fast; retryable next run), auth (never auto-retried).
"""
from __future__ import annotations


class MFError(Exception):
    """Base for all MailFerry errors."""


class StopRun(Exception):
    """Graceful shutdown requested (Ctrl+C / SIGTERM). Not an error."""


class ProtocolError(MFError):
    """Server sent something we could not parse / protocol violation."""


class ConnectionLost(MFError):
    """Socket closed, reset, timed out at the transport level."""


class TransientError(MFError):
    """Network-shaped failure: retrying is likely to help."""


class ThrottledError(TransientError):
    """Server asked us to slow down; back off without burning retry budget."""


class PermanentError(MFError):
    """Repeating cannot succeed this run (still retryable next run)."""


class AuthFailed(PermanentError):
    """Authentication failure. NEVER auto-retried (lockout avoidance)."""


class QuotaExceeded(PermanentError):
    """Destination over quota."""


class CommandFailed(MFError):
    """Tagged NO/BAD completion."""

    def __init__(self, name: str, status: str, text: str, code=None):
        super().__init__(f"{name}: {status} {text}".strip())
        self.name = name
        self.status = status
        self.text = text or ""
        self.code = code or []


_THROTTLE_RX = ("limit", "throttl", "rate", "too many", "slow down", "unavailable. please try")
_AUTH_RX = ("authenticationfailed", "authorizationfailed", "login failed", "invalid credential",
            "authentication failed", "username and password not accepted", "auth", "privacyrequired")
_QUOTA_RX = ("overquota", "over quota", "quota exceed", "exceeded your mail quota")


def classify_command_failure(exc: CommandFailed) -> MFError:
    """Map a NO/BAD into the taxonomy using response code + text."""
    code0 = str(exc.code[0]).upper() if exc.code else ""
    low = (exc.text or "").lower()
    if exc.name in ("LOGIN", "AUTHENTICATE"):
        return AuthFailed(str(exc))
    if code0 in ("OVERQUOTA",) or any(k in low for k in _QUOTA_RX):
        return QuotaExceeded(str(exc))
    if code0 in ("LIMIT", "INUSE", "UNAVAILABLE") or any(k in low for k in _THROTTLE_RX):
        return ThrottledError(str(exc))
    if code0 in ("AUTHENTICATIONFAILED", "AUTHORIZATIONFAILED") or any(k in low for k in _AUTH_RX):
        return AuthFailed(str(exc))
    if code0 in ("NOPERM", "CANNOT"):
        return PermanentError(str(exc))
    return exc  # caller decides (often per-message poison)


def is_transient(exc: BaseException) -> bool:
    if isinstance(exc, AuthFailed):
        return False
    if isinstance(exc, (TransientError, ConnectionLost, OSError)):
        return True
    return False


def reason_of(exc: BaseException) -> str:
    """One-line human reason, wrapper-style."""
    if isinstance(exc, AuthFailed):
        return "authentication failure"
    if isinstance(exc, QuotaExceeded):
        return "quota exceeded"
    if isinstance(exc, ThrottledError):
        return "server throttling"
    if isinstance(exc, ConnectionLost):
        return f"connection dropped/reset ({exc})"
    if isinstance(exc, CommandFailed):
        return f"server refused {exc.name}: {exc.text[:120]}"
    if isinstance(exc, ProtocolError):
        return f"protocol error: {str(exc)[:120]}"
    if isinstance(exc, OSError):
        return f"network error: {exc}"
    return f"{type(exc).__name__}: {str(exc)[:140]}"
