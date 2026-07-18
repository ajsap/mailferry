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

// --portable command-level tests: informational commands stay zero-side-effect
// under portable mode, `config paths` reflects the portable layout, an
// operational run lands mailferry.toml/mailferry.db/logs in the portable root
// and resumes from it, and an explicit --db still beats portable. Portable is
// driven through the package seam (paths.SetPortable) so the portable root is a
// controlled temp dir rather than the test binary's directory.
package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ajsap/mailferry/v2/internal/fakeimap"
	"github.com/ajsap/mailferry/v2/internal/paths"
)

// portableSandbox sets a controlled portable root (the package seam that the
// e2e note calls for) and the same empty HOME/XDG as the native sandbox, so a
// leak to EITHER the portable root or the native locations is caught.
func portableSandbox(t *testing.T) (root, home, cwd string) {
	t.Helper()
	home, cwd = sandbox(t)
	root = t.TempDir()
	paths.SetPortable(root)
	t.Cleanup(func() { paths.SetPortable("") })
	return root, home, cwd
}

func TestPortableInformationalCommandsHaveZeroSideEffects(t *testing.T) {
	cases := [][]string{
		{"--help"}, {"version"}, {"about"},
		{"changelog"}, {"roadmap"},
		{"config", "paths"},
		{"status"}, {"failed"}, {"retry-failed"},
	}
	for _, argv := range cases {
		name := "bare"
		if len(argv) > 0 {
			name = argv[0]
			if len(argv) > 1 {
				name += "_" + argv[1]
			}
		}
		t.Run(name, func(t *testing.T) {
			root, home, cwd := portableSandbox(t)
			run(append([]string(nil), argv...))
			if got := treeOf(t, root); len(got) != 0 {
				t.Fatalf("%v (portable) created files in the portable root: %v", argv, got)
			}
			if got := treeOf(t, home); len(got) != 0 {
				t.Fatalf("%v (portable) created files under HOME: %v", argv, got)
			}
			if got := treeOf(t, cwd); len(got) != 0 {
				t.Fatalf("%v (portable) created files in the working directory: %v", argv, got)
			}
		})
	}
}

func TestPortableConfigPathsShowsPortableLayout(t *testing.T) {
	root, _, _ := portableSandbox(t)
	// Re-bootstrap so bootExplicit/bootCfg reflect portable (run() does this,
	// but config paths reads package state set during bootstrap).
	if rc := run([]string{"config", "paths"}); rc != 0 {
		t.Fatalf("config paths rc=%d", rc)
	}
	// The resolvers must now point every location into the portable root.
	p := paths.Default()
	if p.StateDB != filepath.Join(root, "mailferry.db") {
		t.Fatalf("portable StateDB = %q", p.StateDB)
	}
	if p.ConfigFile != filepath.Join(root, "mailferry.toml") {
		t.Fatalf("portable ConfigFile = %q", p.ConfigFile)
	}
	if p.LogsDir != filepath.Join(root, "logs") {
		t.Fatalf("portable LogsDir = %q", p.LogsDir)
	}
	// config paths must have created nothing.
	if got := treeOf(t, root); len(got) != 0 {
		t.Fatalf("config paths (portable) created files: %v", got)
	}
}

func TestPortableOperationalRunLandsInPortableRootAndResumes(t *testing.T) {
	root, _, _ := portableSandbox(t)

	srcA := fakeimap.NewAccount("alice", "pw1")
	for i := 1; i <= 6; i++ {
		srcA.Folder("INBOX").Add([]byte(
			"Message-ID: <p"+itoa(i)+"@x>\r\nFrom: a@x\r\nTo: b@x\r\n"+
				"Subject: portable "+itoa(i)+"\r\nDate: Fri, 17 Jul 2026 08:00:00 +0000\r\n\r\nbody\r\n"),
			nil, "17-Jul-2026 08:00:00 +0000")
	}
	dstA := fakeimap.NewAccount("bob", "pw2")
	src := fakeimap.NewServer(srcA)
	dst := fakeimap.NewServer(dstA)
	if err := src.Start(); err != nil {
		t.Fatal(err)
	}
	if err := dst.Start(); err != nil {
		t.Fatal(err)
	}
	defer src.Stop()
	defer dst.Stop()

	csv := filepath.Join(t.TempDir(), "p.csv")
	os.WriteFile(csv, []byte(
		"srchost,srcport,srcsecurity,srcuser,srcpassword,"+
			"dsthost,dstport,dstsecurity,dstuser,dstpassword\n"+
			"127.0.0.1,"+itoa(src.Port())+",none,alice,pw1,"+
			"127.0.0.1,"+itoa(dst.Port())+",none,bob,pw2\n"), 0o600)

	if rc := run([]string{"run", csv, "--no-tui", "--workers", "1"}); rc != 0 {
		t.Fatalf("portable run rc=%d", rc)
	}
	// Everything lands in the portable root.
	for _, want := range []string{
		filepath.Join(root, "mailferry.toml"),
		filepath.Join(root, "mailferry.db"),
		filepath.Join(root, "logs", "session.log"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("portable run did not create %s: %v", want, err)
		}
	}
	if n := len(dstA.Folder("INBOX").Msgs); n != 6 {
		t.Fatalf("portable run delivered %d, want 6", n)
	}

	// Resume from the portable root: idempotent, copies nothing new.
	before := dst.AppendCount.Load()
	if rc := run([]string{"run", csv, "--no-tui", "--workers", "1"}); rc != 0 {
		t.Fatalf("portable resume rc=%d", rc)
	}
	if d := dst.AppendCount.Load() - before; d != 0 {
		t.Fatalf("portable resume re-appended %d (want 0 — resumed from the portable DB)", d)
	}
}

func TestPortableExplicitDBOverrideBeatsPortable(t *testing.T) {
	root, _, _ := portableSandbox(t)
	explicitDB := filepath.Join(t.TempDir(), "chosen.db")

	srcA := fakeimap.NewAccount("alice", "pw1")
	srcA.Folder("INBOX").Add([]byte(
		"Message-ID: <o1@x>\r\nFrom: a@x\r\nTo: b@x\r\nSubject: o\r\n"+
			"Date: Fri, 17 Jul 2026 08:00:00 +0000\r\n\r\nbody\r\n"),
		nil, "17-Jul-2026 08:00:00 +0000")
	dstA := fakeimap.NewAccount("bob", "pw2")
	src := fakeimap.NewServer(srcA)
	dst := fakeimap.NewServer(dstA)
	if err := src.Start(); err != nil {
		t.Fatal(err)
	}
	if err := dst.Start(); err != nil {
		t.Fatal(err)
	}
	defer src.Stop()
	defer dst.Stop()

	csv := filepath.Join(t.TempDir(), "p.csv")
	os.WriteFile(csv, []byte(
		"srchost,srcport,srcsecurity,srcuser,srcpassword,"+
			"dsthost,dstport,dstsecurity,dstuser,dstpassword\n"+
			"127.0.0.1,"+itoa(src.Port())+",none,alice,pw1,"+
			"127.0.0.1,"+itoa(dst.Port())+",none,bob,pw2\n"), 0o600)

	if rc := run([]string{"run", csv, "--no-tui", "--workers", "1", "--db", explicitDB}); rc != 0 {
		t.Fatalf("portable+--db run rc=%d", rc)
	}
	// Explicit --db wins: the chosen DB exists, the portable DB does NOT.
	if _, err := os.Stat(explicitDB); err != nil {
		t.Fatalf("explicit --db not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "mailferry.db")); err == nil {
		t.Fatal("explicit --db must beat portable: portable mailferry.db should not exist")
	}
}

// itoa avoids importing strconv just for these small conversions.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
