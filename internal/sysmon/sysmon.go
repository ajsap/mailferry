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

// Package sysmon: system resource sampling — informational only, zero
// subprocesses. Linux via /proc; macOS via sysctl/rusage; else N/A.
package sysmon

import (
	"sync"
	"sync/atomic"
	"time"
)

// interval is how often the background sampler refreshes the snapshot.
const interval = 1500 * time.Millisecond

// Snap is a point-in-time, best-effort resource sample. Every field is
// zero-valued (and, where applicable, its Has* flag is false) whenever the
// underlying data could not be obtained on the current platform.
type Snap struct {
	CPU      float64 // percent; see HasCPU
	HasCPU   bool
	Load     [3]float64
	HasLoad  bool
	MemTotal int64 // bytes; 0 = unknown
	MemUsed  int64 // bytes; 0 = unknown
	RSS      int64 // bytes; 0 = unknown (this process)
}

// Mon is a background system-resource sampler. Use New to construct one,
// Start to begin sampling, Snapshot to read the latest sample, and Stop to
// shut the sampler down. A Mon is safe for concurrent use.
type Mon struct {
	mu   sync.Mutex
	last Snap

	sample sampler // platform-specific sampling state, never nil

	startOnce sync.Once
	stopOnce  sync.Once
	started   atomic.Bool
	stop      chan struct{}
	done      chan struct{}
}

// New creates a ready-to-use Mon. Call Start to begin sampling.
func New() *Mon {
	return &Mon{
		sample: newSampler(),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// Start launches the background sampler goroutine. It is safe to call at
// most once per Mon; subsequent calls are no-ops.
func (m *Mon) Start() {
	m.startOnce.Do(func() {
		m.started.Store(true)
		go m.loop()
	})
}

// Stop signals the background sampler to exit and waits for it to do so.
// It is safe to call Stop multiple times, and safe to call even if Start
// was never called (in which case it returns immediately).
func (m *Mon) Stop() {
	m.stopOnce.Do(func() {
		close(m.stop)
	})
	if m.started.Load() {
		<-m.done
	}
}

func (m *Mon) loop() {
	defer close(m.done)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Take an immediate first sample so Snapshot has something as soon as
	// possible, then continue on the regular interval.
	m.tick()

	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.tick()
		}
	}
}

func (m *Mon) tick() {
	snap := m.sample.Sample()
	m.mu.Lock()
	m.last = snap
	m.mu.Unlock()
}

// Snapshot returns a thread-safe copy of the latest sample. Before the
// first sample completes, Snapshot returns the zero Snap.
func (m *Mon) Snapshot() Snap {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

// sampler is implemented per-platform (sysmon_linux.go, sysmon_darwin.go,
// sysmon_other.go). Sample must never panic, log, or spawn processes, and
// must return promptly (no blocking I/O beyond quick local reads).
type sampler interface {
	Sample() Snap
}
