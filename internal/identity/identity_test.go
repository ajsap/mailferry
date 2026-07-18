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

// Canonical-identity enforcement: the product identity has exactly one
// authoritative form. A drifted slogan or banner fails the suite.
package identity

import (
	"strings"
	"testing"
)

func TestCanonicalIdentity(t *testing.T) {
	if Slogan != "High-Performance Native IMAP Migration Engine" {
		t.Fatalf("slogan drifted: %q", Slogan)
	}
	if strings.HasPrefix(Slogan, "A ") {
		t.Fatal(`the canonical slogan carries no leading "A"`)
	}
	if Product != "MailFerry" {
		t.Fatalf("product drifted: %q", Product)
	}
	want := "MailFerry v" + Version + " — IMAP Migration & Sync"
	if BannerLine() != want {
		t.Fatalf("banner drifted: %q (want %q)", BannerLine(), want)
	}
	if Author != "Andy Saputra" || AuthorEmail != "andy@saputra.org" {
		t.Fatal("author identity drifted")
	}
	if Repository != "https://github.com/ajsap/mailferry" ||
		SupportURL != "https://github.com/ajsap/mailferry/issues" {
		t.Fatal("repository/support URLs drifted")
	}
	about := AboutText()
	for _, want := range []string{Slogan, "v" + Version, "GNU AGPL v3.0",
		"Andy Saputra <andy@saputra.org>"} {
		if !strings.Contains(about, want) {
			t.Fatalf("about text missing %q", want)
		}
	}
}
