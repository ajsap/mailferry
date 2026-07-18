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

import (
	"testing"
	"time"
)

// utc is a fixed non-UTC zone used to prove that timezone-less inputs are
// resolved in the supplied location and then normalised to UTC deterministically.
var fixedZone = time.FixedZone("TEST+05", 5*3600)

func mustTime(t *testing.T, layout, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(layout, s)
	if err != nil {
		t.Fatalf("bad test time %q: %v", s, err)
	}
	return tm.UTC()
}

func TestDateRangeResolveValidForms(t *testing.T) {
	cases := []struct {
		name       string
		from, to   string
		wantActive bool
		wantFromU  string // expected From in RFC3339 UTC ("" = no from)
		wantToU    string
	}{
		{"both empty -> inactive", "", "", false, "", ""},
		{"date only, from only", "2026-07-19", "", true, "2026-07-18T19:00:00Z", ""},
		{"full instant Z, to only", "", "2026-07-19T00:00:00Z", true, "", "2026-07-19T00:00:00Z"},
		{"explicit offset", "2026-07-19T00:00:00+00:00", "", true, "2026-07-19T00:00:00Z", ""},
		{"space separator", "2026-07-19 09:30:00Z", "", true, "2026-07-19T09:30:00Z", ""},
		{"minute precision no tz", "2026-07-19T00:00", "", true, "2026-07-18T19:00:00Z", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dr, err := ResolveRange(c.from, c.to, fixedZone)
			if err != nil {
				t.Fatalf("ResolveRange(%q,%q) unexpected error: %v", c.from, c.to, err)
			}
			if dr.Active != c.wantActive {
				t.Fatalf("Active=%v, want %v", dr.Active, c.wantActive)
			}
			if c.wantFromU == "" {
				if dr.HasFrom {
					t.Fatalf("HasFrom=true, want unbounded")
				}
			} else {
				want := mustTime(t, time.RFC3339, c.wantFromU)
				if !dr.HasFrom || !dr.From.Equal(want) {
					t.Fatalf("From=%v, want %v", dr.From, want)
				}
			}
			if c.wantToU == "" {
				if dr.HasTo {
					t.Fatalf("HasTo=true, want unbounded")
				}
			} else {
				want := mustTime(t, time.RFC3339, c.wantToU)
				if !dr.HasTo || !dr.To.Equal(want) {
					t.Fatalf("To=%v, want %v", dr.To, want)
				}
			}
		})
	}
}

func TestDateRangeRejectsRelativeAndGarbage(t *testing.T) {
	bad := []string{"now", "PRESENT", "today", "yesterday", "epoch",
		"2026-13-01", "not-a-date", "19/07/2026", "2026/07/19"}
	for _, s := range bad {
		if _, err := ResolveRange(s, "", fixedZone); err == nil {
			t.Fatalf("ResolveRange(%q) accepted a bad/relative value; want error", s)
		}
	}
}

func TestDateRangeRejectsInvertedWindow(t *testing.T) {
	_, err := ResolveRange("2026-07-19T00:00:00Z", "2026-07-18T00:00:00Z", fixedZone)
	if err == nil {
		t.Fatal("from later than to was accepted; want an error before any connection")
	}
}

func TestDateRangeIncludesInclusiveBounds(t *testing.T) {
	dr, err := ResolveRange("2026-07-01T00:00:00Z", "2026-07-31T23:59:59Z", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	in := []time.Time{
		mustTime(t, time.RFC3339, "2026-07-01T00:00:00Z"), // lower bound inclusive
		mustTime(t, time.RFC3339, "2026-07-15T12:00:00Z"), // middle
		mustTime(t, time.RFC3339, "2026-07-31T23:59:59Z"), // upper bound inclusive
	}
	for _, ts := range in {
		if !dr.Includes(ts) {
			t.Fatalf("Includes(%v)=false, want true (inclusive)", ts)
		}
	}
	out := []time.Time{
		mustTime(t, time.RFC3339, "2026-06-30T23:59:59Z"),
		mustTime(t, time.RFC3339, "2026-08-01T00:00:00Z"),
		{}, // zero time (undatable) is always OUT of an active window
	}
	for _, ts := range out {
		if dr.Includes(ts) {
			t.Fatalf("Includes(%v)=true, want false", ts)
		}
	}
}

func TestDateRangeInactiveIncludesEverything(t *testing.T) {
	var dr DateRange // zero value = inactive
	for _, ts := range []time.Time{{}, time.Now(), mustTime(t, time.RFC3339, "1990-01-01T00:00:00Z")} {
		if !dr.Includes(ts) {
			t.Fatalf("inactive range excluded %v; an inactive range includes everything", ts)
		}
	}
}

func TestDateRangeOpenSides(t *testing.T) {
	// from only: everything on/after the bound is in.
	fromOnly, _ := ResolveRange("2026-07-01T00:00:00Z", "", time.UTC)
	if fromOnly.Includes(mustTime(t, time.RFC3339, "2026-06-30T23:59:59Z")) {
		t.Fatal("from-only admitted a message before the lower bound")
	}
	if !fromOnly.Includes(mustTime(t, time.RFC3339, "2030-01-01T00:00:00Z")) {
		t.Fatal("from-only excluded a message far after the lower bound")
	}
	// to only: everything on/before the bound is in.
	toOnly, _ := ResolveRange("", "2026-07-01T00:00:00Z", time.UTC)
	if !toOnly.Includes(mustTime(t, time.RFC3339, "1999-01-01T00:00:00Z")) {
		t.Fatal("to-only excluded a message before the upper bound")
	}
	if toOnly.Includes(mustTime(t, time.RFC3339, "2026-07-01T00:00:01Z")) {
		t.Fatal("to-only admitted a message after the upper bound")
	}
}

func TestParseInternalDateFormats(t *testing.T) {
	want := mustTime(t, time.RFC3339, "2026-07-19T09:30:00Z")
	ok := []string{
		"19-Jul-2026 09:30:00 +0000", // two-digit day
		" 9-Jul-2026 09:30:00 +0000", // space-padded single-digit day (different day)
	}
	// first form should equal want; just assert parse success + UTC for both.
	if got, parsed := ParseInternalDate(ok[0]); !parsed || !got.Equal(want) {
		t.Fatalf("ParseInternalDate(%q)=%v parsed=%v, want %v", ok[0], got, parsed, want)
	}
	if _, parsed := ParseInternalDate(ok[1]); !parsed {
		t.Fatalf("ParseInternalDate(%q) failed to parse a space-padded day", ok[1])
	}
	// offset is honoured: +1200 shifts the instant back 12h in UTC.
	got, parsed := ParseInternalDate("19-Jul-2026 12:00:00 +1200")
	if !parsed || !got.Equal(mustTime(t, time.RFC3339, "2026-07-19T00:00:00Z")) {
		t.Fatalf("offset not applied: got %v parsed=%v", got, parsed)
	}
	for _, bad := range []string{"", "not a date", "2026-07-19"} {
		if _, parsed := ParseInternalDate(bad); parsed {
			t.Fatalf("ParseInternalDate(%q) reported success on garbage", bad)
		}
	}
}

func TestDateRangeIncludesRaw(t *testing.T) {
	dr, _ := ResolveRange("2026-07-01T00:00:00Z", "2026-07-31T23:59:59Z", time.UTC)
	// in-window, datable
	if in, parsed := dr.IncludesRaw("15-Jul-2026 12:00:00 +0000"); !in || !parsed {
		t.Fatalf("IncludesRaw in-window: in=%v parsed=%v, want true,true", in, parsed)
	}
	// out-of-window, datable
	if in, parsed := dr.IncludesRaw("15-Aug-2026 12:00:00 +0000"); in || !parsed {
		t.Fatalf("IncludesRaw out-of-window: in=%v parsed=%v, want false,true", in, parsed)
	}
	// undatable under an active range: OUT and not-parsed (caller skips deliberately)
	if in, parsed := dr.IncludesRaw("garbage"); in || parsed {
		t.Fatalf("IncludesRaw undatable: in=%v parsed=%v, want false,false", in, parsed)
	}
	// inactive range: everything in, parsed reported true (datability moot)
	var inactive DateRange
	if in, parsed := inactive.IncludesRaw("garbage"); !in || !parsed {
		t.Fatalf("inactive IncludesRaw: in=%v parsed=%v, want true,true", in, parsed)
	}
}

func TestDateRangeStoreLoadRoundTrip(t *testing.T) {
	orig, err := ResolveRange("2026-07-01T00:00:00Z", "2026-07-31T23:59:59Z", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	got := RangeFromStored(orig.StoredFrom(), orig.StoredTo(), orig.TZ)
	if !got.Active || !got.HasFrom || !got.HasTo {
		t.Fatalf("round-trip lost activity/bounds: %+v", got)
	}
	if !got.From.Equal(orig.From) || !got.To.Equal(orig.To) {
		t.Fatalf("round-trip drifted: from %v/%v to %v/%v",
			got.From, orig.From, got.To, orig.To)
	}
	// An unbounded side round-trips as unbounded, not as the epoch.
	fromOnly, _ := ResolveRange("2026-07-01T00:00:00Z", "", time.UTC)
	rt := RangeFromStored(fromOnly.StoredFrom(), fromOnly.StoredTo(), fromOnly.TZ)
	if rt.HasTo {
		t.Fatal("unbounded upper side became bounded after a store/load round-trip")
	}
}
