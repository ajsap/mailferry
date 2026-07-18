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

package engine

// Recovery: stalled-transfer supervision and progressive failed-message
// isolation. The vocabulary is deliberate and honest:
//
//   Stalled transfer detected -> Connection recovery 1/3 ->
//   Entering Recovery Mode -> Batch isolation -> Failed message isolated ->
//   Message recorded -> Migration resumed -> Completed with warnings
//
// "Recovery exhausted" appears only when nothing else can be done.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/util"
)

// ------------------------------------------------- active runner registry --

type Registry struct {
	mu sync.Mutex
	m  map[string]*MailboxRunner
}

func NewRegistry() *Registry { return &Registry{m: map[string]*MailboxRunner{}} }

func (r *Registry) Add(label string, run *MailboxRunner) {
	r.mu.Lock()
	r.m[label] = run
	r.mu.Unlock()
}

func (r *Registry) Remove(label string) {
	r.mu.Lock()
	delete(r.m, label)
	r.mu.Unlock()
}

func (r *Registry) Get(label string) *MailboxRunner {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[label]
}

// AbortAll hard-closes every registered runner's connections (bounded
// shutdown escalation — a stalled socket must never hold up Ctrl+C).
func (r *Registry) AbortAll() int {
	r.mu.Lock()
	runners := make([]*MailboxRunner, 0, len(r.m))
	for _, run := range r.m {
		runners = append(runners, run)
	}
	r.mu.Unlock()
	n := 0
	for _, run := range runners {
		n += run.AbortClients()
	}
	return n
}

// ------------------------------------------------ poison / isolation state --

// poisonRec survives folder reconnect re-entries on the runner — this is
// what breaks the endless retry-the-same-batch loop.
type ladderEntry struct {
	uids  []uint32
	tries int
}

type poisonRec struct {
	losses    int
	suspects  map[uint32]bool
	ladder    []*ladderEntry
	noCounts  map[uint32]int
	isoLosses int
	announced bool
}

func newPoisonRec() *poisonRec {
	return &poisonRec{suspects: map[uint32]bool{}, noCounts: map[uint32]int{}}
}

// classifyFailure maps a failure reason onto the registry taxonomy.
func classifyFailure(reason string) string {
	low := lower(reason)
	switch {
	case contains(low, "appendlimit", "oversize", "too large", "exceeds"):
		return "OVERSIZE"
	case contains(low, "timed out", "timeout"):
		return "TIMEOUT"
	case contains(low, "eof", "reset", "dropped", "closed", "connection"):
		return "CONNECTION_RESET"
	case contains(low, "parse", "malformed", "invalid", "mime", "encoding"):
		return "MALFORMED_MIME"
	case contains(low, "append"):
		return "APPEND_NO"
	}
	return "UNKNOWN"
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}

func contains(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------- supervisor --

const hardIOBytes = 64 * 1024

type watchState struct {
	sig           string
	ioMark        int64
	lastProgress  time.Time
	attempts      int
	nextKick      time.Time
	episodeStart  time.Time
	episode       bool
	recoveryTried bool
}

func hardSig(m MBValues) string {
	return fmt.Sprintf("%d|%d|%d|%d|%d|%d|%d|%s|%d",
		m.MsgsDone, m.Appended, m.Adopted, m.DupSkipped, m.Skipped,
		m.BytesDone, m.FolderIndex, m.Status, m.Attempt)
}

func ioTotal(m MBValues) int64 {
	return m.Src.RXBytes + m.Src.TXBytes + m.Dst.RXBytes + m.Dst.TXBytes
}

// StaleSupervisor watches meaningful progress per mailbox and recovers
// stalled transfers automatically. Purely observational until a kick is
// due — it can never slow migration down.
func StaleSupervisor(ctx context.Context, cfg *config.Run, stats *Stats, bus *Bus,
	reg *Registry, session func(string)) {
	if cfg.StaleTimeout <= 0 {
		return
	}
	tick := cfg.StaleTimeout / 10
	if tick > 2 {
		tick = 2
	}
	if tick < 0.5 {
		tick = 0.5
	}
	spacing := cfg.RecoveryInterval
	if spacing < 1 {
		spacing = 1
	}
	retries := cfg.RecoveryRetries
	if retries < 1 {
		retries = 1
	}
	watches := map[string]*watchState{}
	t := time.NewTicker(time.Duration(tick * float64(time.Second)))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		if bus.Paused {
			for _, w := range watches {
				w.lastProgress = now
			}
			continue
		}
		snap := stats.Snapshot()
		seen := map[string]bool{}
		for _, m := range snap.Mailboxes {
			label := m.Label
			seen[label] = true
			if m.Status != "RUNNING" {
				delete(watches, label)
				continue
			}
			sig := hardSig(m)
			io := ioTotal(m)
			w := watches[label]
			if w == nil {
				watches[label] = &watchState{sig: sig, ioMark: io, lastProgress: now}
				continue
			}
			progressed := sig != w.sig || io-w.ioMark >= hardIOBytes
			if progressed {
				w.sig = sig
				w.ioMark = io
				w.lastProgress = now
				if w.episode {
					dur := util.FmtDHMS(now.Sub(w.episodeStart).Seconds())
					stats.Bump("stalls_recovered")
					bus.ClearStale(label)
					session(fmt.Sprintf("transfer recovered: %s — resumed after reconnect %d (%s)",
						label, w.attempts, dur))
					bus.Log("INFO", label, fmt.Sprintf("transfer recovered — resumed (reconnect %d)", w.attempts))
					bus.History("Transfer recovered", "OK", label,
						fmt.Sprintf("transfer resumed after reconnect %d (%s)", w.attempts, dur))
					w.episode = false
					w.attempts = 0
				}
				continue
			}
			frozen := now.Sub(w.lastProgress).Seconds()
			if !w.episode {
				if frozen < cfg.StaleTimeout {
					continue
				}
				run := reg.Get(label)
				if run == nil || !run.HasLiveClients() {
					w.lastProgress = now // deliberate wait (backoff) — not a stall
					continue
				}
				w.episode = true
				w.episodeStart = w.lastProgress
				w.attempts = 0
				w.nextKick = now
				stats.Bump("stalls_detected")
				where := fmt.Sprintf("folder %s · op %s", orDash(m.Folder), orDash(m.Op))
				session(fmt.Sprintf("stalled transfer detected: %s — no progress for %s (%s)",
					label, util.FmtDHMS(frozen), where))
				bus.Log("WARN", label, fmt.Sprintf("stalled transfer — no progress for %s (%s)",
					util.FmtDHMS(frozen), where))
				bus.History("Stalled transfer detected", "WARN", label,
					fmt.Sprintf("no progress for %s — %s", util.FmtDHMS(frozen), where))
			}
			if now.Before(w.nextKick) {
				continue
			}
			run := reg.Get(label)
			if run == nil {
				continue
			}
			if w.attempts >= retries {
				if cfg.IsolateFailed && !w.recoveryTried {
					// reconnecting alone did not restart the transfer: switch
					// strategy — Recovery Mode isolates problem messages
					w.recoveryTried = true
					w.attempts = 0
					bus.SetRecoveryHint(label)
					session(fmt.Sprintf("Recovery Mode: %s — repeated connection failures; "+
						"isolating problematic messages", label))
					bus.Log("INFO", label, "Entering Recovery Mode — isolating problematic messages")
					bus.History("Entering Recovery Mode", "OK", label,
						"repeated failures — isolating problematic messages")
					run.KickConnections("recovery mode — isolate problematic messages")
					w.nextKick = now.Add(time.Duration(spacing * float64(time.Second)))
					continue
				}
				w.episode = false
				stats.Bump("stalls_failed")
				bus.SetStaleFailed(label, w.attempts)
				msg := fmt.Sprintf("recovery exhausted after %d reconnect(s) and message "+
					"isolation — marked STALE; rerun to resume", w.attempts)
				session("RECOVERY EXHAUSTED: " + label + " — " + msg)
				bus.Log("ERROR", label, "RECOVERY EXHAUSTED — "+msg)
				bus.History("Recovery exhausted", "FAIL", label, msg)
				run.KickConnections("stale — recovery exhausted")
				continue
			}
			w.attempts++
			bus.RecordAttempt(label, w.attempts)
			run.MB.Set(func(v *MBValues) {
				v.Op = fmt.Sprintf("RECOVERY #%d/%d", w.attempts, retries)
				v.Detail = "stalled — reconnecting"
			})
			session(fmt.Sprintf("connection recovery: %s reconnect %d/%d — resume from the last checkpoint",
				label, w.attempts, retries))
			bus.Log("INFO", label, fmt.Sprintf("connection recovery — reconnect %d/%d", w.attempts, retries))
			bus.History("Connection recovery", "OK", label,
				fmt.Sprintf("reconnect %d/%d — resume from last checkpoint", w.attempts, retries))
			run.KickConnections(fmt.Sprintf("stalled — connection recovery %d/%d", w.attempts, retries))
			w.nextKick = now.Add(time.Duration(spacing * float64(time.Second)))
		}
		for label := range watches {
			if !seen[label] {
				delete(watches, label)
			}
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
