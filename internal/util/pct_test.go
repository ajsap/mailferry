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

package util

import "testing"

func TestPctNeverLies(t *testing.T) {
	cases := []struct {
		done, total int64
		want        string
	}{
		{0, 0, "-"},
		{5, 0, "-"},
		{12, 12, "100%"}, // exact completion only
		{26089, 26089, "100%"},
		{26081, 26089, "99.9%"}, // the reported defect: must NOT say 100%
		{63074, 63087, "99.9%"},
		{999999, 1000000, "99.9%"},
		{991, 1000, "99.1%"},
		{9, 12, "75%"},
		{1, 3, "33%"},
		{1, 1000000, "0.1%"}, // progress never rounds down to 0%
		{0, 10, "0%"},
	}
	for _, c := range cases {
		if got := Pct(c.done, c.total); got != c.want {
			t.Errorf("Pct(%d,%d) = %q, want %q", c.done, c.total, got, c.want)
		}
	}
}
