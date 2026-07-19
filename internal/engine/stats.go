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

package engine

// Live telemetry model. The engine mutates under a mutex; the renderer
// goroutine pulls plain snapshots. Wire byte counters tick on every socket
// read/write, so the dashboard can never mistake slow for stalled.

import (
	"sync"
	"time"
)

type SideValues struct {
	Host       string
	ConnState  string
	Caps       []string
	Existing   int64
	Reconnects int64
	RXBytes    int64
	TXBytes    int64
}

type MBValues struct {
	Index        int
	Label        string
	Label2       string
	Status       string
	Attempt      int
	MaxAttempts  int
	Op           string
	Detail       string
	Folder       string
	FolderIndex  int
	FoldersTotal int
	MsgsDone     int64
	MsgsTotal    int64
	BytesDone    int64
	BytesTotal   int64
	Appended     int64
	Adopted      int64
	DupSkipped   int64
	Skipped      int64
	FailedMsgs   int64
	// PriorDone: messages already confirmed on the destination BEFORE
	// this run started (per-UID State Database rows) - MsgsDone counts
	// them, while Appended/Adopted are this-run deltas. Planned: dry-run
	// would-be copies. Together they close the accounting identity:
	//   MsgsDone == Appended + Adopted + PriorDone + Planned
	PriorDone int64
	Planned   int64
	Retries   int64
	Error     string
	Start     time.Time
	End       time.Time
	Src       SideValues
	Dst       SideValues
	LogPath   string
}

type MBStats struct {
	mu sync.Mutex
	v  MBValues
}

func (m *MBStats) Set(fn func(*MBValues)) {
	m.mu.Lock()
	fn(&m.v)
	m.mu.Unlock()
}

func (m *MBStats) Add(fn func(*MBValues)) { m.Set(fn) }

func (m *MBStats) Snap() MBValues {
	m.mu.Lock()
	v := m.v
	m.mu.Unlock()
	return v
}

// side stat adapters for the IMAP client
type sideAdapter struct {
	mb  *MBStats
	dst bool
}

func (s sideAdapter) RX(n int) {
	s.mb.Set(func(m *MBValues) {
		if s.dst {
			m.Dst.RXBytes += int64(n)
		} else {
			m.Src.RXBytes += int64(n)
		}
	})
}

func (s sideAdapter) TX(n int) {
	s.mb.Set(func(m *MBValues) {
		if s.dst {
			m.Dst.TXBytes += int64(n)
		} else {
			m.Src.TXBytes += int64(n)
		}
	})
}

func (s sideAdapter) State(st string) {
	s.mb.Set(func(m *MBValues) {
		if s.dst {
			m.Dst.ConnState = st
		} else {
			m.Src.ConnState = st
		}
	})
}

// Stats aggregates every mailbox for the renderer.
type Stats struct {
	mu              sync.Mutex
	Mailboxes       []*MBStats
	BatchStart      time.Time
	Mode            string
	Workers         int
	CSVFile         string
	DBPath          string
	LogsDir         string
	Interrupted     bool
	StallsDetected  int64
	StallsRecovered int64
	StallsFailed    int64
}

func (s *Stats) Bump(name string) {
	s.mu.Lock()
	switch name {
	case "stalls_detected":
		s.StallsDetected++
	case "stalls_recovered":
		s.StallsRecovered++
	case "stalls_failed":
		s.StallsFailed++
	}
	s.mu.Unlock()
}

func NewStats() *Stats { return &Stats{BatchStart: time.Now(), Mode: "Migration & Sync"} }

func (s *Stats) Mailbox(index int, label, srcHost, dstHost, label2 string) *MBStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.Mailboxes {
		snap := m.Snap()
		if snap.Index == index {
			return m
		}
	}
	mb := &MBStats{}
	mb.Set(func(m *MBValues) {
		m.Index = index
		m.Label = label
		m.Label2 = label2
		m.Status = "QUEUED"
		m.Attempt = 1
		m.MaxAttempts = 1
		m.Src.Host = srcHost
		m.Dst.Host = dstHost
		m.Src.ConnState = "-"
		m.Dst.ConnState = "-"
	})
	s.Mailboxes = append(s.Mailboxes, mb)
	return mb
}

type Snapshot struct {
	TS          time.Time
	BatchStart  time.Time
	Mode        string
	Workers     int
	CSVFile     string
	DBPath      string
	LogsDir     string
	Interrupted bool
	Stalls      [3]int64 // detected, recovered, failed
	Mailboxes   []MBValues
}

func (s *Stats) Snapshot() Snapshot {
	s.mu.Lock()
	list := append([]*MBStats(nil), s.Mailboxes...)
	snap := Snapshot{TS: time.Now(), BatchStart: s.BatchStart, Mode: s.Mode,
		Workers: s.Workers, CSVFile: s.CSVFile, DBPath: s.DBPath,
		LogsDir: s.LogsDir, Interrupted: s.Interrupted,
		Stalls: [3]int64{s.StallsDetected, s.StallsRecovered, s.StallsFailed}}
	s.mu.Unlock()
	for _, m := range list {
		snap.Mailboxes = append(snap.Mailboxes, m.Snap())
	}
	// stable order by index
	for i := 1; i < len(snap.Mailboxes); i++ {
		for j := i; j > 0 && snap.Mailboxes[j].Index < snap.Mailboxes[j-1].Index; j-- {
			snap.Mailboxes[j], snap.Mailboxes[j-1] = snap.Mailboxes[j-1], snap.Mailboxes[j]
		}
	}
	return snap
}

// Agg holds the whole-run aggregates derived from a snapshot.
type Agg struct {
	MsgsDone, MsgsTotal, BytesDone, BytesTotal      int64
	Appended, Adopted, DupSkipped, SkippedMsgs      int64
	FailedMsgs, Retries, Reconnects, WireRX, WireTX int64
	PriorDone, Planned                              int64
	Counts                                          map[string]int
}

func (s Snapshot) Agg() Agg {
	a := Agg{Counts: map[string]int{}}
	for _, m := range s.Mailboxes {
		a.MsgsDone += m.MsgsDone
		a.MsgsTotal += m.MsgsTotal
		a.BytesDone += m.BytesDone
		a.BytesTotal += m.BytesTotal
		a.Appended += m.Appended
		a.Adopted += m.Adopted
		a.DupSkipped += m.DupSkipped
		a.SkippedMsgs += m.Skipped
		a.FailedMsgs += m.FailedMsgs
		a.PriorDone += m.PriorDone
		a.Planned += m.Planned
		a.Retries += m.Retries
		a.Reconnects += m.Src.Reconnects + m.Dst.Reconnects
		a.WireRX += m.Src.RXBytes + m.Dst.RXBytes
		a.WireTX += m.Src.TXBytes + m.Dst.TXBytes
		a.Counts[m.Status]++
	}
	return a
}

// RateTracker: rolling-window rate + ETA (same maths as v1.x).
type RateTracker struct {
	mu      sync.Mutex
	window  time.Duration
	samples []rateSample
}

type rateSample struct {
	t     time.Time
	bytes int64
	msgs  int64
}

func NewRateTracker() *RateTracker { return &RateTracker{window: 10 * time.Second} }

func (r *RateTracker) Update(t time.Time, bytes, msgs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, rateSample{t, bytes, msgs})
	cutoff := t.Add(-r.window)
	for len(r.samples) > 2 && r.samples[0].t.Before(cutoff) {
		r.samples = r.samples[1:]
	}
}

func (r *RateTracker) Rates() (bytesPerSec, msgsPerSec float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.samples) < 2 {
		return 0, 0
	}
	a, b := r.samples[0], r.samples[len(r.samples)-1]
	dt := b.t.Sub(a.t).Seconds()
	if dt <= 0 {
		return 0, 0
	}
	br := float64(b.bytes-a.bytes) / dt
	mr := float64(b.msgs-a.msgs) / dt
	if br < 0 {
		br = 0
	}
	if mr < 0 {
		mr = 0
	}
	return br, mr
}

func (r *RateTracker) ETA(remBytes, remMsgs int64) (float64, bool) {
	br, mr := r.Rates()
	if remBytes > 0 && br > 1 {
		return float64(remBytes) / br, true
	}
	if remMsgs > 0 && mr > 0.01 {
		return float64(remMsgs) / mr, true
	}
	return 0, false
}
