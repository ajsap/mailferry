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

// Parity regression tests for behaviours audited against the final Python
// reference: mailferry.toml lifecycle, COMPRESS=DEFLATE, baseline mode,
// trace redaction and --sync-flags.
package mailferry_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/fakeimap"
)

// --- mailferry.toml lifecycle ---------------------------------------------

func TestConfigFirstRunGeneratesTOML(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAILFERRY_CONFIG_DIR", dir)
	cfg := config.Defaults()
	warns, path, created := config.LoadTOML(cfg, "", true)
	if !created {
		t.Fatalf("first run must create the default TOML (warns=%v)", warns)
	}
	if path != filepath.Join(dir, "mailferry.toml") {
		t.Fatalf("unexpected path %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"# MailFerry Configuration", "[migration]", "[retry]",
		"[recovery]", "[logging]", "[dashboard]", "[database]", "stale_timeout_seconds"} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated TOML missing %q", want)
		}
	}
	// Second load: no overwrite, byte-identical, created=false.
	st0, _ := os.Stat(path)
	cfg2 := config.Defaults()
	_, _, created2 := config.LoadTOML(cfg2, "", true)
	if created2 {
		t.Fatal("second run must not re-create the config")
	}
	data2, _ := os.ReadFile(path)
	if string(data2) != text {
		t.Fatal("existing configuration was modified on reload")
	}
	st1, _ := os.Stat(path)
	if st0.ModTime() != st1.ModTime() {
		t.Fatal("existing configuration was rewritten (mtime changed)")
	}
}

func TestConfigNeverFatal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAILFERRY_CONFIG_DIR", dir)
	path := filepath.Join(dir, "mailferry.toml")
	os.WriteFile(path, []byte("this is not toml at all\n<<<>>>\n"), 0o644)
	cfg := config.Defaults()
	warns, _, _ := config.LoadTOML(cfg, "", true)
	joined := strings.Join(warns, " | ")
	if !strings.Contains(joined, "could not parse") {
		t.Fatalf("mangled config must warn, got %v", warns)
	}
	if cfg.Workers != 10 {
		t.Fatal("defaults must stand when the file is unreadable")
	}
}

func TestConfigUpgradeAppendsNewOptions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAILFERRY_CONFIG_DIR", dir)
	path := filepath.Join(dir, "mailferry.toml")
	old := "# MailFerry Configuration\n# (older version)\n\n[migration]\n" +
		"# Mailboxes migrated concurrently.\nparallel_mailboxes = 5\n"
	os.WriteFile(path, []byte(old), 0o644)
	cfg := config.Defaults()
	warns, _, _ := config.LoadTOML(cfg, "", true)
	if cfg.Workers != 5 {
		t.Fatalf("user customisation lost: workers=%d", cfg.Workers)
	}
	joined := strings.Join(warns, " | ")
	if !strings.Contains(joined, "documented") {
		t.Fatalf("upgrade must be announced, got %v", warns)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	if !strings.HasPrefix(text, old) {
		t.Fatal("upgrade must be append-only — existing content changed")
	}
	if !strings.Contains(text, "# stale_timeout_seconds = 300") {
		t.Fatal("new options must be appended as commented defaults")
	}
	if strings.Contains(strings.TrimPrefix(text, "#"), "\nstale_timeout_seconds") {
		t.Fatal("appended options must stay commented (semantics unchanged)")
	}
	// Idempotent: a second load appends nothing further.
	size1 := len(text)
	cfg3 := config.Defaults()
	config.LoadTOML(cfg3, "", true)
	data2, _ := os.ReadFile(path)
	if len(data2) != size1 {
		t.Fatal("upgrade append must be idempotent")
	}
	// The appended file still parses under the strict mini-parser.
	cfg4 := config.Defaults()
	warns4, _, _ := config.LoadTOML(cfg4, "", true)
	for _, w := range warns4 {
		if strings.Contains(w, "could not parse") {
			t.Fatalf("appended block broke parsing: %v", warns4)
		}
	}
	if cfg4.Workers != 5 {
		t.Fatal("customisation must survive the upgrade")
	}
}

// --- COMPRESS=DEFLATE / baseline / trace -----------------------------------

func TestCompressDeflateEndToEnd(t *testing.T) {
	h := newHarness(t, buildSrc(), fakeimap.NewAccount("bob", "pw2"))
	counts := h.run(nil) // Compress defaults to "auto"; fake servers offer DEFLATE
	if counts["SUCCESS"] != 1 {
		t.Fatalf("counts=%v", counts)
	}
	if h.src.CompressConns.Load() == 0 || h.dst.CompressConns.Load() == 0 {
		t.Fatalf("COMPRESS=DEFLATE not negotiated (src=%d dst=%d)",
			h.src.CompressConns.Load(), h.dst.CompressConns.Load())
	}
	if !h.logContains("COMPRESS=DEFLATE enabled") {
		t.Fatal("compression must be visible in the mailbox log")
	}
}

func TestCompressOffAndBaseline(t *testing.T) {
	h := newHarness(t, buildSrc(), fakeimap.NewAccount("bob", "pw2"))
	counts := h.run(func(c *config.Run) { c.Compress = "off" })
	if counts["SUCCESS"] != 1 {
		t.Fatalf("counts=%v", counts)
	}
	if h.src.CompressConns.Load() != 0 {
		t.Fatal("--compress off must not negotiate DEFLATE")
	}

	h2 := newHarness(t, buildSrc(), fakeimap.NewAccount("bob", "pw2"))
	counts2 := h2.run(func(c *config.Run) { c.Baseline = true })
	if counts2["SUCCESS"] != 1 {
		t.Fatalf("baseline counts=%v", counts2)
	}
	if h2.src.CompressConns.Load() != 0 || h2.dst.CompressConns.Load() != 0 {
		t.Fatal("--baseline must not negotiate DEFLATE")
	}
}

func TestTraceRedactsCredentials(t *testing.T) {
	h := newHarness(t, buildSrc(), fakeimap.NewAccount("bob", "pw2"))
	counts := h.run(func(c *config.Run) { c.Trace = true })
	if counts["SUCCESS"] != 1 {
		t.Fatalf("counts=%v", counts)
	}
	sawLogin, sawServer := false, false
	for _, l := range h.sessions {
		if strings.Contains(l, "pw1") || strings.Contains(l, "pw2") {
			t.Fatalf("password leaked into trace: %s", l)
		}
		if strings.Contains(l, "C: ") && strings.Contains(l, "LOGIN") {
			sawLogin = true
			if !strings.Contains(l, "****") {
				t.Fatalf("LOGIN trace not redacted: %s", l)
			}
		}
		if strings.Contains(l, "S: ") {
			sawServer = true
		}
	}
	if !sawLogin || !sawServer {
		t.Fatalf("trace lines missing (login=%v server=%v)", sawLogin, sawServer)
	}
}

// --- --sync-flags (backup mode) --------------------------------------------

func TestSyncFlagsReappliesChangedFlags(t *testing.T) {
	srcA := buildSrc()
	h := newHarness(t, srcA, fakeimap.NewAccount("bob", "pw2"))
	if c := h.run(nil); c["SUCCESS"] != 1 {
		t.Fatalf("first run: %v", c)
	}
	// Flip flags on two source messages after the initial sync.
	inbox := srcA.Folder("INBOX")
	changedUIDs := 0
	for _, m := range inbox.Msgs {
		if changedUIDs >= 2 {
			break
		}
		m.Flags = map[string]bool{`\Seen`: true, `\Flagged`: true}
		changedUIDs++
	}
	if c := h.run(func(cfg *config.Run) { cfg.SyncFlags = true }); c["SUCCESS"] != 1 {
		t.Fatalf("sync-flags run: %v", c)
	}
	events := h.dst.StoreEvents()
	if len(events) == 0 {
		t.Fatal("--sync-flags must issue UID STORE on the destination")
	}
	flagged := false
	for _, e := range events {
		if strings.Contains(e, `\Flagged`) {
			flagged = true
		}
	}
	if !flagged {
		t.Fatalf("changed flag not re-applied: %v", events)
	}
	// And it must be recorded so the next pass is a no-op.
	h.dst.StoreLog = nil
	if c := h.run(func(cfg *config.Run) { cfg.SyncFlags = true }); c["SUCCESS"] != 1 {
		t.Fatalf("idempotent run: %v", c)
	}
	if n := len(h.dst.StoreEvents()); n != 0 {
		t.Fatalf("flag sync must be idempotent, saw %d further STOREs", n)
	}
}
