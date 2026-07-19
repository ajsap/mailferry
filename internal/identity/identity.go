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

// Package identity is the single source of truth for MailFerry's version
// and branding. Every banner, report, header and artefact derives from
// these constants — never hard-code them elsewhere.
package identity

import "fmt"

const (
	Version      = "2.0.3"
	Product      = "MailFerry"
	Title        = "MailFerry – IMAP Migration & Sync"
	Slogan       = "High-Performance Native IMAP Migration Engine"
	Author       = "Andy Saputra"
	AuthorEmail  = "andy@saputra.org"
	Copyright    = "Copyright (C) 2026 Andy Saputra"
	Repository   = "https://github.com/ajsap/mailferry"
	ProjectURL   = "https://saputra.org"
	SupportURL   = "https://github.com/ajsap/mailferry/issues"
	DocsURL      = "https://github.com/ajsap/mailferry/tree/main/docs"
	LicenseName  = "GNU Affero General Public License v3.0"
	LicenseShort = "GNU AGPL v3.0"
)

// BannerLine is the one-line product banner shown across the UI.
func BannerLine() string {
	return fmt.Sprintf("%s v%s — IMAP Migration & Sync", Product, Version)
}

// VersionText is the --version output.
func VersionText() string {
	return BannerLine() + "\n" + Slogan + "\n"
}

// AboutLines feeds --about and the Help/About view.
func AboutLines() []string {
	return []string{
		"About " + Product,
		Slogan,
		"",
		fmt.Sprintf("%-14s v%s", "Version", Version),
		fmt.Sprintf("%-14s %s <%s>", "Author", Author, AuthorEmail),
		fmt.Sprintf("%-14s %s", "Repository", Repository),
		fmt.Sprintf("%-14s %s", "Documentation", DocsURL),
		fmt.Sprintf("%-14s %s", "Issue Tracker", SupportURL),
		fmt.Sprintf("%-14s %s", "Community", ProjectURL),
		fmt.Sprintf("%-14s %s", "Licence", LicenseShort),
		"",
		Copyright,
	}
}

func AboutText() string {
	out := ""
	for _, l := range AboutLines() {
		out += l + "\n"
	}
	return out
}
