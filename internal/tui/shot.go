// MailFerry — IMAP Migration & Sync
// A High-Performance Native IMAP Migration Engine
//
// Copyright (C) 2026 Andy Saputra
// Author: Andy Saputra <andy@saputra.org>
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This file is part of MailFerry (https://github.com/ajsap/mailferry).
// Licensed under the GNU Affero General Public License v3.0 or later;
// see the LICENSE file for details.

// Documentation shot support (cmd/tuishot): renders populated View()
// frames for screenshots. Fixture data only — RFC-2606 example domains.
package tui

import (
	"fmt"
	"time"

	"github.com/ajsap/mailferry/v2/internal/engine"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// RenderShot returns one fully rendered frame of the named view using
// synthetic fixture data. Views: dashboard, workers, history, logs.
func RenderShot(view string) string {
	lipgloss.SetColorProfile(termenv.ANSI256)
	stats := engine.NewStats()
	stats.CSVFile = "mailboxes.csv"
	stats.DBPath = "./migration.db"
	stats.LogsDir = "./logs"
	stats.Workers = 4
	now := time.Now()

	type fix struct {
		label, status, op string
		fldr, fldrTot     int
		done, total       int64
		bdone, btotal     int64
		failed            int64
	}
	rows := []fix{
		{"user1@example.org", "SUCCESS", "", 6, 6, 4211, 4211, 512 << 20, 512 << 20, 0},
		{"user2@example.org", "SUCCESS", "", 4, 4, 1874, 1874, 96 << 20, 96 << 20, 0},
		{"user3@example.org", "WARNINGS", "", 9, 9, 3409, 3417, 1207 << 20, 1210 << 20, 8},
		{"user4@example.org", "RUNNING", "INBOX: FETCH->APPEND 512/2210", 3, 11, 1642, 5804, 210 << 20, 730 << 20, 0},
		{"user5@example.org", "RUNNING", "Sent: FETCH->APPEND 88/410", 2, 7, 410, 2996, 66 << 20, 402 << 20, 0},
		{"user6@example.org", "REMOTE", "worker helm:70211", 0, 0, 902, 3120, 100 << 20, 350 << 20, 0},
		{"user7@example.org", "RETRYING", "reconnect 2/5", 1, 5, 204, 1450, 30 << 20, 190 << 20, 0},
		{"user8@example.org", "QUEUED", "", 0, 0, 0, 0, 0, 0, 0},
	}
	for i, r := range rows {
		mb := stats.Mailbox(i+1, r.label, "imap.example.com", "imap.example.net",
			"user"+fmt.Sprint(i+1)+"@example.net")
		st := r
		mb.Set(func(m *engine.MBValues) {
			m.Status = st.status
			m.Op = st.op
			m.FolderIndex = st.fldr
			m.FoldersTotal = st.fldrTot
			m.MsgsDone, m.MsgsTotal = st.done, st.total
			m.BytesDone, m.BytesTotal = st.bdone, st.btotal
			m.FailedMsgs = st.failed
			m.Start = now.Add(-time.Duration(25+i*7) * time.Minute)
			if st.status == "SUCCESS" || st.status == "WARNINGS" {
				m.End = now.Add(-time.Duration(i) * time.Minute)
			}
			m.Src.Host, m.Dst.Host = "imap.example.com", "imap.example.net"
			m.Src.ConnState, m.Dst.ConnState = "ready", "ready"
			m.Src.Caps = []string{"SSL", "UID+", "LIT+", "ZIP"}
			m.Dst.Caps = []string{"SSL", "UID+", "LIT+"}
			m.Src.RXBytes = st.bdone + st.bdone/9
			m.Dst.TXBytes = st.bdone + st.bdone/8
			m.Folder = "INBOX"
		})
	}
	stats.BatchStart = now.Add(-92 * time.Minute)

	bus := engine.NewBus()
	bus.WorkerID = "ferry:70210:a1b2c3d4"
	bus.SetCluster([]engine.WorkerInfo{
		{ID: "ferry:70210:a1b2c3d4", Host: "ferry.local", Status: "WORKING",
			Active: 2, Heartbeat: now.Add(-3 * time.Second), HBAge: 3,
			Started: now.Add(-92 * time.Minute)},
		{ID: "helm:70211:e5f6a7b8", Host: "helm.local", Status: "WORKING",
			Active: 1, Heartbeat: now.Add(-7 * time.Second), HBAge: 7,
			Started: now.Add(-31 * time.Minute)},
		{ID: "quay:1044:c9d0e1f2", Host: "quay.local", Status: "IDLE",
			Active: 0, Heartbeat: now.Add(-12 * time.Second), HBAge: 12,
			Started: now.Add(-8 * time.Minute)},
	})
	hist := []struct{ ev, st, mb, det string }{
		{"Run started", "OK", "-", "mailboxes.csv · 8 mailbox(es) · run 20260718-093012"},
		{"Worker joined", "OK", "-", "ferry:70210:a1b2c3d4 — cluster on ./migration.db"},
		{"Migration started", "OK", "user1@example.org", "user1 → user1@example.net"},
		{"Folder migrated", "OK", "user1@example.org", "INBOX: 2,210 msgs, 380.0 MB"},
		{"Worker joined", "OK", "-", "helm:70211:e5f6a7b8 — cluster on ./migration.db"},
		{"Stalled transfer detected", "WARN", "user4@example.org", "no progress for 5m 0s — verifying"},
		{"Connection recovery", "WARN", "user4@example.org", "attempt 1/3 — reconnecting from the last checkpoint"},
		{"Transfer recovered", "OK", "user4@example.org", "resumed after reconnect 1 — no data lost"},
		{"Stalled transfer detected", "WARN", "user3@example.org", "no progress for 5m 0s — verifying"},
		{"Entering Recovery Mode", "WARN", "user3@example.org", "isolate the failing batch, keep everything else moving"},
		{"Batch isolation", "WARN", "user3@example.org", "Archive: batch 8 → 4+4 → 2+2 → 1 (attempt 2/3)"},
		{"Failed message isolated", "WARN", "user3@example.org", "Archive UID 4188 — connection dies on APPEND"},
		{"Message recorded", "OK", "user3@example.org", "Failed Message Registry: CONNECTION_RESET · will be skipped"},
		{"Migration resumed", "OK", "user3@example.org", "Archive: continuing after UID 4188"},
		{"Completed with warnings", "WARN", "user3@example.org", "3,409 of 3,417 migrated · 8 in the registry (99.77%)"},
		{"Stale lock auto-reset", "OK", "user6@example.org", "quay:31022 last heartbeat 214s ago (> 90s) — that worker is dead; continuing"},
		{"Worker takeover", "WARN", "user6@example.org", "reclaimed from offline worker helm:70155 — resuming from the last checkpoint"},
		{"Folder migrated", "OK", "user2@example.org", "Sent: 512 msgs, 48.1 MB"},
	}
	for _, h := range hist {
		bus.History(h.ev, h.st, h.mb, h.det)
	}
	logsFix := []struct{ sev, mb, msg string }{
		{"INFO", "-", "cluster: joined as worker ferry:70210:a1b2c3d4 (offline threshold 60s)"},
		{"INFO", "user1@example.org", "plan: 6 folder(s), est 4,211 msgs"},
		{"INFO", "user1@example.org", "src: COMPRESS=DEFLATE enabled"},
		{"INFO", "user4@example.org", "INBOX: streaming FETCH->APPEND window 8"},
		{"WARN", "user4@example.org", "src: stalled — connection recovery 1/3"},
		{"INFO", "user4@example.org", "transfer recovered: INBOX resumed and completed (reconnect 1)"},
		{"WARN", "user3@example.org", "dst: APPEND NO — isolating (batch 8 → 4+4)"},
		{"WARN", "user3@example.org", "recovery mode — isolate problematic messages"},
		{"ERROR", "user3@example.org", "Archive UID 4188 failed permanently: CONNECTION_RESET"},
		{"INFO", "user3@example.org", "registry: 8 message(s) recorded — future runs skip them"},
		{"INFO", "user6@example.org", "lease lost — mailbox taken over by another worker; stopping local work"},
		{"INFO", "user2@example.org", "done: src=1874 dst=1874 synced=1874 adopted=0 copied=1874"},
		{"INFO", "-", "migration paused"},
		{"INFO", "-", "migration resumed"},
	}
	for i := 0; i < 2; i++ {
		for _, l := range logsFix {
			bus.Log(l.sev, l.mb, l.msg)
		}
	}

	done := make(chan struct{})
	m := New(stats, bus, func() {}, func() {}, 250*time.Millisecond, done)
	m.width, m.height = 124, 33
	m.snap = stats.Snapshot()
	m.lastRates = [2]float64{9.4 * 1024 * 1024, 41}
	switch view {
	case "workers":
		m.active = vWorkers
	case "history":
		m.active = vHistory
	case "logs":
		m.active = vLogs
	default:
		m.active = vDashboard
	}
	frame := m.View()
	m.stopSysmon()
	return frame
}
