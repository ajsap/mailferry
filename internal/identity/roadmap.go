// MailFerry — IMAP Migration & Sync
// High-Performance Native IMAP Migration Engine
//
// Copyright (C) 2026 Andy Saputra
// Author: Andy Saputra <andy@saputra.org>
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This file is part of MailFerry (https://github.com/ajsap/mailferry).
// Licensed under the GNU Affero General Public License v3.0 or later;
// see the LICENSE file for details.

package identity

import "strings"

// The project roadmap (aspirational), kept in sync with README/CHANGELOG.
// Statuses: done | next | planned.
var Roadmap = []struct{ Version, Status, Summary string }{
	{"v1.0.0", "done",
		"Initial public release (Python) — native asyncio IMAP protocol core, " +
			"per-message State Database, duplicate-free adoption, live two-sided " +
			"Dashboard, release tooling."},
	{"v1.2-dev", "done",
		"Unreleased Python reference line: full TUI (ten views), self-healing " +
			"stall recovery, failed-message isolation with a persistent registry and " +
			"COMPLETED WITH WARNINGS, multi-instance clustering with failover, live " +
			"wire-speed metering, mailferry.toml."},
	{"v2.0.0", "next",
		"Complete architectural rewrite in Go: single static cross-platform " +
			"binary (macOS/Linux/Windows, arm64+amd64), goroutine-based concurrent " +
			"engine, plus destination deduplication and date-range migration modes. " +
			"Released only after full feature parity with the Python reference."},
	{"v2.1.0", "planned",
		"Performance: MULTIAPPEND batching and QRESYNC/CONDSTORE delta sync; " +
			"OAuth 2.0 (XOAUTH2 / OAUTHBEARER); Prometheus-style metrics."},
}

// RoadmapLines renders plain, terminal-friendly roadmap lines.
func RoadmapLines() []string {
	mark := map[string]string{"done": "✓", "next": "▶", "planned": "·"}
	label := map[string]string{"done": "released", "next": "in progress", "planned": "planned"}
	var out []string
	for _, r := range Roadmap {
		lb := label[r.Status]
		if strings.Contains(r.Version, "dev") {
			lb = "reference" // unreleased development line, never published
		}
		out = append(out, "  "+mark[r.Status]+" "+pad(r.Version, 9)+
			" ("+lb+") — "+r.Summary)
	}
	return out
}

func pad(s string, n int) string {
	for len(s) < n {
		s += " "
	}
	return s
}
