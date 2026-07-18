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

// Multi-process concurrency tests: real OS processes (the actual built
// binary), one shared canonical State Database, in-process fake IMAP
// servers. Verifies concurrent independent runs, already-active
// reporting, mixed CSVs, no duplicate simultaneous processing, stale
// reclaim across processes, and unique run/worker identities.
package mailferry_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ajsap/mailferry/v2/internal/fakeimap"
)

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mf")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/mailferry")
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func acct(user string, n int) *fakeimap.Account {
	a := fakeimap.NewAccount(user, "pw")
	in := a.Folder("INBOX")
	for i := 1; i <= n; i++ {
		in.Add([]byte(fmt.Sprintf("Message-ID: <%s%d@example.test>\r\n"+
			"From: %s@example.com\r\nTo: t@example.org\r\nSubject: m %d\r\n"+
			"Date: Fri, 17 Jul 2026 08:00:00 +0000\r\n\r\np %d\r\n",
			user, i, user, i, i)), nil, "17-Jul-2026 08:00:00 +0000")
	}
	return a
}

func csvFor(t *testing.T, dir, name string, sp, dp int, pairs [][2]string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("srchost,srcport,srcsecurity,srcuser,srcpassword," +
		"dsthost,dstport,dstsecurity,dstuser,dstpassword\n")
	for _, p := range pairs {
		fmt.Fprintf(&b, "127.0.0.1,%d,none,%s,pw,127.0.0.1,%d,none,%s,pw\n",
			sp, p[0], dp, p[1])
	}
	path := filepath.Join(dir, name)
	os.WriteFile(path, []byte(b.String()), 0o600)
	return path
}

func mf(bin, home, db string, args ...string) *exec.Cmd {
	cmd := exec.Command(bin, append(append([]string{"run"}, args[0]),
		append([]string{"--no-tui", "--db", db, "--stale-timeout", "0",
			"--retry-delay", "1"}, args[1:]...)...)...)
	cmd.Env = append(os.Environ(), "HOME="+home,
		"XDG_CONFIG_HOME="+home+"/.config", "XDG_STATE_HOME="+home+"/.local/state",
		"XDG_CACHE_HOME="+home+"/.cache", "MAILFERRY_CONFIG_DIR=")
	return cmd
}

func TestMultiProcessConcurrencyAndClaiming(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	home := t.TempDir()
	db := filepath.Join(dir, "mailferry.db")

	aliceA, bobA, caraA := acct("alice", 40), acct("bob", 12), acct("cara", 24)
	src := fakeimap.NewServer(aliceA, bobA, caraA)
	dst := fakeimap.NewServer(fakeimap.NewAccount("alice2", "pw"),
		fakeimap.NewAccount("bob2", "pw"), fakeimap.NewAccount("cara2", "pw"))
	dst.AppendDelayMS.Store(110) // slow appends: real contention windows
	if err := src.Start(); err != nil {
		t.Fatal(err)
	}
	if err := dst.Start(); err != nil {
		t.Fatal(err)
	}
	defer src.Stop()
	defer dst.Stop()

	aCSV := csvFor(t, dir, "a.csv", src.Port(), dst.Port(), [][2]string{{"alice", "alice2"}})
	mixCSV := csvFor(t, dir, "mix.csv", src.Port(), dst.Port(),
		[][2]string{{"alice", "alice2"}, {"bob", "bob2"}})
	cCSV := csvFor(t, dir, "c.csv", src.Port(), dst.Port(), [][2]string{{"cara", "cara2"}})

	// --- P1 owns alice (≈4.4 s of slow appends) -------------------------
	p1 := mf(bin, home, db, aCSV)
	var p1out strings.Builder
	p1.Stdout, p1.Stderr = &p1out, &p1out
	if err := p1.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // alice leased and transferring

	// --- P2: mixed CSV — must do bob now, report alice as active, then
	// top alice up after P1 releases it, and exit 0 on its own ----------
	p2 := mf(bin, home, db, mixCSV)
	var p2out strings.Builder
	p2.Stdout, p2.Stderr = &p2out, &p2out
	if err := p2.Start(); err != nil {
		t.Fatal(err)
	}
	p2done := make(chan error, 1)
	go func() { p2done <- p2.Wait() }()

	if err := p1.Wait(); err != nil {
		t.Fatalf("p1: %v\n%s", err, p1out.String())
	}
	select {
	case err := <-p2done:
		if err != nil {
			t.Fatalf("p2: %v\n%s", err, p2out.String())
		}
	case <-time.After(60 * time.Second):
		p2.Process.Kill()
		t.Fatalf("p2 did not finish after p1 released alice\n%s", p2out.String())
	}

	o1, o2 := p1out.String(), p2out.String()
	if !strings.Contains(o2, "already active") && !strings.Contains(o2, "REMOTE") {
		t.Fatalf("p2 must clearly report the held mailbox, got:\n%s", o2)
	}
	// no duplicate simultaneous processing: destination totals are exact
	if got := len(dst.Accounts["alice2"].Folder("INBOX").Msgs); got != 40 {
		t.Fatalf("alice2 has %d messages, want exactly 40 (no duplicates)", got)
	}
	if got := len(dst.Accounts["bob2"].Folder("INBOX").Msgs); got != 12 {
		t.Fatalf("bob2 has %d messages, want exactly 12", got)
	}
	// unique run + worker identities on both processes
	id := func(out string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "Run ") {
				return l
			}
		}
		return ""
	}
	if id(o1) == "" || id(o2) == "" || id(o1) == id(o2) {
		t.Fatalf("run/worker identity lines missing or identical:\np1=%q\np2=%q",
			id(o1), id(o2))
	}

	// --- Scenario D across real processes: kill -9 the owner, another
	// process must reclaim via the stale-worker mechanism ----------------
	p3 := mf(bin, home, db, cCSV, "--worker-timeout", "3")
	var p3out strings.Builder
	p3.Stdout, p3.Stderr = &p3out, &p3out
	if err := p3.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond) // cara leased, mid-transfer
	p3.Process.Signal(syscall.SIGKILL)  // simulated crash: no cleanup at all
	p3.Wait()

	p4 := mf(bin, home, db, cCSV, "--worker-timeout", "3")
	out4, err := p4.CombinedOutput()
	if err != nil {
		t.Fatalf("p4 (reclaim): %v\n%s", err, out4)
	}
	if got := len(dst.Accounts["cara2"].Folder("INBOX").Msgs); got != 24 {
		t.Fatalf("cara2 has %d messages after crash+reclaim, want exactly 24 "+
			"(no loss, no duplicates)", got)
	}

	// --- resume/idempotency across processes: everything re-runs to zero
	p5 := mf(bin, home, db, mixCSV)
	out5, err := p5.CombinedOutput()
	if err != nil {
		t.Fatalf("p5 rerun: %v\n%s", err, out5)
	}
	if !strings.Contains(string(out5), "copied (new)       : 0") {
		t.Fatalf("rerun must copy nothing:\n%s", out5)
	}
}
