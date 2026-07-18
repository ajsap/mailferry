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

// Per-OS path resolution tests: every target OS is verified from any OS,
// and resolution is proven to create nothing on disk.
package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestMacOSNativePaths(t *testing.T) {
	p := For("darwin", env(nil), "/Users/jane")
	want := map[string]string{
		p.ConfigFile: "/Users/jane/Library/Application Support/MailFerry/mailferry.toml",
		p.StateDB:    "/Users/jane/Library/Application Support/MailFerry/mailferry.db",
		p.LogsDir:    "/Users/jane/Library/Logs/MailFerry",
		p.CacheDir:   "/Users/jane/Library/Caches/MailFerry",
	}
	for got, exp := range want {
		if got != exp {
			t.Fatalf("got %q want %q", got, exp)
		}
	}
	if p.LegacyConfigFile != "/Users/jane/.config/mailferry/mailferry.toml" {
		t.Fatalf("legacy config: %q", p.LegacyConfigFile)
	}
}

func TestLinuxXDGSet(t *testing.T) {
	e := env(map[string]string{
		"XDG_CONFIG_HOME": "/tmp/xdgc",
		"XDG_STATE_HOME":  "/tmp/xdgs",
		"XDG_CACHE_HOME":  "/tmp/xdgk",
	})
	p := For("linux", e, "/home/jane")
	if p.ConfigFile != "/tmp/xdgc/mailferry/mailferry.toml" {
		t.Fatalf("config: %q", p.ConfigFile)
	}
	if p.StateDB != "/tmp/xdgs/mailferry/mailferry.db" {
		t.Fatalf("db: %q", p.StateDB)
	}
	if p.LogsDir != "/tmp/xdgs/mailferry/logs" {
		t.Fatalf("logs: %q", p.LogsDir)
	}
	if p.CacheDir != "/tmp/xdgk/mailferry" {
		t.Fatalf("cache: %q", p.CacheDir)
	}
}

func TestLinuxXDGAbsentFallbacks(t *testing.T) {
	p := For("linux", env(nil), "/home/jane")
	if p.ConfigFile != "/home/jane/.config/mailferry/mailferry.toml" {
		t.Fatalf("config fallback: %q", p.ConfigFile)
	}
	if p.StateDB != "/home/jane/.local/state/mailferry/mailferry.db" {
		t.Fatalf("db fallback: %q", p.StateDB)
	}
	if p.LogsDir != "/home/jane/.local/state/mailferry/logs" {
		t.Fatalf("logs fallback: %q", p.LogsDir)
	}
	if p.CacheDir != "/home/jane/.cache/mailferry" {
		t.Fatalf("cache fallback: %q", p.CacheDir)
	}
}

func TestWindowsNativePaths(t *testing.T) {
	e := env(map[string]string{
		"APPDATA":      `C:\Users\Jane\AppData\Roaming`,
		"LOCALAPPDATA": `C:\Users\Jane\AppData\Local`,
	})
	p := For("windows", e, `C:\Users\Jane`)
	if p.ConfigFile != `C:\Users\Jane\AppData\Roaming\MailFerry\mailferry.toml` {
		t.Fatalf("config: %q", p.ConfigFile)
	}
	if p.StateDB != `C:\Users\Jane\AppData\Local\MailFerry\mailferry.db` {
		t.Fatalf("db: %q", p.StateDB)
	}
	if p.LogsDir != `C:\Users\Jane\AppData\Local\MailFerry\Logs` {
		t.Fatalf("logs: %q", p.LogsDir)
	}
	if p.CacheDir != `C:\Users\Jane\AppData\Local\MailFerry\Cache` {
		t.Fatalf("cache: %q", p.CacheDir)
	}
	// correct separators for the target OS
	for _, s := range []string{p.ConfigFile, p.StateDB, p.LogsDir, p.CacheDir} {
		for _, r := range s {
			if r == '/' {
				t.Fatalf("windows path with forward slash: %q", s)
			}
		}
	}
}

func TestWindowsEnvAbsentFallsBackToProfile(t *testing.T) {
	p := For("windows", env(nil), `C:\Users\Jane`)
	if p.ConfigFile != `C:\Users\Jane\AppData\Roaming\MailFerry\mailferry.toml` {
		t.Fatalf("config fallback: %q", p.ConfigFile)
	}
}

// Resolution must never create anything — the core zero-side-effect rule.
func TestResolutionCreatesNothing(t *testing.T) {
	home := t.TempDir()
	for _, goos := range []string{"darwin", "linux", "windows"} {
		For(goos, env(nil), home)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("path resolution created entries: %v", entries)
	}
}

func TestEnsureAndRestrict(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "MailFerry", "sub", "state.db")
	if err := EnsureParent(file); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("dir perm %v, want 0700", st.Mode().Perm())
	}
	os.WriteFile(file, []byte("x"), 0o644)
	Restrict(file)
	st, _ = os.Stat(file)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file perm %v, want 0600", st.Mode().Perm())
	}
}
