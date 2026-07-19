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

// Results-view rendering tests: verdicts, decoded subjects, concise
// attention panel, and clean layout — on fictional fixture data only.
package tui

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/engine"
	"github.com/ajsap/mailferry/v2/internal/report"
	"github.com/ajsap/mailferry/v2/internal/util"
)

func TestResultsShotSuccess(t *testing.T) {
	f := RenderShot("results-success")
	for _, want := range []string{"MIGRATION COMPLETE", "Run", "Messages",
		"Mailboxes (8)", "COMPLETED", "Success rate", "Q/Esc Quit"} {
		if !strings.Contains(f, want) {
			t.Fatalf("success frame lacks %q:\n%s", want, f)
		}
	}
	if strings.Contains(f, "Needs attention") {
		t.Fatal("clean success must not show the attention panel")
	}
	if strings.Contains(f, "NOTHING NEW") {
		t.Fatal("fresh copy must not claim nothing-new")
	}
}

func TestResultsShotWarnings(t *testing.T) {
	f := RenderShot("results-warnings")
	for _, want := range []string{"MIGRATION COMPLETED WITH WARNINGS", "Needs attention",
		"8 messages could not be migrated", "Failed Message Registry",
		"user3@example.org", "retry-failed", "COMPLETED WITH WARNINGS",
		"failed_messages.csv"} {
		if !strings.Contains(f, want) {
			t.Fatalf("warnings frame lacks %q:\n%s", want, f)
		}
	}
	// concise: never dump individual registry entries on the final screen
	if strings.Contains(f, "UID 4188") || strings.Contains(f, "APPEND: NO") {
		t.Fatal("final screen must not dump individual failed messages")
	}
	// 26,081 of 26,089 must never round up to 100% (v2.0.3 correctness rule)
	if !strings.Contains(f, "(99.9%)") || strings.Contains(f, "(100%)") {
		t.Fatalf("warnings frame percentage must be 99.9%%, never 100%%:\n%s", f)
	}
}

func TestResultsShotNothingNew(t *testing.T) {
	f := RenderShot("results-nothing")
	for _, want := range []string{"NOTHING NEW TO COPY", "Adopted", "(100%)"} {
		if !strings.Contains(f, want) {
			t.Fatalf("nothing-new frame lacks %q:\n%s", want, f)
		}
	}
}

// TestResultsAndHeadlessShareOneTruth renders the Results view AND the
// headless summary from the SAME engine snapshot and asserts they show
// the same synced figures and the same (never-lying) percentage.
func TestResultsAndHeadlessShareOneTruth(t *testing.T) {
	stats := engine.NewStats()
	mb := stats.Mailbox(1, "carol@example.org", "imap.example.com", "imap.example.net", "carol@example.net")
	now := time.Now()
	mb.Set(func(m *engine.MBValues) {
		m.Status = "WARNINGS"
		m.MsgsTotal, m.MsgsDone = 26089, 26081
		m.Appended, m.FailedMsgs = 26081, 8
		m.BytesTotal, m.BytesDone = 4<<30, 4<<30
		m.Start, m.End = now.Add(-time.Hour), now
	})
	snap := stats.Snapshot()
	agg := snap.Agg()
	wantPct := util.Pct(agg.MsgsDone, agg.MsgsTotal)
	if wantPct != "99.9%" {
		t.Fatalf("authoritative Pct(26081,26089) = %q, want 99.9%%", wantPct)
	}

	done := make(chan struct{})
	m := New(stats, engine.NewBus(), func() {}, func() {}, time.Second, done)
	defer m.stopSysmon()
	m.snap = snap
	m.finished = true
	frame := m.resultsView(124, 40)
	if !strings.Contains(frame, "26,081 of 26,089 ("+wantPct+")") {
		t.Fatalf("Results view does not show the authoritative figures:\n%s", frame)
	}

	oldStdout := os.Stdout
	rp, wp, _ := os.Pipe()
	os.Stdout = wp
	cfg := config.Defaults()
	cfg.RunID, cfg.LogsDir, cfg.DBPath = "truth-test", t.TempDir(), "test.db"
	report.PrintSummary(snap, "results.csv", cfg, 3600, false,
		func(s, _ string) string { return s })
	wp.Close()
	os.Stdout = oldStdout
	out, _ := io.ReadAll(rp)
	if !strings.Contains(string(out), "26081 of 26089 ("+wantPct+")") {
		t.Fatalf("headless summary diverges from the shared truth:\n%s", out)
	}
}

func TestFailedViewDecodesSubjects(t *testing.T) {
	// Errors view after completion doubles as the Failed Messages browser;
	// RFC 2047 words must be decoded there, raw encoded words must not leak.
	m := shotModelForErrors(t)
	f := m.errorsView(124, 40)
	if !strings.Contains(f, "Résumé of Q3 plans") {
		t.Fatalf("decoded subject missing:\n%s", f)
	}
	if strings.Contains(f, "=?utf-8?") {
		t.Fatalf("raw encoded word leaked into the failed view:\n%s", f)
	}
}

func shotModelForErrors(t *testing.T) *Model {
	t.Helper()
	stats := engine.NewStats()
	done := make(chan struct{})
	m := New(stats, engine.NewBus(), func() {}, func() {}, time.Second, done)
	defer m.stopSysmon()
	m.results = shotResult("results-warnings")
	m.finished = true
	m.snap = stats.Snapshot()
	return m
}
