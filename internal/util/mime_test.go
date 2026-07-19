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

func TestDecodeMIME(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain subject", "plain subject"},
		{"=?utf-8?B?TlrigJlzIExlYWRpbmc=?=", "NZ’s Leading"},
		{"=?utf-8?Q?RE:_Request_=E2=80=93_F&P?=", "RE: Request – F&P"},
		{"=?iso-8859-1?Q?caf=E9?=", "café"},
		// truncated encoded word (legacy display cut): fall back verbatim
		{"=?utf-8?B?TlrigJlzIExlYWRpbmcgU21hcnQgRmlsbSBTcG", "=?utf-8?B?TlrigJlzIExlYWRpbmcgU21hcnQgRmlsbSBTcG"},
		{"", ""},
	}
	for _, c := range cases {
		if got := DecodeMIME(c.in); got != c.want {
			t.Errorf("DecodeMIME(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEllipsize(t *testing.T) {
	if got := Ellipsize("Request for Quote – Appliances and more", 22); got != "Request for Quote – A…" {
		t.Errorf("got %q", got)
	}
	if got := Ellipsize("short", 22); got != "short" {
		t.Errorf("got %q", got)
	}
	if got := Ellipsize("多字节字符串安全截断测试", 6); got != "多字节字符…" {
		t.Errorf("got %q", got)
	}
}
