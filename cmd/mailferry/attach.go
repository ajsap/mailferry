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

// `mailferry attach` — a READ-ONLY live monitor over the shared State
// Database. It opens the database for reading only, renders periodic
// snapshots (1 Hz) plus a tail of the session log, and takes NO leases,
// registers NO worker and writes NOTHING. Detaching never affects a running
// migration. On a non-TTY it degrades gracefully to a one-shot status
// snapshot and exits 0.

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ajsap/mailferry/v2/internal/identity"
	"github.com/ajsap/mailferry/v2/internal/paths"
	"github.com/ajsap/mailferry/v2/internal/progress"
	"github.com/ajsap/mailferry/v2/internal/state"
	"github.com/ajsap/mailferry/v2/internal/termstate"
	"github.com/ajsap/mailferry/v2/internal/tui"
)

func cmdAttach(rest []string) int {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	dbPath := fs.String("db", "", "State database path (default: the native per-user mailferry.db)")
	logsDir := fs.String("logs-dir", "", "Directory holding session.log (default: the native per-OS location)")
	workerTO := fs.Float64("worker-timeout", 60, "Offline threshold (s)")
	cfgPath := fs.String("config", "", "Path to mailferry.toml (already applied at startup)")
	fs.Parse(reorderArgs(fs, rest))
	_ = cfgPath // consumed by bootstrapConfig before dispatch

	// Optional <run-id> argument filters the displayed title only.
	filter := ""
	if fs.NArg() > 0 {
		filter = fs.Arg(0)
	}

	resolved, ok := requireExistingDB(*dbPath)
	if !ok {
		return 1
	}
	// Strictly read-only (SQLite mode=ro): no DDL, no meta insert, no lease —
	// attach writes NOTHING and, thanks to WAL, never competes with live
	// workers. Detaching can therefore never affect a running migration.
	db, err := state.OpenReadOnly(resolved)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	defer db.Close()

	// Session-log location: --logs-dir > TOML (bootCfg) > native/portable
	// default — the same resolution `mailferry status` would use for the logs
	// dir. Under --portable the portable root wins over a TOML directory.
	logDir := *logsDir
	if logDir == "" && bootCfg != nil && !paths.PortableActive() {
		logDir = bootCfg.LogsDir
	}
	if logDir == "" {
		logDir = paths.Default().LogsDir
	}
	logPath := filepath.Join(logDir, "session.log")

	poller := tui.NewAttachPoller(db, resolved, logPath, filter, *workerTO, 12)

	// Non-TTY invocation: print a one-shot snapshot (equivalent to
	// `mailferry status`) and exit 0 — documented graceful degradation.
	if !(progress.IsTTY && termInteractive()) {
		fmt.Print(tui.RenderAttach(poller.Poll()))
		return 0
	}

	fmt.Println(identity.BannerLine())
	model := tui.NewAttachModel(poller)
	restoreTerm := captureTerminal()
	defer termstate.Snapshot("post-tui-attach") // runs after the restore below
	defer restoreTerm()                         // same guarantee as the main TUI
	termstate.Snapshot("pre-tui-attach")
	p := tea.NewProgram(model, tea.WithAltScreen())
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	defer func() { signal.Stop(hupCh); close(hupCh) }()
	go func() { // hangup: quit cleanly, never die in raw mode
		if _, ok := <-hupCh; ok {
			p.Send(tea.QuitMsg{})
		}
	}()
	if _, err := p.Run(); err != nil {
		// The monitor is a pure reader; if the TUI cannot start, fall back to
		// a one-shot snapshot rather than failing.
		fmt.Fprintln(os.Stderr, "note: monitor UI unavailable (", err, ") — one-shot snapshot:")
		fmt.Print(tui.RenderAttach(poller.Poll()))
	}
	return 0
}
