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

// `mailferry attach` read-only monitor tests. A real migration runs in a
// goroutine against a slow fake server; the attach DATA layer is exercised
// directly (the pollable view-model), proving it observes the RUNNING mailbox
// and a live worker heartbeat mid-migration, that repeated snapshots are safe,
// that it writes NOTHING to the State Database (the file is untouched by polls
// once the migration is done), and that the migration finishes to exact
// message counts (attach never disturbed it). A smoke render of the model's
// View() confirms the banner and a mailbox label appear.
package mailferry_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/engine"
	"github.com/ajsap/mailferry/v2/internal/fakeimap"
	"github.com/ajsap/mailferry/v2/internal/state"
	"github.com/ajsap/mailferry/v2/internal/tui"
)

func TestAttachDataLayerObservesRunningMigrationReadOnly(t *testing.T) {
	srcA := fakeimap.NewAccount("alice", "pw1")
	const total = 30
	for i := 1; i <= total; i++ {
		srcA.Folder("INBOX").Add(msgBody(i, "attach", true, 1500), nil,
			"17-Jul-2026 10:00:00 +0000")
	}
	dstA := fakeimap.NewAccount("bob", "pw2")
	h := newHarness(t, srcA, dstA)
	h.dst.AppendDelayMS.Store(40) // slow server: the migration is observably in-flight

	dbPath := filepath.Join(h.dir, "mailferry.db")
	logsDir := filepath.Join(h.dir, "logs")
	os.MkdirAll(logsDir, 0o755)
	cfg := config.Defaults()
	cfg.CSVFile = "test.csv"
	cfg.DBPath = dbPath
	cfg.LogsDir = logsDir
	cfg.Workers = 1
	cfg.Timeout = 30
	cfg.RunID = "attachrun"
	specs := []config.MailboxSpec{{
		Index: 1,
		Src: config.Endpoint{Host: "127.0.0.1", Port: h.src.Port(), Security: "none",
			User: "alice", Password: "pw1"},
		Dst: config.Endpoint{Host: "127.0.0.1", Port: h.dst.Port(), Security: "none",
			User: "bob", Password: "pw2"},
	}}

	// Session log so the tail feed has something to read.
	sessionLog := filepath.Join(logsDir, "session.log")
	os.WriteFile(sessionLog, []byte("attachrun start\n"), 0o600)

	done := make(chan map[string]int, 1)
	stats := engine.NewStats()
	go func() {
		res, err := engine.RunMigration(context.Background(), cfg, specs, stats,
			func(s string) {
				f, _ := os.OpenFile(sessionLog, os.O_APPEND|os.O_WRONLY, 0o600)
				if f != nil {
					f.WriteString(s + "\n")
					f.Close()
				}
			},
			func(config.MailboxSpec) func(string, ...any) {
				return func(string, ...any) {}
			})
		if err != nil {
			t.Errorf("migration: %v", err)
		}
		done <- res.Counts
	}()

	// Open a SEPARATE read-only handle — exactly what `mailferry attach` does —
	// and poll while the migration proceeds.
	rdb, err := state.OpenReadOnly(dbPath)
	if err != nil {
		// The migration may not have created the file yet; wait briefly.
		deadline := time.Now().Add(5 * time.Second)
		for err != nil && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			rdb, err = state.OpenReadOnly(dbPath)
		}
		if err != nil {
			t.Fatalf("open read-only DB: %v", err)
		}
	}
	defer rdb.Close()
	poller := tui.NewAttachPoller(rdb, dbPath, sessionLog, "attachrun", 60, 12)

	// Poll until we observe the mailbox RUNNING with a registered worker — the
	// migration is deliberately slow, so this must be visible.
	sawRunning := false
	sawWorker := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		snap := poller.Poll()
		if snap.Report.Counts["RUNNING"] >= 1 {
			sawRunning = true
		}
		for _, wk := range snap.Report.Workers {
			if wk.Heartbeat > 0 {
				sawWorker = true
			}
		}
		// Mailbox row should surface the alice label while running.
		for _, mb := range snap.Report.Mailboxes {
			if strings.Contains(mb.Label, "alice") && mb.Status == "RUNNING" {
				sawRunning = true
			}
		}
		if sawRunning && sawWorker {
			break
		}
		select {
		case <-done:
			// Migration finished before we could observe it running — put the
			// result back and stop looping.
			goto finished
		default:
			time.Sleep(15 * time.Millisecond)
		}
	}
finished:
	if !sawRunning {
		t.Fatal("attach never observed the RUNNING mailbox during the migration")
	}
	if !sawWorker {
		t.Fatal("attach never observed a live worker heartbeat during the migration")
	}

	// Repeated snapshots are safe (no panic, no error path).
	for i := 0; i < 5; i++ {
		_ = poller.Poll()
	}

	// Wait for the migration to finish and assert EXACT counts — attach did
	// not disturb it.
	var counts map[string]int
	select {
	case counts = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("migration did not finish")
	}
	if counts["SUCCESS"] != 1 {
		t.Fatalf("migration result: %v (attach must not disturb it)", counts)
	}
	if n := len(dstA.Folder("INBOX").Msgs); n != total {
		t.Fatalf("delivered %d, want %d (attach must not disturb it)", n, total)
	}
	if u := uniqueBodies(dstA.Folder("INBOX")); u != total {
		t.Fatalf("attach run produced duplicates: %d unique of %d", u, total)
	}

	// Read-only proof: with the migration finished (no more writers), polling
	// must not modify the State Database file at all.
	st0, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		_ = poller.Poll()
		time.Sleep(5 * time.Millisecond)
	}
	st1, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if st0.ModTime() != st1.ModTime() || st0.Size() != st1.Size() {
		t.Fatalf("attach polling modified the State Database: mtime %v->%v size %d->%d",
			st0.ModTime(), st1.ModTime(), st0.Size(), st1.Size())
	}

	// Final snapshot reflects the completed run.
	final := poller.Poll()
	if final.Report.Counts["SUCCESS"] != 1 {
		t.Fatalf("final snapshot SUCCESS=%d, want 1", final.Report.Counts["SUCCESS"])
	}
}

func TestAttachRenderShowsBannerAndMailbox(t *testing.T) {
	// A tiny completed migration gives the renderer real data.
	srcA := fakeimap.NewAccount("carol", "pw1")
	for i := 1; i <= 3; i++ {
		srcA.Folder("INBOX").Add(msgBody(i, "render", true, 10), nil,
			"17-Jul-2026 10:00:00 +0000")
	}
	dstA := fakeimap.NewAccount("dave", "pw2")
	h := newHarness(t, srcA, dstA)
	counts := h.run(func(c *config.Run) { c.RunID = "renderrun" })
	if counts["SUCCESS"] != 1 {
		t.Fatalf("seed migration: %v", counts)
	}

	rdb, err := state.OpenReadOnly(h.cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	logPath := filepath.Join(h.cfg.LogsDir, "session.log")
	poller := tui.NewAttachPoller(rdb, h.cfg.DBPath, logPath, "renderrun", 60, 12)

	// Data-layer render (single source of truth for the View()).
	out := tui.RenderAttach(poller.Poll())
	if out == "" {
		t.Fatal("RenderAttach returned empty output")
	}
	for _, want := range []string{"MailFerry", "ATTACH", "carol", "Workers"} {
		if !strings.Contains(out, want) {
			t.Fatalf("attach render missing %q; got:\n%s", want, out)
		}
	}

	// Smoke the Bubble Tea model's View() too — non-empty, banner + label.
	model := tui.NewAttachModel(poller)
	v := model.View()
	if v == "" || !strings.Contains(v, "MailFerry") || !strings.Contains(v, "carol") {
		t.Fatalf("model.View() smoke failed; got:\n%s", v)
	}
	// The optional run-id argument appears in the title.
	if !strings.Contains(v, "renderrun") {
		t.Fatalf("model.View() missing the run-id filter in the title:\n%s", v)
	}
}

func TestAttachNoSessionLogIsGraceful(t *testing.T) {
	srcA := fakeimap.NewAccount("erin", "pw1")
	srcA.Folder("INBOX").Add(msgBody(1, "nolog", true, 10), nil, "17-Jul-2026 10:00:00 +0000")
	dstA := fakeimap.NewAccount("frank", "pw2")
	h := newHarness(t, srcA, dstA)
	if counts := h.run(nil); counts["SUCCESS"] != 1 {
		t.Fatalf("seed migration: %v", counts)
	}
	rdb, err := state.OpenReadOnly(h.cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	missing := filepath.Join(t.TempDir(), "does-not-exist", "session.log")
	poller := tui.NewAttachPoller(rdb, h.cfg.DBPath, missing, "", 60, 12)
	out := tui.RenderAttach(poller.Poll())
	if !strings.Contains(out, "no session log found") {
		t.Fatalf("missing-log path not surfaced gracefully:\n%s", out)
	}
}
