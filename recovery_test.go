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

// Recovery & registry regression tests, including the poison-batch signature:
// a poison batch that repeatedly fails APPEND then kills the connection
// must be isolated, recorded, and the mailbox must complete WITH WARNINGS
// while every healthy message migrates. Resume must skip the known-failed
// messages; retry-failed must recover them.
package mailferry_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/engine"
	"github.com/ajsap/mailferry/v2/internal/fakeimap"
	_ "modernc.org/sqlite"
)

func poisonBody(i int, marker string) []byte {
	return []byte(fmt.Sprintf("Message-ID: <n%d@x>\r\nSubject: poison %d\r\n"+
		"From: Boss <boss@x>\r\nDate: Fri, 17 Jul 2026 10:00:00 +0000\r\n\r\n%s %s",
		i, i, marker, string(make([]byte, 900))))
}

func TestPoisonIsolationWarningsAndRetry(t *testing.T) {
	srcA := fakeimap.NewAccount("u13", "p")
	for i := 0; i < 20; i++ {
		marker := "OK"
		if i == 7 {
			marker = "KILLME"
		} else if i == 12 {
			marker = "REJECTME"
		}
		srcA.Folder("INBOX").Add(poisonBody(i, marker), nil, "17-Jul-2026 10:00:00 +0000")
	}
	dstA := fakeimap.NewAccount("u13", "p")
	h := newHarness(t, srcA, dstA)
	h.dst.AppendKill = []byte("KILLME")     // content filter drops the connection
	h.dst.AppendReject = []byte("REJECTME") // server answers a clean NO

	start := time.Now()
	counts := h.run(func(c *config.Run) { c.Workers = 1; c.StaleTimeout = 0 })
	elapsed := time.Since(start)
	if counts["WARNINGS"] != 1 {
		t.Fatalf("want WARNINGS, got %v (%v)", counts, tail(h.sessions, 12))
	}
	if elapsed > 60*time.Second {
		t.Fatalf("isolation too slow: %s", elapsed)
	}
	if n := len(dstA.Folder("INBOX").Msgs); n != 18 {
		t.Fatalf("healthy delivered: %d (want 18)", n)
	}
	if u := uniqueBodies(dstA.Folder("INBOX")); u != 18 {
		t.Fatalf("duplicates around poison: %d unique", u)
	}
	if !h.logContains("Recovery Mode") {
		t.Fatalf("Recovery Mode not logged: %v", tail(h.sessions, 12))
	}

	// registry: both poison UIDs recorded with the right failure types
	reg := readRegistry(t, h.cfg.DBPath)
	if len(reg) != 2 {
		t.Fatalf("registry has %d rows, want 2: %v", len(reg), reg)
	}
	if reg[8] != "CONNECTION_RESET" {
		t.Fatalf("UID 8 (kill) type = %q, want CONNECTION_RESET", reg[8])
	}
	if reg[13] != "APPEND_NO" {
		t.Fatalf("UID 13 (reject) type = %q, want APPEND_NO", reg[13])
	}

	// resume: known-failed skipped instantly, zero new appends
	before := h.dst.AppendCount.Load()
	counts = h.run(func(c *config.Run) { c.Workers = 1; c.StaleTimeout = 0 })
	if counts["WARNINGS"] != 1 {
		t.Fatalf("resume status: %v", counts)
	}
	if d := h.dst.AppendCount.Load() - before; d != 0 {
		t.Fatalf("resume re-appended %d (want 0 — should skip known-failed)", d)
	}

	// server fixed + retry-failed: the two messages recover
	h.dst.AppendKill = nil
	h.dst.AppendReject = nil
	db, _ := engine.OpenStateForTest(h.cfg.DBPath) // thin helper below
	n := db.SetFailedStatus("RETRY_PENDING", "", "", 0)
	db.Close()
	if n != 2 {
		t.Fatalf("retry-failed re-queued %d (want 2)", n)
	}
	counts = h.run(func(c *config.Run) { c.Workers = 1; c.StaleTimeout = 0 })
	if counts["SUCCESS"] != 1 {
		t.Fatalf("after retry-failed: %v (%v)", counts, tail(h.sessions, 12))
	}
	if n := len(dstA.Folder("INBOX").Msgs); n != 20 {
		t.Fatalf("after recovery: %d delivered (want 20)", n)
	}
	recovered := 0
	for _, st := range readRegistryStatus(t, h.cfg.DBPath) {
		if st == "RECOVERED" {
			recovered++
		}
	}
	if recovered != 2 {
		t.Fatalf("registry RECOVERED rows: %d (want 2)", recovered)
	}
}

func TestStaleRecoveryAutoResumes(t *testing.T) {
	srcA := fakeimap.NewAccount("us", "p")
	for i := 0; i < 40; i++ {
		srcA.Folder("INBOX").Add(msgBody(i, "stale", true, 1500), nil,
			"17-Jul-2026 10:00:00 +0000")
	}
	dstA := fakeimap.NewAccount("us", "p")
	h := newHarness(t, srcA, dstA)
	h.src.StallAfterN.Store(10) // hang once after 10 bodies, then recover
	h.src.StallOnce.Store(true)

	counts := h.run(func(c *config.Run) {
		c.Workers = 1
		c.Timeout = 60
		c.StaleTimeout = 2
		c.RecoveryInterval = 1
		c.RecoveryRetries = 3
		c.RetryDelay = 1
	})
	if counts["SUCCESS"] != 1 {
		t.Fatalf("stale recovery run: %v (%v)", counts, tail(h.sessions, 14))
	}
	if n := len(dstA.Folder("INBOX").Msgs); n != 40 {
		t.Fatalf("delivered %d (want 40)", n)
	}
	if u := uniqueBodies(dstA.Folder("INBOX")); u != 40 {
		t.Fatalf("stale recovery duplicated: %d unique", u)
	}
	if !h.logContains("stalled transfer detected") {
		t.Fatalf("no stall detected: %v", tail(h.sessions, 14))
	}
	if !h.logContains("transfer recovered") {
		t.Fatalf("no recovery logged: %v", tail(h.sessions, 14))
	}
}

func readRegistry(t *testing.T, dbPath string) map[uint32]string {
	con, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer con.Close()
	rows, err := con.Query("SELECT src_uid, ftype FROM failed_messages ORDER BY src_uid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[uint32]string{}
	for rows.Next() {
		var u uint32
		var ft string
		rows.Scan(&u, &ft)
		out[u] = ft
	}
	return out
}

func readRegistryStatus(t *testing.T, dbPath string) map[uint32]string {
	con, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer con.Close()
	rows, _ := con.Query("SELECT src_uid, status FROM failed_messages")
	defer rows.Close()
	out := map[uint32]string{}
	for rows.Next() {
		var u uint32
		var st string
		rows.Scan(&u, &st)
		out[u] = st
	}
	return out
}

var _ = os.MkdirAll
var _ = filepath.Join
var _ = context.Background
