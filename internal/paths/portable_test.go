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

// Portable-mode path resolution tests: given an executable directory, all
// four canonical locations must land directly inside it, resolution must
// create nothing, and the writability probe must detect a read-only root.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPortableForPutsEverythingBesideTheExecutable(t *testing.T) {
	root := "/opt/mailferry-portable"
	p := PortableFor(root)
	want := map[string]string{
		p.ConfigFile: filepath.Join(root, "mailferry.toml"),
		p.StateDB:    filepath.Join(root, "mailferry.db"),
		p.LogsDir:    filepath.Join(root, "logs"),
		p.CacheDir:   filepath.Join(root, "cache"),
	}
	for got, exp := range want {
		if got != exp {
			t.Fatalf("portable path: got %q want %q", got, exp)
		}
	}
}

func TestSetPortableRedirectsDefault(t *testing.T) {
	t.Cleanup(func() { SetPortable("") })
	if PortableActive() {
		t.Fatal("portable should start inactive")
	}
	root := t.TempDir()
	SetPortable(root)
	if !PortableActive() || PortableRoot() != root {
		t.Fatalf("SetPortable not reflected: active=%v root=%q", PortableActive(), PortableRoot())
	}
	d := Default()
	if d.StateDB != filepath.Join(root, "mailferry.db") {
		t.Fatalf("Default().StateDB under portable = %q", d.StateDB)
	}
	if d.ConfigFile != filepath.Join(root, "mailferry.toml") {
		t.Fatalf("Default().ConfigFile under portable = %q", d.ConfigFile)
	}
	if d.LogsDir != filepath.Join(root, "logs") {
		t.Fatalf("Default().LogsDir under portable = %q", d.LogsDir)
	}
	SetPortable("")
	if PortableActive() {
		t.Fatal("SetPortable(\"\") must disable portable mode")
	}
}

func TestPortableResolutionCreatesNothing(t *testing.T) {
	root := t.TempDir()
	PortableFor(root)
	SetPortable(root)
	_ = Default()
	SetPortable("")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("portable resolution created entries: %v", entries)
	}
}

func TestWritableProbeCreatesAndCleansUp(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	if err := Writable(root); err != nil {
		t.Fatalf("Writable on a fresh dir should succeed: %v", err)
	}
	// The probe file must not linger.
	if _, err := os.Stat(filepath.Join(root, ".mailferry-write-probe")); err == nil {
		t.Fatal("write probe file was left behind")
	}
}

func TestWritableProbeDetectsReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX read-only mode semantics do not apply on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem permission checks")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o500); err != nil { // r-x only: no writes
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(root, 0o700) })
	// Some CI/container filesystems (overlayfs) do not enforce POSIX write
	// bits for the owner; detect that and skip rather than falsely fail —
	// the probe logic itself is exercised by TestWritableProbeCreatesAndCleansUp.
	if f, err := os.OpenFile(filepath.Join(root, ".enforce-probe"), os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
		os.Remove(filepath.Join(root, ".enforce-probe"))
		t.Skip("filesystem does not enforce read-only permissions for the owner")
	}
	// The probe targets a subdir of the read-only root, which cannot be
	// created — Writable must surface an error rather than silently pass.
	if err := Writable(filepath.Join(root, "sub")); err == nil {
		t.Fatal("Writable must fail on a read-only location")
	}
}
