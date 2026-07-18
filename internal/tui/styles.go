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

// Package tui: the interactive MailFerry dashboard, rebuilt natively on
// Bubble Tea + Lip Gloss. It consumes engine state and events through the
// engine.Bus and a stats snapshot — it never drives migration directly, so
// a resize, lost terminal or SSH disconnect can never corrupt state.
package tui

import "github.com/charmbracelet/lipgloss"

// Lip Gloss adapts to the terminal's colour depth automatically, so these
// degrade gracefully on limited-colour terminals.
var (
	cReset  = lipgloss.NewStyle()
	cBold   = lipgloss.NewStyle().Bold(true)
	cDim    = lipgloss.NewStyle().Faint(true)
	cGreen  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	cYellow = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	cRed    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	cCyan   = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	cBanner = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4"))
)

func statusStyle(status string) lipgloss.Style {
	switch status {
	case "SUCCESS":
		return cGreen
	case "RUNNING", "RETRYING", "PARTIAL", "WARNINGS", "CANCELLED":
		return cYellow
	case "FAILED", "STALE":
		return cRed
	case "REMOTE", "SKIPPED":
		return cCyan
	default:
		return cDim
	}
}

func sevStyle(sev string) lipgloss.Style {
	switch sev {
	case "ERROR":
		return cRed
	case "WARN":
		return cYellow
	default:
		return cReset
	}
}

func histStyle(status string) lipgloss.Style {
	switch status {
	case "OK":
		return cGreen
	case "WARN":
		return cYellow
	case "FAIL":
		return cRed
	default:
		return cReset
	}
}
