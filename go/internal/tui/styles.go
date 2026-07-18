// MailFerry - IMAP Migration & Sync
// A High-Performance Native IMAP Migration Engine
//
// Copyright (C) 2026 Andy Saputra <andy@saputra.org>
//
// https://saputra.org
// https://github.com/ajsap/mailferry
//
// Licensed under the GNU Affero General Public License v3.0 (AGPL-3.0).
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at
// your option) any later version.
//
// Contributions welcome: submit issues, feature requests and pull requests
// at https://github.com/ajsap/mailferry

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
