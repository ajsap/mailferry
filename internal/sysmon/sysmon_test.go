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

package sysmon

import (
	"runtime"
	"testing"
	"time"
)

// TestMonBasicLifecycle exercises the full New -> Start -> sample ->
// Snapshot -> Stop lifecycle end to end. It must never panic, and Stop
// must return promptly (no deadlock) regardless of platform.
func TestMonBasicLifecycle(t *testing.T) {
	m := New()
	m.Start()

	// Give the background sampler enough time to take at least two
	// samples at the ~1.5s interval, which is required for CPU% (a
	// delta measurement) to become available.
	time.Sleep(3500 * time.Millisecond)

	snap := m.Snapshot()

	stopped := make(chan struct{})
	go func() {
		m.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		// good: Stop returned without deadlocking.
	case <-time.After(5 * time.Second):
		t.Fatal("Mon.Stop() did not return in time; possible deadlock")
	}

	if runtime.GOOS == "linux" {
		if snap.RSS <= 0 {
			t.Errorf("linux: expected Snap.RSS > 0, got %d", snap.RSS)
		}
		if snap.MemTotal <= 0 {
			t.Errorf("linux: expected Snap.MemTotal > 0, got %d", snap.MemTotal)
		}
		if !snap.HasCPU {
			t.Errorf("linux: expected Snap.HasCPU to be true after %s of sampling", 3500*time.Millisecond)
		}
	}
}

// TestMonStopWithoutStart ensures Stop is safe to call even if Start was
// never invoked, and that it returns promptly rather than blocking
// forever waiting on a sampler goroutine that was never launched.
func TestMonStopWithoutStart(t *testing.T) {
	m := New()

	stopped := make(chan struct{})
	go func() {
		m.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Mon.Stop() without Start() did not return in time")
	}

	// Snapshot before any sampling has occurred should just be the zero
	// value, and must not panic.
	_ = m.Snapshot()
}

// TestMonDoubleStartStop ensures repeated Start/Stop calls are safe
// no-ops and do not panic or deadlock.
func TestMonDoubleStartStop(t *testing.T) {
	m := New()
	m.Start()
	m.Start() // second call must be a no-op, not a second goroutine

	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		m.Stop()
		m.Stop() // second call must be a no-op
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("double Start/Stop did not return in time; possible deadlock")
	}
}
