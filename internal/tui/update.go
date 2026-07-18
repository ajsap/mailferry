// MailFerry — IMAP Migration & Sync
// A High-Performance Native IMAP Migration Engine
//
// Copyright (C) 2026 Andy Saputra
// Author: Andy Saputra <andy@saputra.org>
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This file is part of MailFerry (https://github.com/ajsap/mailferry).
// Licensed under the GNU Affero General Public License v3.0 or later;
// see the LICENSE file for details.

package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		if !m.frozen {
			m.snap = m.stats.Snapshot()
			m.sampleRates()
		}
		m.samplePerf()
		return m, m.tickCmd()

	case busMsg:
		return m, m.waitBus()

	case doneMsg:
		if m.shuttingDown {
			// walk the remaining shutdown phases to completion so the dialog
			// shows every task finishing (engine has already unwound)
			return m, m.phaseCmd()
		}
		m.quitting = true
		m.stopSysmon()
		return m, tea.Quit

	case phaseMsg:
		m.shutPhase++
		if m.shutPhase < len(shutdownPhases) {
			return m, m.phaseCmd()
		}
		if !m.shutHold {
			m.shutHold = true // hold the completed dialog briefly
			return m, tea.Tick(700*time.Millisecond, func(time.Time) tea.Msg { return phaseMsg{} })
		}
		m.quitting = true
		m.stopSysmon()
		return m, tea.Quit

	case tea.MouseMsg:
		if m.popup == nil && m.active == vLogs {
			switch msg.Button {
			case tea.MouseButtonWheelUp:
				m.logFollow = false
				m.logTop -= 3
				if m.logTop < 0 {
					m.logTop = 0
				}
			case tea.MouseButtonWheelDown:
				m.logTop += 3
			}
		}
		return m, nil

	case tea.KeyMsg:
		return m.onKey(msg)
	}
	return m, nil
}

func (m *Model) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Ctrl+C: graceful shutdown dialog; a second Ctrl+C forces exit.
	if key == "ctrl+c" {
		if m.shuttingDown {
			m.forced = true
			if m.hardKC != nil {
				m.hardKC()
			}
			m.quitting = true
			return m, tea.Quit
		}
		m.shuttingDown = true
		m.shutdownAt = timeNow()
		m.shutPhase = 0
		if m.cancel != nil {
			m.cancel()
		}
		return m, nil
	}
	if m.shuttingDown {
		return m, nil // dialog owns the screen; ignore other keys
	}

	// An open detail popup captures input: Esc / Enter / q close it.
	if m.popup != nil {
		if key == "esc" || key == "enter" || key == "q" {
			m.popup = nil
		}
		return m, nil
	}

	// Search capture mode (/): type to filter, Enter keeps, Esc clears.
	if m.searching {
		switch key {
		case "enter":
			m.searching = false
		case "esc":
			m.searching = false
			m.search = ""
			m.searchView = -1
		case "backspace":
			if len(m.search) > 0 {
				m.search = m.search[:len(m.search)-1]
			}
		default:
			if len(msg.Runes) == 1 && msg.Runes[0] >= ' ' {
				m.search += string(msg.Runes)
			}
		}
		return m, nil
	}

	// vim-style navigation everywhere
	if key == "k" {
		key = "up"
	} else if key == "j" {
		key = "down"
	}

	// F1–F10 and their digit aliases (1–9,0) — always available because
	// some terminals/SSH clients intercept the function keys.
	if v, ok := viewKey(key); ok {
		m.active = v
		return m, nil
	}

	// global keys (parity with the classic TUI)
	switch key {
	case "tab":
		m.active = (m.active + 1) % numViews
		return m, nil
	case "shift+tab":
		m.active = (m.active + numViews - 1) % numViews
		return m, nil
	case "esc", "q":
		if m.search != "" {
			m.search = ""
			m.searchView = -1
		} else {
			m.active = vDashboard
		}
		return m, nil
	case "?":
		m.active = vHelp
		return m, nil
	case " ":
		m.frozen = !m.frozen
		return m, nil
	case "ctrl+l":
		return m, tea.ClearScreen
	case "p":
		m.bus.TogglePause()
		return m, nil
	case "/":
		if searchable[m.active] {
			m.searching = true
			m.search = ""
			m.searchView = m.active
		}
		return m, nil
	case "R":
		if n := m.bus.RequeueFailed("", true); n > 0 {
			m.flash = fmt.Sprintf("%d mailbox(es) re-queued", n)
		} else {
			m.flash = "nothing to re-queue"
		}
		return m, nil
	case "u":
		added, _ := m.bus.ReloadCSV()
		m.flash = fmt.Sprintf("CSV reload: %d new mailbox(es)", added)
		return m, nil
	}

	// per-view navigation & actions
	switch m.active {
	case vMailboxes:
		rows := m.mailboxRows()
		switch key {
		case "enter":
			if len(rows) > 0 && m.mailSel < len(rows) {
				m.openMailboxPopup(rows[m.mailSel])
			}
		case "r":
			if len(rows) > 0 && m.mailSel < len(rows) {
				if n := m.bus.RequeueFailed(rows[m.mailSel].Label, false); n > 0 {
					m.flash = rows[m.mailSel].Label + " re-queued"
				} else {
					m.flash = "not in a retryable state (FAILED/PARTIAL/STALE only)"
				}
			}
		case "s":
			m.mailSort = (m.mailSort + 1) % 3
		case "f":
			m.mailFilter = (m.mailFilter + 1) % 3
		default:
			m.mailSel = clampNav(key, m.mailSel, len(rows))
		}
	case vErrors:
		rows := m.errorRows()
		if key == "enter" {
			if len(rows) > 0 && m.errSel < len(rows) {
				m.openErrorPopup(rows[m.errSel])
			}
		} else {
			m.errSel = clampNav(key, m.errSel, len(rows))
		}
	case vHistory:
		hist := m.historyRows()
		switch key {
		case "enter":
			if len(hist) > 0 {
				if m.histFollow {
					m.histSel = len(hist) - 1
				}
				if m.histSel < len(hist) {
					m.openHistoryPopup(hist[m.histSel])
				}
			}
		case "up", "pgup", "home", "down", "pgdown":
			m.histFollow = false
			m.histSel = clampNav(key, m.histSel, len(hist))
		case "end", "f", "F":
			m.histFollow = true
		}
	case vLogs:
		n := len(m.filteredLogs())
		vh := m.logViewHeight()
		switch key {
		case "f", "F", "end":
			m.logFollow = true
		case "up":
			m.logFollow = false
			m.logTop--
		case "down":
			m.logFollow = false
			m.logTop++
		case "pgup":
			m.logFollow = false
			m.logTop -= vh
		case "pgdown":
			m.logFollow = false
			m.logTop += vh
		case "home":
			m.logFollow = false
			m.logTop = 0
		}
		if m.logTop < 0 {
			m.logTop = 0
		}
		if max := n - vh; m.logTop > max && max >= 0 {
			m.logTop = max
		}
	case vHelp:
		m.helpTop = clampNav(key, m.helpTop, 200)
	}
	return m, nil
}

// searchable marks the views with / search (parity with the classic TUI).
var searchable = map[int]bool{
	vWorkers: true, vMailboxes: true, vHistory: true, vErrors: true, vLogs: true,
}

// viewSearch returns the active filter string for a view ("" = no filter).
func (m *Model) viewSearch(view int) string {
	if m.searchView == view && m.search != "" {
		return m.search
	}
	return ""
}

// viewKey maps F-keys and digit aliases to a view index.
func viewKey(key string) (int, bool) {
	switch key {
	case "f1", "1":
		return vDashboard, true
	case "f2", "2":
		return vWorkers, true
	case "f3", "3":
		return vMailboxes, true
	case "f4", "4":
		return vQueue, true
	case "f5", "5":
		return vHistory, true
	case "f6", "6":
		return vErrors, true
	case "f7", "7":
		return vPerformance, true
	case "f8", "8":
		return vLogs, true
	case "f9", "9":
		return vSettings, true
	case "f10", "0":
		return vHelp, true
	}
	return 0, false
}

func clampNav(key string, sel, n int) int {
	switch key {
	case "up":
		sel--
	case "down":
		sel++
	case "pgup":
		sel -= 10
	case "pgdown":
		sel += 10
	case "home":
		sel = 0
	case "end":
		sel = n - 1
	}
	if sel < 0 {
		sel = 0
	}
	if sel >= n {
		sel = n - 1
	}
	if sel < 0 {
		sel = 0
	}
	return sel
}
