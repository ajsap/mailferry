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

package imapx

// Modified UTF-7 folder-name codec (RFC 3501 §5.1.3).

import (
	"encoding/base64"
	"strings"
	"unicode/utf16"
)

var b64 = base64.NewEncoding(
	"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,").WithPadding(base64.NoPadding)

// EncodeMUTF7 encodes a Unicode folder name to modified UTF-7 wire form.
func EncodeMUTF7(s string) string {
	var out strings.Builder
	var pend []rune
	flush := func() {
		if len(pend) == 0 {
			return
		}
		u16 := utf16.Encode(pend)
		raw := make([]byte, 0, len(u16)*2)
		for _, u := range u16 {
			raw = append(raw, byte(u>>8), byte(u))
		}
		out.WriteByte('&')
		out.WriteString(b64.EncodeToString(raw))
		out.WriteByte('-')
		pend = pend[:0]
	}
	for _, r := range s {
		if r >= 0x20 && r <= 0x7e {
			flush()
			if r == '&' {
				out.WriteString("&-")
			} else {
				out.WriteRune(r)
			}
		} else {
			pend = append(pend, r)
		}
	}
	flush()
	return out.String()
}

// DecodeMUTF7 decodes a modified UTF-7 wire name to Unicode; malformed
// input is returned unchanged (server names are operator data, not ours).
func DecodeMUTF7(s string) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '&' {
			out.WriteByte(c)
			i++
			continue
		}
		j := strings.IndexByte(s[i+1:], '-')
		if j < 0 {
			out.WriteString(s[i:])
			break
		}
		seg := s[i+1 : i+1+j]
		if seg == "" {
			out.WriteByte('&')
		} else {
			raw, err := b64.DecodeString(seg)
			if err != nil || len(raw)%2 != 0 {
				out.WriteString(s[i : i+2+j])
			} else {
				u16 := make([]uint16, len(raw)/2)
				for k := 0; k < len(u16); k++ {
					u16[k] = uint16(raw[2*k])<<8 | uint16(raw[2*k+1])
				}
				out.WriteString(string(utf16.Decode(u16)))
			}
		}
		i += 2 + j
	}
	return out.String()
}
