// MailFerry - IMAP Migration & Sync
// High-Performance Native IMAP Migration Engine
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

// Package util: formatting, UID interval maths and message fingerprints.
//
// Fingerprints are byte-compatible with the Python v1.x engine so a
// migration.db written by either implementation adopts identically:
//
//	m:  Message-ID with every [<>\s] character removed
//	h:  sha256(Date \x00 From \x00 To \x00 Subject \x00 size)[:32]
//
// with header values in Python email-library form (first line stripped of
// leading space/tab; folded continuation lines appended as "\r\n" + line).
package util

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"time"
)

// ------------------------------------------------------------ formatting --

func FmtBytes(n float64) string {
	neg := ""
	if n < 0 {
		neg, n = "-", -n
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%s%.0f B", neg, n)
	}
	return fmt.Sprintf("%s%.1f %s", neg, n, units[i])
}

// FmtDHMS renders "N day HH:MM:SS" exactly like the Python dashboard.
func FmtDHMS(secs float64) string {
	if secs < 0 {
		secs = 0
	}
	s := int64(secs)
	d := s / 86400
	h := (s % 86400) / 3600
	m := (s % 3600) / 60
	return fmt.Sprintf("%d day %02d:%02d:%02d", d, h, m, s%60)
}

func Pct(done, total int64) string {
	if total <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", float64(done)*100/float64(total))
}

func SafeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' || r == '@' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func Truncate(s string, w int) string {
	if len(s) <= w {
		return s
	}
	if w <= 1 {
		return s[:w]
	}
	return s[:w-1] + "…"
}

// BackoffDelay: jittered exponential backoff (base * 2^(attempt-1), capped).
func BackoffDelay(base float64, attempt int, cap float64) float64 {
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= cap {
			d = cap
			break
		}
	}
	j := d * 0.15
	return d - j + rand.Float64()*2*j
}

// ------------------------------------------------------------- intervals --

type Interval struct{ Lo, Hi uint32 }

// ToIntervals turns a sorted-or-not UID slice into merged intervals.
func ToIntervals(uids []uint32) []Interval {
	if len(uids) == 0 {
		return nil
	}
	sorted := append([]uint32(nil), uids...)
	for i := 1; i < len(sorted); i++ { // insertion sort: inputs are near-sorted
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	out := []Interval{{sorted[0], sorted[0]}}
	for _, u := range sorted[1:] {
		if u == out[len(out)-1].Hi || u == out[len(out)-1].Hi+1 {
			out[len(out)-1].Hi = u
		} else {
			out = append(out, Interval{u, u})
		}
	}
	return out
}

func IntervalsCount(iv []Interval) int64 {
	var n int64
	for _, i := range iv {
		n += int64(i.Hi) - int64(i.Lo) + 1
	}
	return n
}

// IntervalsDiff returns a minus b.
func IntervalsDiff(a, b []Interval) []Interval {
	var out []Interval
	bi := 0
	for _, ia := range a {
		lo := ia.Lo
		for bi < len(b) && b[bi].Hi < lo {
			bi++
		}
		j := bi
		for lo <= ia.Hi {
			if j >= len(b) || b[j].Lo > ia.Hi {
				out = append(out, Interval{lo, ia.Hi})
				break
			}
			if b[j].Lo > lo {
				out = append(out, Interval{lo, b[j].Lo - 1})
			}
			if b[j].Hi >= ia.Hi {
				break
			}
			lo = b[j].Hi + 1
			j++
		}
	}
	return out
}

// SetStrings renders intervals as IMAP set strings, chunked so a single
// command line stays comfortably small.
func SetStrings(iv []Interval) []string {
	const maxRanges = 200
	var out []string
	var cur []string
	for _, i := range iv {
		if i.Lo == i.Hi {
			cur = append(cur, fmt.Sprintf("%d", i.Lo))
		} else {
			cur = append(cur, fmt.Sprintf("%d:%d", i.Lo, i.Hi))
		}
		if len(cur) >= maxRanges {
			out = append(out, strings.Join(cur, ","))
			cur = nil
		}
	}
	if len(cur) > 0 {
		out = append(out, strings.Join(cur, ","))
	}
	return out
}

// ----------------------------------------------------------- fingerprint --

var msgidClean = regexp.MustCompile(`[<>\s]`)

// headerValue extracts a header value in Python email-library form.
func headerValue(header []byte, name string) (string, bool) {
	want := strings.ToLower(name) + ":"
	lines := bytes.Split(header, []byte("\r\n"))
	if len(lines) == 1 {
		lines = bytes.Split(header, []byte("\n"))
	}
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		low := strings.ToLower(string(l))
		if !strings.HasPrefix(low, want) {
			continue
		}
		val := string(l[len(want):])
		val = strings.TrimPrefix(val, " ")
		if strings.HasPrefix(val, "\t") {
			val = val[1:]
		}
		for i+1 < len(lines) && len(lines[i+1]) > 0 &&
			(lines[i+1][0] == ' ' || lines[i+1][0] == '\t') {
			i++
			val += "\r\n" + string(lines[i])
		}
		return val, true
	}
	return "", false
}

// FingerprintFromHeaders is byte-compatible with the Python engine.
func FingerprintFromHeaders(header []byte, size int64) string {
	if len(header) > 0 {
		if mid, ok := headerValue(header, "Message-ID"); !ok || strings.TrimSpace(mid) == "" {
			if mid2, ok2 := headerValue(header, "Message-Id"); ok2 {
				mid, ok = mid2, ok2
				if strings.TrimSpace(mid) != "" {
					return "m:" + msgidClean.ReplaceAllString(strings.TrimSpace(mid), "")
				}
			}
		} else {
			return "m:" + msgidClean.ReplaceAllString(strings.TrimSpace(mid), "")
		}
		date, _ := headerValue(header, "Date")
		from, _ := headerValue(header, "From")
		to, _ := headerValue(header, "To")
		subj, _ := headerValue(header, "Subject")
		basis := strings.Join([]string{date, from, to, subj, fmt.Sprintf("%d", size)}, "\x00")
		sum := sha256.Sum256([]byte(basis))
		return "h:" + hex.EncodeToString(sum[:])[:32]
	}
	basis := "\x00raw\x00" + fmt.Sprintf("%d", size)
	sum := sha256.Sum256([]byte(basis))
	return "h:" + hex.EncodeToString(sum[:])[:32]
}

// HeaderSniffer accumulates the first streamed chunks of a message until
// the blank line so a fingerprint costs zero extra round trips.
type HeaderSniffer struct {
	buf  []byte
	done bool
}

const snifferLimit = 65536

func (h *HeaderSniffer) Feed(chunk []byte) {
	if h.done {
		return
	}
	room := snifferLimit - len(h.buf)
	if room > len(chunk) {
		room = len(chunk)
	}
	h.buf = append(h.buf, chunk[:room]...)
	if bytes.Contains(h.buf, []byte("\r\n\r\n")) || bytes.Contains(h.buf, []byte("\n\n")) ||
		len(h.buf) >= snifferLimit {
		h.done = true
	}
}

func (h *HeaderSniffer) Fingerprint(size int64) string {
	buf := h.buf
	for _, sep := range [][]byte{[]byte("\r\n\r\n"), []byte("\n\n")} {
		if i := bytes.Index(buf, sep); i >= 0 {
			buf = buf[:i]
			break
		}
	}
	return FingerprintFromHeaders(buf, size)
}

func NowISO() string {
	return time.Now().Format("2006-01-02T15:04:05")
}

// Sha256Hex32: first 32 hex chars of sha256 (test support for the
// Python-compatible h: fingerprint).
func Sha256Hex32(basis string) string {
	sum := sha256.Sum256([]byte(basis))
	return hex.EncodeToString(sum[:])[:32]
}
