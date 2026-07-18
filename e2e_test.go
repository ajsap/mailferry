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

// End-to-end engine tests against the in-memory fake IMAP server:
// fresh migration, idempotent re-run, adoption of a pre-synced destination,
// incremental top-up, resume across an ACK-lost APPEND (no duplicates),
// per-message failure handling, and cross-engine fingerprint compatibility.
package mailferry_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/engine"
	"github.com/ajsap/mailferry/v2/internal/fakeimap"
	"github.com/ajsap/mailferry/v2/internal/util"
)

func msgBody(i int, subj string, withMID bool, pad int) []byte {
	mid := ""
	if withMID {
		mid = fmt.Sprintf("Message-ID: <m%d@example.test>\r\n", i)
	}
	return []byte(fmt.Sprintf("From: alice@example.test\r\nTo: bob@example.test\r\n"+
		"Subject: %s %d\r\nDate: Thu, 16 Jul 2026 10:%02d:00 +0000\r\n%s\r\n"+
		"Body of message %d.\r\n%s", subj, i, i%60, mid, i,
		strings.Repeat("X", pad)))
}

func buildSrc() *fakeimap.Account {
	a := fakeimap.NewAccount("alice", "pw1")
	inbox := a.Folder("INBOX")
	for i := 1; i <= 40; i++ {
		var flags []string
		if i%3 == 0 {
			flags = append(flags, `\Seen`)
		}
		if i%7 == 0 {
			flags = append(flags, `\Flagged`)
		}
		if i%11 == 0 {
			flags = append(flags, "$Label1")
		}
		inbox.Add(msgBody(i, "inbox", i != 5 && i != 6, i*37), flags,
			fmt.Sprintf("%02d-Jun-2026 09:00:00 +0000", (i%27)+1))
	}
	sent := a.AddFolder(fakeimap.NewFolder("Sent", 1201, `\Sent`))
	for i := 100; i < 110; i++ {
		sent.Add(msgBody(i, "sent", true, 0), []string{`\Seen`}, "17-Jul-2026 10:00:00 +0000")
	}
	arch := a.AddFolder(fakeimap.NewFolder("Archive/2025", 1301))
	for i := 200; i < 215; i++ {
		arch.Add(msgBody(i, "archive", true, 500), nil, "17-Jul-2026 10:00:00 +0000")
	}
	return a
}

type harness struct {
	t        *testing.T
	src, dst *fakeimap.Server
	srcA     *fakeimap.Account
	dstA     *fakeimap.Account
	dir      string
	cfg      *config.Run
	stats    *engine.Stats
	sessions []string
}

func newHarness(t *testing.T, srcA, dstA *fakeimap.Account) *harness {
	h := &harness{t: t, srcA: srcA, dstA: dstA, dir: t.TempDir()}
	h.src = fakeimap.NewServer(srcA)
	h.dst = fakeimap.NewServer(dstA)
	if err := h.src.Start(); err != nil {
		t.Fatal(err)
	}
	if err := h.dst.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.src.Stop(); h.dst.Stop() })
	return h
}

func (h *harness) run(mutate func(*config.Run)) map[string]int {
	cfg := config.Defaults()
	cfg.CSVFile = "test.csv"
	cfg.DBPath = filepath.Join(h.dir, "migration.db")
	cfg.LogsDir = filepath.Join(h.dir, "logs")
	cfg.Workers = 2
	cfg.Timeout = 30
	cfg.RetryDelay = 1
	cfg.RunID = time.Now().Format("20060102-150405.000")
	if mutate != nil {
		mutate(cfg)
	}
	h.cfg = cfg
	os.MkdirAll(cfg.LogsDir, 0o755)
	specs := []config.MailboxSpec{{
		Index: 1,
		Src: config.Endpoint{Host: "127.0.0.1", Port: h.src.Port(), Security: "none",
			User: h.srcA.User, Password: h.srcA.Password},
		Dst: config.Endpoint{Host: "127.0.0.1", Port: h.dst.Port(), Security: "none",
			User: h.dstA.User, Password: h.dstA.Password},
	}}
	h.stats = engine.NewStats()
	logf := func(spec config.MailboxSpec) func(string, ...any) {
		return func(format string, a ...any) {
			h.sessions = append(h.sessions, fmt.Sprintf(format, a...))
		}
	}
	res, err := engine.RunMigration(context.Background(), cfg, specs, h.stats,
		func(s string) { h.sessions = append(h.sessions, s) }, logf)
	if err != nil {
		h.t.Fatal(err)
	}
	return res.Counts
}

func (h *harness) logContains(sub string) bool {
	for _, l := range h.sessions {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func uniqueBodies(f *fakeimap.Folder) int {
	seen := map[string]bool{}
	for _, m := range f.Msgs {
		seen[string(m.Body)] = true
	}
	return len(seen)
}

func TestFreshMigrationAndIdempotentRerun(t *testing.T) {
	srcA := buildSrc()
	dstA := fakeimap.NewAccount("bob", "pw2")
	h := newHarness(t, srcA, dstA)

	counts := h.run(nil)
	if counts["SUCCESS"] != 1 {
		t.Fatalf("expected SUCCESS, got %v (log tail: %v)", counts, tail(h.sessions, 8))
	}
	if n := len(dstA.Folder("INBOX").Msgs); n != 40 {
		t.Fatalf("INBOX: got %d msgs, want 40", n)
	}
	if f := dstA.Folder("Sent"); f == nil || len(f.Msgs) != 10 {
		t.Fatalf("Sent folder missing or wrong count")
	}
	if f := dstA.Folder("Archive/2025"); f == nil || len(f.Msgs) != 15 {
		t.Fatalf("hierarchy folder missing or wrong count")
	}
	if got, want := dstA.TotalMsgs(), srcA.TotalMsgs(); got != want {
		t.Fatalf("total: %d != %d", got, want)
	}
	// flags + internaldate preserved
	var s3 *fakeimap.Msg
	for _, m := range srcA.Folder("INBOX").Msgs {
		if m.UID == 3 {
			s3 = m
		}
	}
	found := false
	for _, m := range dstA.Folder("INBOX").Msgs {
		if string(m.Body) == string(s3.Body) {
			found = true
			if !m.Flags[`\Seen`] {
				t.Fatalf("flags not preserved: %v", m.Flags)
			}
			if m.InternalDate != s3.InternalDate {
				t.Fatalf("internaldate not preserved: %q vs %q", m.InternalDate, s3.InternalDate)
			}
		}
	}
	if !found {
		t.Fatal("byte-identical body not found on destination")
	}

	// idempotent re-run: zero new appends
	before := h.dst.AppendCount.Load()
	counts = h.run(nil)
	if counts["SUCCESS"] != 1 {
		t.Fatalf("rerun: %v", counts)
	}
	if d := h.dst.AppendCount.Load() - before; d != 0 {
		t.Fatalf("rerun appended %d messages (want 0)", d)
	}
}

func TestAdoptionAfterLostDB(t *testing.T) {
	srcA := buildSrc()
	dstA := fakeimap.NewAccount("bob", "pw2")
	h := newHarness(t, srcA, dstA)
	h.run(nil) // populate destination

	// lose the DB: fresh one must adopt, never duplicate
	before := h.dst.AppendCount.Load()
	counts := h.run(func(c *config.Run) {
		c.DBPath = filepath.Join(h.dir, "migration-fresh.db")
	})
	if counts["SUCCESS"] != 1 {
		t.Fatalf("adoption run: %v (%v)", counts, tail(h.sessions, 8))
	}
	if d := h.dst.AppendCount.Load() - before; d != 0 {
		t.Fatalf("adoption appended %d messages (want 0)", d)
	}
	if got, want := dstA.TotalMsgs(), srcA.TotalMsgs(); got != want {
		t.Fatalf("adoption changed totals: %d != %d", got, want)
	}
	if !h.logContains("adopted") {
		t.Fatal("adoption not logged")
	}
}

func TestIncrementalTopUp(t *testing.T) {
	srcA := buildSrc()
	dstA := fakeimap.NewAccount("bob", "pw2")
	h := newHarness(t, srcA, dstA)
	h.run(nil)

	for i := 500; i < 503; i++ {
		srcA.Folder("INBOX").Add(msgBody(i, "new", true, 0), []string{`\Seen`},
			"18-Jul-2026 10:00:00 +0000")
	}
	before := h.dst.AppendCount.Load()
	counts := h.run(nil)
	if counts["SUCCESS"] != 1 {
		t.Fatalf("topup: %v", counts)
	}
	if d := h.dst.AppendCount.Load() - before; d != 3 {
		t.Fatalf("topup appended %d (want exactly 3)", d)
	}
	if n := len(dstA.Folder("INBOX").Msgs); n != 43 {
		t.Fatalf("INBOX after topup: %d (want 43)", n)
	}
}

// TestAckLostAppendNoDuplicate is THE critical correctness scenario: the
// destination accepts an APPEND and dies before MailFerry sees the OK.
// The resume must reconcile and never append that message twice.
func TestAckLostAppendNoDuplicate(t *testing.T) {
	srcA := fakeimap.NewAccount("alice", "pw1")
	for i := 1; i <= 12; i++ {
		srcA.Folder("INBOX").Add(msgBody(i, "ack", true, 200), nil,
			"17-Jul-2026 10:00:00 +0000")
	}
	dstA := fakeimap.NewAccount("bob", "pw2")
	h := newHarness(t, srcA, dstA)
	h.dst.DropAfterOKN.Store(5) // 5th append: message stored, OK sent, line dies

	counts := h.run(nil)
	if counts["SUCCESS"] != 1 {
		t.Fatalf("ack-lost run: %v (%v)", counts, tail(h.sessions, 10))
	}
	if n := len(dstA.Folder("INBOX").Msgs); n != 12 {
		t.Fatalf("dst INBOX: %d (want 12)", n)
	}
	if u := uniqueBodies(dstA.Folder("INBOX")); u != 12 {
		t.Fatalf("DUPLICATES: %d unique of %d", u, len(dstA.Folder("INBOX").Msgs))
	}
	if !h.logContains("reconnecting") && !h.logContains("reconcil") {
		t.Fatalf("expected a reconnect/reconcile in the log: %v", tail(h.sessions, 10))
	}
}

func TestRejectedMessagesDoNotStopTheMailbox(t *testing.T) {
	srcA := fakeimap.NewAccount("alice", "pw1")
	for i := 1; i <= 10; i++ {
		body := msgBody(i, "rej", true, 0)
		if i == 4 {
			body = append(body, []byte("REJECTME")...)
		}
		srcA.Folder("INBOX").Add(body, nil, "17-Jul-2026 10:00:00 +0000")
	}
	dstA := fakeimap.NewAccount("bob", "pw2")
	h := newHarness(t, srcA, dstA)
	h.dst.AppendReject = []byte("REJECTME")

	counts := h.run(nil)
	if counts["WARNINGS"] != 1 {
		t.Fatalf("want WARNINGS, got %v (%v)", counts, tail(h.sessions, 8))
	}
	if n := len(dstA.Folder("INBOX").Msgs); n != 9 {
		t.Fatalf("healthy delivered: %d (want 9)", n)
	}
}

func TestResumeAfterHardStop(t *testing.T) {
	srcA := fakeimap.NewAccount("alice", "pw1")
	for i := 1; i <= 30; i++ {
		srcA.Folder("INBOX").Add(msgBody(i, "resume", true, 4000), nil,
			"17-Jul-2026 10:00:00 +0000")
	}
	dstA := fakeimap.NewAccount("bob", "pw2")
	h := newHarness(t, srcA, dstA)
	h.dst.AppendDelayMS.Store(25) // slow server: the cancel lands mid-transfer

	// first run: cancel shortly after start (simulates Ctrl+C mid-transfer)
	cfg := config.Defaults()
	cfg.CSVFile = "test.csv"
	cfg.DBPath = filepath.Join(h.dir, "migration.db")
	cfg.LogsDir = filepath.Join(h.dir, "logs")
	cfg.Workers = 1
	cfg.Timeout = 30
	cfg.RunID = "cancelrun"
	os.MkdirAll(cfg.LogsDir, 0o755)
	specs := []config.MailboxSpec{{
		Index: 1,
		Src: config.Endpoint{Host: "127.0.0.1", Port: h.src.Port(), Security: "none",
			User: "alice", Password: "pw1"},
		Dst: config.Endpoint{Host: "127.0.0.1", Port: h.dst.Port(), Security: "none",
			User: "bob", Password: "pw2"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for h.dst.AppendCount.Load() < 5 {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	stats := engine.NewStats()
	engine.RunMigration(ctx, cfg, specs, stats, func(string) {},
		func(config.MailboxSpec) func(string, ...any) {
			return func(string, ...any) {}
		})
	delivered := len(dstA.Folder("INBOX").Msgs)
	if delivered == 0 || delivered >= 30 {
		t.Fatalf("expected a partial delivery before cancel, got %d", delivered)
	}
	h.dst.AppendDelayMS.Store(0)

	// resume completes without duplicating anything
	counts := h.run(nil)
	if counts["SUCCESS"] != 1 {
		t.Fatalf("resume: %v (%v)", counts, tail(h.sessions, 8))
	}
	if n := len(dstA.Folder("INBOX").Msgs); n != 30 {
		t.Fatalf("after resume: %d (want 30)", n)
	}
	if u := uniqueBodies(dstA.Folder("INBOX")); u != 30 {
		t.Fatalf("resume created duplicates: %d unique", u)
	}
}

// TestFingerprintPythonCompat pins byte-compatibility with the Python
// engine so mixed-version use of one migration.db adopts identically.
func TestFingerprintPythonCompat(t *testing.T) {
	// values produced by the Python reference implementation
	cases := []struct {
		header string
		size   int64
		want   string
	}{
		{"Message-ID: <abc@x>\r\nSubject: s\r\n", 100, "m:abc@x"},
		{"Message-ID:\r\n <xy z@q>\r\nDate: D\r\n", 5, "m:xyz@q"},
		{"Subject: hello\r\n world folded\r\nFrom: A B\r\n <a@b>\r\nTo: t@x\r\nDate: Fri, 17 Jul\r\n",
			42, "h:" + pyHash("Fri, 17 Jul\x00A B\r\n <a@b>\x00t@x\x00hello\r\n world folded\x0042")},
	}
	for _, c := range cases {
		got := util.FingerprintFromHeaders([]byte(c.header), c.size)
		if got != c.want {
			t.Fatalf("fingerprint mismatch:\n header %q\n got  %s\n want %s",
				c.header, got, c.want)
		}
	}
}

func pyHash(basis string) string {
	// mirror: sha256(basis)[:32]
	return util.Sha256Hex32(basis)
}

func tail(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}
