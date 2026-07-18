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

package config

// ISO 8601 date-range selection (--from / --to).
//
// A range narrows a migration to messages whose authoritative timestamp
// falls inside [From, To] inclusive. The authoritative timestamp is the IMAP
// INTERNALDATE (RFC 3501), NOT the Date: header — see the engine's selection
// code for why (headers are client-supplied and routinely wrong or absent).
//
// The user-facing bounds are resolved to fixed instants ONCE, at run
// creation, and then persisted, so that a later resume of the same run
// applies exactly the same window regardless of the clock or the machine's
// timezone at resume time.

import (
	"fmt"
	"strings"
	"time"
)

// DateRange is a resolved, fixed-instant selection window. The zero value is
// an inactive (unrestricted) range. From/To are stored in UTC; HasFrom/HasTo
// distinguish "unbounded" from "the epoch". TZ records the timezone the user
// supplied (or the resolved local zone) purely for display and the log.
type DateRange struct {
	Active  bool
	HasFrom bool
	HasTo   bool
	From    time.Time // inclusive lower bound (UTC); valid when HasFrom
	To      time.Time // inclusive upper bound (UTC); valid when HasTo
	TZ      string    // resolved timezone label for the display/log
	FromRaw string    // the exact --from string the user gave ("" if none)
	ToRaw   string    // the exact --to string the user gave ("" if none)
}

// canonical accepted layouts. The canonical form is YYYY-MM-DDTHH:MM:SS with
// an optional timezone offset or Z; a handful of common, unambiguous shorter
// forms are accepted for convenience. NO relative keywords are honoured —
// "now"/"present"/"today" are rejected with a clear message (a range must be
// a fixed instant so a resume is deterministic).
var isoLayouts = []struct {
	layout string
	hasTZ  bool
}{
	{"2006-01-02T15:04:05Z07:00", true}, // full + offset/Z
	{"2006-01-02T15:04:05", false},      // full, no tz -> local
	{"2006-01-02T15:04Z07:00", true},    // minute + offset/Z
	{"2006-01-02T15:04", false},         // minute, no tz -> local
	{"2006-01-02 15:04:05Z07:00", true}, // space separator + offset
	{"2006-01-02 15:04:05", false},      // space separator, no tz
	{"2006-01-02", false},               // date only -> local midnight
}

// relativeKeywords are explicitly rejected: a persisted range must be a fixed
// instant, so "now"/"present"/"today" (which drift on resume) are refused.
var relativeKeywords = map[string]bool{
	"now": true, "present": true, "today": true, "yesterday": true,
	"tomorrow": true, "epoch": true,
}

// parseISO resolves one bound string to a fixed instant. loc is the location
// used when the string carries no explicit offset (the system local zone at
// run creation). It also reports the resolved timezone label used.
func parseISO(field, s string, loc *time.Location) (time.Time, string, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return time.Time{}, "", fmt.Errorf("%s is empty", field)
	}
	if relativeKeywords[strings.ToLower(trimmed)] {
		return time.Time{}, "", fmt.Errorf("%s %q is not allowed — a date range must be an "+
			"absolute ISO 8601 instant (e.g. 2026-07-19T00:00:00 or 2026-07-19T00:00:00Z); "+
			"relative keywords like \"now\"/\"present\" are rejected so a resume is deterministic",
			field, trimmed)
	}
	for _, l := range isoLayouts {
		var (
			t   time.Time
			err error
		)
		if l.hasTZ {
			t, err = time.Parse(l.layout, trimmed)
		} else {
			t, err = time.ParseInLocation(l.layout, trimmed, loc)
		}
		if err != nil {
			continue
		}
		tz := t.Format("-07:00")
		if l.hasTZ && (strings.HasSuffix(trimmed, "Z") || strings.HasSuffix(trimmed, "z")) {
			tz = "UTC"
		}
		return t, tz, nil
	}
	return time.Time{}, "", fmt.Errorf("%s %q is not valid ISO 8601 — expected "+
		"YYYY-MM-DDTHH:MM:SS with an optional timezone offset or Z "+
		"(e.g. 2026-07-19T09:30:00, 2026-07-19T09:30:00+12:00, or 2026-07-19T09:30:00Z)",
		field, trimmed)
}

// ResolveRange validates the raw --from/--to strings and resolves them to a
// fixed-instant DateRange. Both bounds are INCLUSIVE. An omitted bound is
// unbounded on that side; both omitted yields an inactive range. A --from
// later than --to is rejected here, BEFORE any connection is attempted.
//
// loc is the location applied to timezone-less inputs; pass time.Local for
// normal operation (tests inject a fixed zone for determinism).
func ResolveRange(from, to string, loc *time.Location) (DateRange, error) {
	if loc == nil {
		loc = time.Local
	}
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" && to == "" {
		return DateRange{}, nil
	}
	dr := DateRange{Active: true, FromRaw: from, ToRaw: to}
	var tzLabels []string
	if from != "" {
		t, tz, err := parseISO("--from", from, loc)
		if err != nil {
			return DateRange{}, err
		}
		dr.HasFrom = true
		dr.From = t.UTC()
		tzLabels = append(tzLabels, tz)
	}
	if to != "" {
		t, tz, err := parseISO("--to", to, loc)
		if err != nil {
			return DateRange{}, err
		}
		dr.HasTo = true
		dr.To = t.UTC()
		tzLabels = append(tzLabels, tz)
	}
	if dr.HasFrom && dr.HasTo && dr.From.After(dr.To) {
		return DateRange{}, fmt.Errorf("--from (%s) is later than --to (%s) — "+
			"the range is empty; nothing would be migrated", from, to)
	}
	// A single, consistent timezone label is friendliest; if the two bounds
	// used different explicit offsets, show them both.
	dr.TZ = tzLabels[0]
	if len(tzLabels) == 2 && tzLabels[0] != tzLabels[1] {
		dr.TZ = tzLabels[0] + " / " + tzLabels[1]
	}
	return dr, nil
}

// Includes reports whether an instant falls inside the window. Both bounds
// are inclusive; an unbounded side never excludes. A zero instant (an
// INTERNALDATE that could not be parsed) is treated as OUT of any active
// window — correctness first: a message we cannot date must not be silently
// swept in or out on a guess; the caller decides how to surface it.
func (d DateRange) Includes(t time.Time) bool {
	if !d.Active {
		return true
	}
	if t.IsZero() {
		return false
	}
	u := t.UTC()
	if d.HasFrom && u.Before(d.From) {
		return false
	}
	if d.HasTo && u.After(d.To) {
		return false
	}
	return true
}

// internalDateLayouts are the RFC 3501 INTERNALDATE wire forms. The day may be
// space-padded (" 2-Jan-2026 …") or two-digit ("02-Jan-2026 …"); Go's "_2"
// day token accepts both. A trailing-zone form and a UTC "Z" form are both
// tolerated so a server that deviates slightly is still dated correctly.
var internalDateLayouts = []string{
	"_2-Jan-2006 15:04:05 -0700",
	"_2-Jan-2006 15:04:05 Z0700",
	"2006-01-02 15:04:05 -0700", // defensive: some servers echo an ISO-ish form
}

// ParseInternalDate parses a raw IMAP INTERNALDATE string to an instant. The
// bool reports success; on failure the zero time is returned (and the caller,
// via IncludesRaw, treats an undatable message as outside any active window).
func ParseInternalDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, l := range internalDateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// IncludesRaw is the engine-facing selection test: it parses a raw IMAP
// INTERNALDATE string and reports (inWindow, parsed). When the range is
// inactive every message is in-window and parsed is reported true (nothing was
// excluded, so datability is moot). When the range is active and the date does
// not parse, the message is OUT (inWindow=false, parsed=false) — the caller
// can log/skip it deliberately rather than guess it in or out.
func (d DateRange) IncludesRaw(internalDate string) (inWindow bool, parsed bool) {
	if !d.Active {
		return true, true
	}
	t, ok := ParseInternalDate(internalDate)
	if !ok {
		return false, false
	}
	return d.Includes(t), true
}

// Label renders the human-readable "from → to (tz)" line for the start
// banner, the TUI dashboard and the session log. "(any)" marks an open side.
func (d DateRange) Label() string {
	if !d.Active {
		return ""
	}
	fromS, toS := "(any)", "(any)"
	if d.HasFrom {
		fromS = d.From.Format("2006-01-02T15:04:05Z")
	}
	if d.HasTo {
		toS = d.To.Format("2006-01-02T15:04:05Z")
	}
	tz := d.TZ
	if tz == "" {
		tz = "UTC"
	}
	return fmt.Sprintf("%s → %s (input tz %s)", fromS, toS, tz)
}

// Store/Load helpers: the persisted representation is RFC 3339 UTC (or "" for
// an unbounded side), so a resume reconstructs the identical window.
func (d DateRange) StoredFrom() string {
	if d.HasFrom {
		return d.From.UTC().Format(time.RFC3339)
	}
	return ""
}

func (d DateRange) StoredTo() string {
	if d.HasTo {
		return d.To.UTC().Format(time.RFC3339)
	}
	return ""
}

// RangeFromStored reconstructs a DateRange from persisted UTC strings (the
// stored range wins on resume — see the engine). Any side that fails to parse
// is treated as unbounded, which is the safe direction for a resume.
func RangeFromStored(fromUTC, toUTC, tz string) DateRange {
	dr := DateRange{TZ: tz}
	if fromUTC != "" {
		if t, err := time.Parse(time.RFC3339, fromUTC); err == nil {
			dr.Active, dr.HasFrom, dr.From = true, true, t.UTC()
		}
	}
	if toUTC != "" {
		if t, err := time.Parse(time.RFC3339, toUTC); err == nil {
			dr.Active, dr.HasTo, dr.To = true, true, t.UTC()
		}
	}
	return dr
}
