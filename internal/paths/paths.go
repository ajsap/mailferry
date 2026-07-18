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

// Package paths centralises every default filesystem location MailFerry
// uses, per operating system. The cardinal rule: RESOLVING a path never
// creates anything. Directories are created lazily, by the specific
// operations that need them, via Ensure* — informational commands
// (--help, version, about, changelog, roadmap, config paths) therefore
// have zero filesystem side effects.
//
// Native defaults:
//
//	macOS    ~/Library/Application Support/MailFerry/{mailferry.toml,mailferry.db}
//	         ~/Library/Logs/MailFerry/   ~/Library/Caches/MailFerry/
//	Linux    $XDG_CONFIG_HOME/mailferry/mailferry.toml   (~/.config fallback)
//	         $XDG_STATE_HOME/mailferry/{mailferry.db,logs/} (~/.local/state fallback)
//	         $XDG_CACHE_HOME/mailferry/ (~/.cache fallback)
//	Windows  %APPDATA%\MailFerry\mailferry.toml
//	         %LOCALAPPDATA%\MailFerry\{mailferry.db,Logs\,Cache\}
//
// Precedence for the effective locations is decided by the caller:
// CLI flag > TOML configuration > these native defaults.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Paths holds the resolved default locations. Resolution only — nothing
// here exists on disk unless something else created it.
type Paths struct {
	ConfigFile string // mailferry.toml
	StateDB    string // mailferry.db
	LogsDir    string
	CacheDir   string

	// LegacyConfigFile / LegacyStateDB are pre-rc.2 development defaults
	// (detected, never written): ~/.config/mailferry/mailferry.toml on
	// non-Linux systems and ./migration.db in the working directory.
	LegacyConfigFile string
	LegacyStateDB    string
}

// join builds a path for the TARGET goos, so resolution is unit-testable
// for every OS from any OS (Windows paths use backslashes).
func join(goos string, parts ...string) string {
	sep := "/"
	if goos == "windows" {
		sep = `\`
	}
	return strings.Join(parts, sep)
}

// For resolves the default paths for goos using the supplied environment
// lookup and home directory. Pure: performs no filesystem access.
func For(goos string, getenv func(string) string, home string) Paths {
	var p Paths
	switch goos {
	case "darwin":
		appSup := join(goos, home, "Library", "Application Support", "MailFerry")
		p.ConfigFile = join(goos, appSup, "mailferry.toml")
		p.StateDB = join(goos, appSup, "mailferry.db")
		p.LogsDir = join(goos, home, "Library", "Logs", "MailFerry")
		p.CacheDir = join(goos, home, "Library", "Caches", "MailFerry")
		p.LegacyConfigFile = join(goos, home, ".config", "mailferry", "mailferry.toml")
	case "windows":
		appData := getenv("APPDATA")
		if appData == "" {
			appData = join(goos, home, "AppData", "Roaming")
		}
		localApp := getenv("LOCALAPPDATA")
		if localApp == "" {
			localApp = join(goos, home, "AppData", "Local")
		}
		p.ConfigFile = join(goos, appData, "MailFerry", "mailferry.toml")
		p.StateDB = join(goos, localApp, "MailFerry", "mailferry.db")
		p.LogsDir = join(goos, localApp, "MailFerry", "Logs")
		p.CacheDir = join(goos, localApp, "MailFerry", "Cache")
	default: // linux and other unixes: XDG Base Directory spec
		cfgHome := getenv("XDG_CONFIG_HOME")
		if cfgHome == "" {
			cfgHome = join(goos, home, ".config")
		}
		stateHome := getenv("XDG_STATE_HOME")
		if stateHome == "" {
			stateHome = join(goos, home, ".local", "state")
		}
		cacheHome := getenv("XDG_CACHE_HOME")
		if cacheHome == "" {
			cacheHome = join(goos, home, ".cache")
		}
		p.ConfigFile = join(goos, cfgHome, "mailferry", "mailferry.toml")
		p.StateDB = join(goos, stateHome, "mailferry", "mailferry.db")
		p.LogsDir = join(goos, stateHome, "mailferry", "logs")
		p.CacheDir = join(goos, cacheHome, "mailferry")
		// On Linux the XDG config path IS canonical; only the DB moved.
	}
	p.LegacyStateDB = "./migration.db"
	return p
}

// Default resolves the native defaults for the running system. The
// MAILFERRY_CONFIG_DIR override (used by tests and controlled
// deployments) relocates the configuration file only.
//
// When --portable is active (see SetPortable) Default returns the portable
// layout instead: everything lives beside the executable, so native and
// TOML locations are bypassed. Explicit --config/--db/--logs-dir flags still
// win — the caller applies those on top of what Default reports.
func Default() Paths {
	if portableRoot != "" {
		return PortableFor(portableRoot)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	p := For(runtime.GOOS, os.Getenv, home)
	if d := os.Getenv("MAILFERRY_CONFIG_DIR"); d != "" {
		p.ConfigFile = filepath.Join(d, "mailferry.toml")
	}
	return p
}

// portableRoot, once set, relocates every default location to the directory
// holding the running executable (a self-contained "run from a USB stick"
// layout). It is a process-global toggle set once during argv handling.
var portableRoot string

// PortableFor is the pure portable resolver: given a root directory, every
// canonical location lands directly inside it. The portable root is THE
// EXECUTABLE'S DIRECTORY (os.Executable with symlinks resolved) — resolved
// by ExecutableDir and documented there. Performs no filesystem access.
func PortableFor(root string) Paths {
	return Paths{
		ConfigFile:    filepath.Join(root, "mailferry.toml"),
		StateDB:       filepath.Join(root, "mailferry.db"),
		LogsDir:       filepath.Join(root, "logs"),
		CacheDir:      filepath.Join(root, "cache"),
		LegacyStateDB: "./migration.db",
	}
}

// SetPortable turns on portable mode rooted at dir. Passing "" disables it.
// After this call Default() reports the portable layout.
func SetPortable(dir string) { portableRoot = dir }

// PortableActive reports whether portable mode is on.
func PortableActive() bool { return portableRoot != "" }

// PortableRoot returns the active portable root ("" when inactive).
func PortableRoot() string { return portableRoot }

// ExecutableDir resolves the directory of the running executable with
// symlinks followed, so a symlinked launcher still roots portable mode at
// the real binary's folder. Falls back to the current directory only if the
// executable path cannot be determined.
func ExecutableDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return ".", err
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

// Writable verifies dir exists and accepts writes, returning a clear,
// actionable error for a read-only portable root. It creates dir (and
// parents) first — a fresh portable stick has no logs/ yet — then probes
// with a temp file it immediately removes.
func Writable(dir string) error {
	if err := EnsureDir(dir); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".mailferry-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	os.Remove(probe)
	return nil
}

// EnsureParent creates the parent directory of file with owner-only
// permissions. This is the ONLY place MailFerry creates application
// directories — called by operations that genuinely need the file.
func EnsureParent(file string) error {
	return EnsureDir(filepath.Dir(file))
}

// EnsureDir creates dir (and parents) with 0700. Windows ignores the
// mode; NTFS ACLs inherited from the profile directory apply instead.
func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

// Restrict tightens permissions on a sensitive file (config, State
// Database, logs). Best-effort: on platforms without POSIX permissions
// this is a no-op.
func Restrict(file string) {
	_ = os.Chmod(file, 0o600)
}

// RestrictDB tightens the State Database and its WAL/SHM side files.
func RestrictDB(file string) {
	for _, f := range []string{file, file + "-wal", file + "-shm"} {
		_ = os.Chmod(f, 0o600)
	}
}
