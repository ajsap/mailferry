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

import (
	"mime"
	"strings"
	"unicode/utf8"
)

// DecodeMIME renders an RFC 2047 encoded-word header (=?utf-8?B?…?= /
// =?…?Q?…?=) human-readable for interactive display. Decoding is strictly
// best-effort: any failure — unknown charset, malformed or truncated
// encoded word (e.g. a value cut mid-token by an older storage limit) —
// falls back to the original string unchanged. Never panics, never
// returns an empty string for non-empty input.
func DecodeMIME(s string) string {
	if !strings.Contains(s, "=?") {
		return s
	}
	dec := mime.WordDecoder{}
	out, err := dec.DecodeHeader(s)
	if err != nil || strings.TrimSpace(out) == "" {
		return s
	}
	return out
}

// Ellipsize shortens s to at most n runes, appending a single ellipsis
// when something was cut. Rune-safe: never splits a multi-byte character.
func Ellipsize(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	if n == 1 {
		return "…"
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}

// DecodeEllipsize is the standard interactive rendering of a stored
// header: decode RFC 2047 words, then truncate cleanly with an ellipsis —
// an encoded word is never cut in half at display time.
func DecodeEllipsize(s string, n int) string {
	return Ellipsize(DecodeMIME(s), n)
}
