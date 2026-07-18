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

package main

// --portable support. The flag is global: it is extracted from argv during
// early bootstrap (alongside --config) so EVERY command sees it. When active,
// the portable root is the executable's own directory and all four canonical
// locations live inside it. Precedence is preserved:
//
//	explicit CLI (--config/--db/--logs-dir) > portable > TOML > native
//
// The portable root replaces the native layer via paths.SetPortable; explicit
// flags are still applied on top by each command, and portable beats a TOML
// database.path because portableDBOverride is applied after the TOML load.

import (
	"fmt"
	"os"

	"github.com/ajsap/mailferry/v2/internal/paths"
	"github.com/ajsap/mailferry/v2/internal/progress"
)

// portableActive mirrors paths.PortableActive() for local convenience.
var portableActive bool

// extractPortable removes a global --portable flag from argv and, when
// present, turns on portable mode rooted at the executable's directory. It
// returns the filtered argv. Resolution only — creates nothing.
func extractPortable(argv []string) []string {
	found := false
	out := argv[:0]
	for _, a := range argv {
		if a == "--portable" || a == "--portable=true" {
			found = true
			continue
		}
		if a == "--portable=false" {
			continue
		}
		out = append(out, a)
	}
	if !found {
		return out
	}
	root, err := paths.ExecutableDir()
	if err != nil {
		// Fall back to the resolved dir but warn — portable must still name a
		// concrete root so the paths are deterministic.
		fmt.Fprintln(os.Stderr, progress.C(
			"note: could not resolve the executable directory for --portable ("+
				err.Error()+"); using "+root, "yellow"))
	}
	paths.SetPortable(root)
	portableActive = true
	return out
}

// portableWritableGuard verifies the portable root is writable before an
// operational command creates files there, surfacing a clear, actionable
// error for a read-only location (e.g. a mounted read-only image). It is a
// no-op when portable mode is off. dirs are the directories the command will
// write into (e.g. the DB parent and the logs dir).
func portableWritableGuard(dirs ...string) error {
	if !paths.PortableActive() {
		return nil
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if err := paths.Writable(d); err != nil {
			return fmt.Errorf("portable location is not writable: %s — move the "+
				"executable to a writable location or use explicit --db/--config paths (%w)",
				d, err)
		}
	}
	return nil
}
