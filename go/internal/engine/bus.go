// MailFerry - IMAP Migration & Sync
// A High-Performance Native IMAP Migration Engine
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

package engine

// The event/message bus between the migration engine and any presentation
// layer (Bubble Tea TUI, headless output, a future `mailferry attach`
// client over local IPC). The engine PUBLISHES; presentation CONSUMES.
// The engine never blocks on a slow or absent consumer, and losing every
// consumer never affects a running migration — which is exactly the
// property a later attach transport needs.

import (
	"sync"
	"time"
)

type HistoryEntry struct {
	TS      time.Time
	Event   string // "Migration started", "Entering Recovery Mode", ...
	Status  string // OK | WARN | FAIL
	Mailbox string
	Details string
}

type LogEntry struct {
	TS       time.Time
	Severity string // INFO | WARN | ERROR
	Mailbox  string
	Message  string
}

type Bus struct {
	mu      sync.Mutex
	history []HistoryEntry
	logs    []LogEntry
	errors  []LogEntry
	subs    []chan struct{}

	// runtime control surface shared with the presentation layer
	Paused       bool
	StaleFailed  map[string]bool
	StaleTries   map[string]int
	RecoveryHint map[string]bool
	Cluster      []WorkerInfo
	WorkerID     string

	// engine-installed action hooks (nil-safe; the TUI only ever calls the
	// wrapper methods below, so a missing hook is a no-op, never a crash)
	aborter  func() int                       // hard-close every open connection
	requeue  func(label string, all bool) int // re-queue FAILED/PARTIAL/STALE
	reloader func() (added int, known int)    // re-read the CSV for new rows
}

type WorkerInfo struct {
	ID        string
	Host      string
	PID       int64
	Started   time.Time
	Heartbeat time.Time
	HBAge     float64
	Active    int64
	Status    string // WORKING | IDLE | OFFLINE
}

func NewBus() *Bus {
	return &Bus{StaleFailed: map[string]bool{}, StaleTries: map[string]int{},
		RecoveryHint: map[string]bool{}}
}

const busKeep = 2000

// Notify wakes subscribers (presentation refresh) without ever blocking.
func (b *Bus) notify() {
	for _, ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Subscribe returns a non-blocking wake channel for presentation layers.
func (b *Bus) Subscribe() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan struct{}, 1)
	b.subs = append(b.subs, ch)
	return ch
}

// TogglePause flips the engine-wide pause gate. Transfers finish the byte
// in flight and wait; paused time never counts as stalled time.
func (b *Bus) TogglePause() bool {
	b.mu.Lock()
	b.Paused = !b.Paused
	p := b.Paused
	b.mu.Unlock()
	if p {
		b.Log("INFO", "-", "migration paused")
	} else {
		b.Log("INFO", "-", "migration resumed")
	}
	return p
}

// IsPaused is the engine-side gate (transfer loops poll it between messages).
func (b *Bus) IsPaused() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Paused
}

// SetAborter installs the shutdown escalation hook (scheduler-owned).
func (b *Bus) SetAborter(f func() int) { b.mu.Lock(); b.aborter = f; b.mu.Unlock() }

// AbortAllConnections hard-closes every open IMAP connection so a graceful
// stop always completes promptly even when the network is hung. State stays
// consistent — the per-message commit protocol means the next run resumes.
func (b *Bus) AbortAllConnections() int {
	b.mu.Lock()
	f := b.aborter
	b.mu.Unlock()
	if f == nil {
		return 0
	}
	return f()
}

// SetRequeue / RequeueFailed: manual retry of failed mailboxes (TUI r / R).
func (b *Bus) SetRequeue(f func(label string, all bool) int) {
	b.mu.Lock()
	b.requeue = f
	b.mu.Unlock()
}

func (b *Bus) RequeueFailed(label string, all bool) int {
	b.mu.Lock()
	f := b.requeue
	b.mu.Unlock()
	if f == nil {
		return 0
	}
	return f(label, all)
}

// SetReloader / ReloadCSV: pick up rows added to the CSV since start (TUI u).
func (b *Bus) SetReloader(f func() (int, int)) { b.mu.Lock(); b.reloader = f; b.mu.Unlock() }

func (b *Bus) ReloadCSV() (int, int) {
	b.mu.Lock()
	f := b.reloader
	b.mu.Unlock()
	if f == nil {
		return 0, 0
	}
	return f()
}

func (b *Bus) History(event, status, mailbox, details string) {
	b.mu.Lock()
	b.history = append(b.history, HistoryEntry{time.Now(), event, status, mailbox, details})
	if len(b.history) > busKeep {
		b.history = b.history[len(b.history)-busKeep:]
	}
	b.mu.Unlock()
	b.notify()
}

func (b *Bus) Log(severity, mailbox, message string) {
	b.mu.Lock()
	e := LogEntry{time.Now(), severity, mailbox, message}
	b.logs = append(b.logs, e)
	if len(b.logs) > 5000 {
		b.logs = b.logs[len(b.logs)-5000:]
	}
	if severity == "WARN" || severity == "ERROR" {
		b.errors = append(b.errors, e)
		if len(b.errors) > 500 {
			b.errors = b.errors[len(b.errors)-500:]
		}
	}
	b.mu.Unlock()
	b.notify()
}

func (b *Bus) HistorySnapshot() []HistoryEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]HistoryEntry(nil), b.history...)
}

func (b *Bus) LogsSnapshot() []LogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]LogEntry(nil), b.logs...)
}

func (b *Bus) ErrorsSnapshot() []LogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]LogEntry(nil), b.errors...)
}

func (b *Bus) SetCluster(ws []WorkerInfo) {
	b.mu.Lock()
	b.Cluster = ws
	b.mu.Unlock()
	b.notify()
}

func (b *Bus) ClusterSnapshot() []WorkerInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]WorkerInfo(nil), b.Cluster...)
}

func (b *Bus) SetStaleFailed(label string, tries int) {
	b.mu.Lock()
	b.StaleFailed[label] = true
	b.StaleTries[label] = tries
	b.mu.Unlock()
}

// RecordAttempt notes a connection-recovery attempt without marking the
// mailbox failed, so a recovery that completes before the supervisor's
// next tick is still credited as "transfer recovered" by the runner.
func (b *Bus) RecordAttempt(label string, n int) {
	b.mu.Lock()
	if n > b.StaleTries[label] {
		b.StaleTries[label] = n
	}
	b.mu.Unlock()
}

func (b *Bus) IsStaleFailed(label string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.StaleFailed[label]
}

func (b *Bus) ClearStale(label string) {
	b.mu.Lock()
	delete(b.StaleFailed, label)
	delete(b.StaleTries, label)
	b.mu.Unlock()
}

func (b *Bus) StaleAttempts(label string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.StaleTries[label]
}

func (b *Bus) SetRecoveryHint(label string) {
	b.mu.Lock()
	b.RecoveryHint[label] = true
	b.mu.Unlock()
}

func (b *Bus) TakeRecoveryHint(label string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.RecoveryHint[label] {
		delete(b.RecoveryHint, label)
		return true
	}
	return false
}
