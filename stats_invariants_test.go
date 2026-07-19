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

// Statistics invariants: the counters every completion surface renders
// (Results TUI, headless summary, results.csv) must reconcile — synced =
// copied + adopted, failures live outside MsgsDone, and idempotent
// reruns account every message. Runs the REAL engine against fake
// servers with injected APPEND failures; nothing is mocked at the
// statistics layer.
package mailferry_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/engine"
	"github.com/ajsap/mailferry/v2/internal/fakeimap"
	"github.com/ajsap/mailferry/v2/internal/util"
)

// runFull mirrors harness.run but returns the full RunResult.
func (h *harness) runFull(mutate func(*config.Run)) engine.RunResult {
	h.t.Helper()
	cfg := config.Defaults()
	cfg.CSVFile = "test.csv"
	cfg.DBPath = h.dir + "/migration.db"
	cfg.LogsDir = h.dir + "/logs"
	cfg.Workers = 1
	cfg.Timeout = 30
	cfg.RetryDelay = 1
	cfg.StaleTimeout = 0
	cfg.RunID = fmt.Sprintf("inv-%d", len(h.sessions))
	if mutate != nil {
		mutate(cfg)
	}
	h.cfg = cfg
	specs := []config.MailboxSpec{{
		Index: 1,
		Src: config.Endpoint{Host: "127.0.0.1", Port: h.src.Port(), Security: "none",
			User: h.srcA.User, Password: h.srcA.Password},
		Dst: config.Endpoint{Host: "127.0.0.1", Port: h.dst.Port(), Security: "none",
			User: h.dstA.User, Password: h.dstA.Password},
	}}
	h.stats = engine.NewStats()
	res, err := engine.RunMigration(context.Background(), cfg, specs, h.stats,
		func(s string) { h.sessions = append(h.sessions, s) },
		func(config.MailboxSpec) func(string, ...any) {
			return func(format string, a ...any) {
				h.sessions = append(h.sessions, fmt.Sprintf(format, a...))
			}
		})
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func assertReconciled(t *testing.T, snap engine.Snapshot, tag string) {
	t.Helper()
	for _, m := range snap.Mailboxes {
		if m.MsgsDone != m.Appended+m.Adopted+m.PriorDone+m.Planned {
			t.Fatalf("%s: %s: MsgsDone %d != Appended %d + Adopted %d + Prior %d + Planned %d",
				tag, m.Label, m.MsgsDone, m.Appended, m.Adopted, m.PriorDone, m.Planned)
		}
		if m.MsgsDone > m.MsgsTotal {
			t.Fatalf("%s: %s: MsgsDone %d exceeds MsgsTotal %d",
				tag, m.Label, m.MsgsDone, m.MsgsTotal)
		}
		if m.MsgsDone+m.FailedMsgs+m.Skipped < m.MsgsTotal &&
			(m.Status == "SUCCESS" || m.Status == "WARNINGS") {
			t.Fatalf("%s: %s: completed mailbox does not account for every message: "+
				"done %d + failed %d + skipped %d < total %d",
				tag, m.Label, m.MsgsDone, m.FailedMsgs, m.Skipped, m.MsgsTotal)
		}
	}
	agg := snap.Agg()
	if agg.MsgsDone != agg.Appended+agg.Adopted+agg.PriorDone+agg.Planned {
		t.Fatalf("%s: aggregate: done %d != appended %d + adopted %d + prior %d + planned %d",
			tag, agg.MsgsDone, agg.Appended, agg.Adopted, agg.PriorDone, agg.Planned)
	}
}

func TestStatsInvariantsWarningsAndIdempotentRerun(t *testing.T) {
	srcA := fakeimap.NewAccount("inv1", "p")
	for i := 0; i < 12; i++ {
		marker := "OK"
		if i == 3 || i == 7 || i == 9 {
			marker = "REJECTME"
		}
		srcA.Folder("INBOX").Add(poisonBody(i, marker), nil, "17-Jul-2026 10:00:00 +0000")
	}
	dstA := fakeimap.NewAccount("inv1", "p")
	h := newHarness(t, srcA, dstA)
	h.dst.AppendReject = []byte("REJECTME")

	res := h.runFull(nil)
	snap := h.stats.Snapshot()
	assertReconciled(t, snap, "warnings-run")
	agg := snap.Agg()
	if agg.Counts["WARNINGS"] != 1 {
		t.Fatalf("want WARNINGS mailbox, got %v", agg.Counts)
	}
	if agg.MsgsDone != 9 || agg.Appended != 9 || agg.Adopted != 0 {
		t.Fatalf("first run: done/appended/adopted = %d/%d/%d, want 9/9/0",
			agg.MsgsDone, agg.Appended, agg.Adopted)
	}
	if agg.FailedMsgs != 3 || res.Outstanding != 3 {
		t.Fatalf("failed counters: agg %d, outstanding %d, want 3/3",
			agg.FailedMsgs, res.Outstanding)
	}
	// Near-complete must never render as 100% anywhere.
	if p := util.Pct(agg.MsgsDone, agg.MsgsTotal); p == "100%" {
		t.Fatalf("9 of 12 rendered as %q", p)
	}

	// Idempotent rerun on the same State DB: nothing copied, everything
	// accounted, still 3 outstanding — and still reconciled.
	res2 := h.runFull(nil)
	snap2 := h.stats.Snapshot()
	assertReconciled(t, snap2, "rerun")
	agg2 := snap2.Agg()
	if agg2.Appended != 0 {
		t.Fatalf("rerun copied %d messages, want 0", agg2.Appended)
	}
	if res2.Outstanding != 3 {
		t.Fatalf("rerun outstanding %d, want 3", res2.Outstanding)
	}
	if agg2.PriorDone != 9 {
		t.Fatalf("rerun prior-confirmed %d, want 9", agg2.PriorDone)
	}
}

func TestStatsInvariantsDryRunPlanned(t *testing.T) {
	srcA := fakeimap.NewAccount("inv3", "p")
	for i := 0; i < 5; i++ {
		srcA.Folder("INBOX").Add(poisonBody(i, "OK"), nil, "17-Jul-2026 10:00:00 +0000")
	}
	dstA := fakeimap.NewAccount("inv3", "p")
	h := newHarness(t, srcA, dstA)
	h.runFull(func(c *config.Run) { c.DryRun = true; c.Ephemeral = true })
	snap := h.stats.Snapshot()
	assertReconciled(t, snap, "dry-run")
	agg := snap.Agg()
	if agg.Planned != 5 || agg.Appended != 0 {
		t.Fatalf("dry run planned/appended = %d/%d, want 5/0", agg.Planned, agg.Appended)
	}
	if n := len(dstA.Folder("INBOX").Msgs); n != 0 {
		t.Fatalf("dry run wrote %d messages to the destination", n)
	}
}

func TestStatsInvariantsCleanCompletionIsExactly100(t *testing.T) {
	srcA := fakeimap.NewAccount("inv2", "p")
	for i := 0; i < 7; i++ {
		srcA.Folder("INBOX").Add(poisonBody(i, "OK"), nil, "17-Jul-2026 10:00:00 +0000")
	}
	dstA := fakeimap.NewAccount("inv2", "p")
	h := newHarness(t, srcA, dstA)
	res := h.runFull(nil)
	snap := h.stats.Snapshot()
	assertReconciled(t, snap, "clean")
	agg := snap.Agg()
	if agg.Counts["SUCCESS"] != 1 || res.Outstanding != 0 {
		t.Fatalf("clean run: %v outstanding=%d", agg.Counts, res.Outstanding)
	}
	if agg.MsgsDone != agg.MsgsTotal {
		t.Fatalf("clean run incomplete: %d of %d", agg.MsgsDone, agg.MsgsTotal)
	}
	if p := util.Pct(agg.MsgsDone, agg.MsgsTotal); p != "100%" {
		t.Fatalf("exact completion must render 100%%, got %q", p)
	}
}
