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

import (
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ajsap/mailferry/v2/internal/identity"
	"github.com/ajsap/mailferry/v2/internal/termstate"
)

// cmdTermDiag is the minimal terminal reproduction: the same banner-style
// "\n" line output as a real run, a two-second Bubble Tea alternate-screen
// stage through the exact same capture/restore path as the migration TUI,
// then more line output. No IMAP, no network, no State Database, no
// credentials — it isolates the terminal path completely. With
// MAILFERRY_TERM_DIAG=FILE set it records termios flags at every stage.
func cmdTermDiag(_ []string) int {
	termstate.Snapshot("termdiag-start")
	fmt.Println(identity.BannerLine())
	fmt.Println(identity.Slogan)
	fmt.Println()
	fmt.Println("MailFerry test line 1")
	fmt.Println("MailFerry test line 2")
	fmt.Println("MailFerry test line 3")
	termstate.Snapshot("termdiag-pre-tui")
	if termInteractive() {
		restoreTerm := captureTerminal()
		p := tea.NewProgram(diagModel{}, tea.WithAltScreen())
		_, err := p.Run()
		restoreTerm()
		termstate.Snapshot("termdiag-post-tui")
		if err != nil {
			fmt.Fprintln(os.Stderr, "note: TUI stage unavailable (", err, ")")
		}
	} else {
		fmt.Println("(not an interactive terminal — TUI stage skipped)")
	}
	fmt.Println("MailFerry test line 4")
	fmt.Println("MailFerry test line 5")
	fmt.Println("MailFerry test line 6")
	termstate.Snapshot("termdiag-end")
	fmt.Println()
	if diag := os.Getenv(termstate.DiagEnv); diag != "" {
		fmt.Println("Diagnostic log written to:", diag)
	} else {
		fmt.Printf("Hint: %s=FILE records termios flags at every stage "+
			"(flags only — no mailbox data, no credentials).\n", termstate.DiagEnv)
	}
	fmt.Println("Healthy result: test lines 1–6 all start at the left margin," +
		" and this shell behaves normally now.")
	return 0
}

// diagModel is the minimal alternate-screen program: it exists only to
// exercise the raw-mode enter/exit path and quits by itself.
type diagModel struct{}

type diagDone struct{}

func (diagModel) Init() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return diagDone{} })
}

func (m diagModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case diagDone, tea.KeyMsg:
		return m, tea.Quit
	}
	return m, nil
}

func (diagModel) View() string {
	return "\n  MailFerry terminal diagnostic — alternate-screen stage\n" +
		"  (restores automatically in 2 seconds; any key quits)\n"
}
