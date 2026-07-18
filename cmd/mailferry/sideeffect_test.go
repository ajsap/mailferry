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

// Zero-side-effect regression tests: informational commands must never
// create configuration, state, logs, cache or any application file. They
// exercise the real command dispatch (run()) inside a fully sandboxed
// HOME / XDG / working directory.
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// sandbox points every path-resolution input at empty temp directories
// and switches the working directory there too. It returns the roots to
// inspect afterwards.
func sandbox(t *testing.T) (home, cwd string) {
	t.Helper()
	home = t.TempDir()
	cwd = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("MAILFERRY_CONFIG_DIR", "") // force native resolution
	os.Unsetenv("MAILFERRY_CONFIG_DIR")
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	return home, cwd
}

// treeOf lists every file/dir below root (relative paths).
func treeOf(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, rel)
		return nil
	})
	return out
}

func TestInformationalCommandsHaveZeroSideEffects(t *testing.T) {
	cases := [][]string{
		{"--help"}, {"-h"}, {"help"},
		{"version"}, {"--version"}, {"-V"},
		{"about"}, {"--about"},
		{"changelog"}, {"roadmap"},
		{"config", "paths"},
		{"status"},       // no DB exists: must error, never create one
		{"failed"},       // same
		{"retry-failed"}, // same
		{},               // bare invocation -> usage
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
			home, cwd := sandbox(t)
			run(append([]string(nil), argv...))
			if got := treeOf(t, home); len(got) != 0 {
				t.Fatalf("%v created files under HOME: %v", argv, got)
			}
			if got := treeOf(t, cwd); len(got) != 0 {
				t.Fatalf("%v created files in the working directory: %v", argv, got)
			}
		})
	}
}

func TestConfigCommandCreatesExplicitly(t *testing.T) {
	home, cwd := sandbox(t)
	if rc := run([]string{"config"}); rc != 0 {
		t.Fatalf("config rc=%d", rc)
	}
	cfg := filepath.Join(home, ".config", "mailferry", "mailferry.toml")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("explicit `mailferry config` must create the configuration: %v", err)
	}
	// nothing else appears — no DB, no logs, no cache
	for _, unwanted := range []string{
		filepath.Join(home, ".local", "state", "mailferry", "mailferry.db"),
		filepath.Join(home, ".local", "state", "mailferry", "logs"),
		filepath.Join(home, ".cache", "mailferry"),
	} {
		if _, err := os.Stat(unwanted); err == nil {
			t.Fatalf("`mailferry config` must not create %s", unwanted)
		}
	}
	if got := treeOf(t, cwd); len(got) != 0 {
		t.Fatalf("config polluted the working directory: %v", got)
	}
	// never overwritten on a second explicit call
	if rc := run([]string{"config"}); rc != 0 {
		t.Fatalf("second config rc=%d", rc)
	}
	data2, _ := os.ReadFile(cfg)
	if string(data) != string(data2) {
		t.Fatal("`mailferry config` rewrote an existing configuration")
	}
	// restrictive permissions on the generated file
	if st, _ := os.Stat(cfg); st.Mode().Perm() != 0o600 {
		t.Fatalf("config file permissions %v, want 0600", st.Mode().Perm())
	}
}

func TestReadOnlyDBCommandsNeverCreateADatabase(t *testing.T) {
	home, _ := sandbox(t)
	native := filepath.Join(home, ".local", "state", "mailferry", "mailferry.db")
	for _, argv := range [][]string{{"status"}, {"failed"}, {"compact"},
		{"retry-failed"}} {
		if rc := run(append([]string(nil), argv...)); rc != 1 {
			t.Fatalf("%v with no database: rc=%d, want 1", argv, rc)
		}
		if _, err := os.Stat(native); err == nil {
			t.Fatalf("%v created a State Database as a side effect", argv)
		}
	}
}

func TestLegacyDevelopmentDBIsDetectedNotAdopted(t *testing.T) {
	home, cwd := sandbox(t)
	// a pre-rc.2 development database sits in the working directory
	os.WriteFile(filepath.Join(cwd, "migration.db"), []byte("x"), 0o600)
	db, hint := resolveStateDB("")
	if hint == "" {
		t.Fatal("legacy ./migration.db must produce an explicit hint")
	}
	if db != filepath.Join(home, ".local", "state", "mailferry", "mailferry.db") {
		t.Fatalf("resolved db drifted: %s", db)
	}
	// explicit --db always wins, no hint
	db, hint = resolveStateDB("./migration.db")
	if db != "./migration.db" || hint != "" {
		t.Fatalf("explicit --db must be honoured verbatim (db=%s hint=%q)", db, hint)
	}
}
