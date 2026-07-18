# MailFerry Roadmap

The canonical roadmap also ships in the binary: `mailferry roadmap`.

- [x] **v1.0.0** — Initial public release (Python): native asyncio IMAP
  protocol core, per-message State Database, duplicate-free adoption,
  live two-sided dashboard, release tooling.
- [x] **v1.2-dev** — Unreleased Python reference line (preserved on the
  `legacy/python-final` branch): full TUI, self-healing stall recovery,
  failed-message isolation with a persistent registry and COMPLETED WITH
  WARNINGS, multi-instance clustering with failover, live wire-speed
  metering, mailferry.toml. Never published as a release.
- [ ] **v2.0.0** — Complete architectural rewrite in Go (this
  repository): single static cross-platform binary, goroutine-based
  concurrent engine, plus destination deduplication and date-range
  migration modes. `v2.0.0-rc.1` is the public release candidate;
  v2.0.0 final ships only after full parity validation in the field.
- [ ] **v2.1.0** — Performance: MULTIAPPEND batching and
  QRESYNC/CONDSTORE delta sync; OAuth 2.0 (XOAUTH2 / OAUTHBEARER);
  Prometheus-style metrics.
