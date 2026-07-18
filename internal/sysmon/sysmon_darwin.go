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
	"encoding/binary"
	"time"

	"golang.org/x/sys/unix"
)

// darwinLoadavg mirrors the kernel's `struct loadavg` as sysctl'd via
// "vm.loadavg": three fixed-point load averages plus the fixed-point
// scale factor to divide them by.
type darwinLoadavg struct {
	Ldavg  [3]uint32
	Fscale int64
}

// darwinSampler samples system/process resources on macOS via
// golang.org/x/sys/unix, never spawning a process. CPU% is derived from
// this process's own accumulated user+system CPU time (via Getrusage)
// versus wall-clock time between samples — there is no cheap, subprocess-
// free way to get *system-wide* CPU% here, so this mirrors the process-
// level rusage delta the task calls for.
type darwinSampler struct {
	// memTotal is looked up once and cached since "hw.memsize" never
	// changes for the lifetime of the process.
	memTotal      int64
	memTotalKnown bool

	prevCPUTime time.Duration // accumulated utime+stime at prior sample
	prevWall    time.Time
	prevValid   bool
}

func newSampler() sampler {
	return &darwinSampler{}
}

func (s *darwinSampler) Sample() Snap {
	var snap Snap

	if cpu, ok := s.cpuPercent(); ok {
		snap.CPU = cpu
		snap.HasCPU = true
	}

	if load, ok := readLoadAvg(); ok {
		snap.Load = load
		snap.HasLoad = true
	}

	if total, ok := s.memTotalBytes(); ok {
		snap.MemTotal = total
	}
	// MemUsed intentionally stays 0 (unknown) on darwin.

	if rss, ok := readMaxRSS(); ok {
		snap.RSS = rss
	}

	return snap
}

// cpuPercent computes this process's CPU utilisation (percent of one
// core) since the previous sample, from the delta of accumulated
// user+system CPU time (Getrusage) over the delta of wall-clock time.
// The first call has no prior sample to diff against and always returns
// ok=false.
func (s *darwinSampler) cpuPercent() (pct float64, ok bool) {
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}

	cpuTime := timevalToDuration(ru.Utime) + timevalToDuration(ru.Stime)
	now := time.Now()

	prevCPU, prevWall, prevValid := s.prevCPUTime, s.prevWall, s.prevValid
	s.prevCPUTime, s.prevWall, s.prevValid = cpuTime, now, true

	if !prevValid {
		return 0, false
	}

	dWall := now.Sub(prevWall)
	if dWall <= 0 {
		return 0, false
	}
	dCPU := cpuTime - prevCPU
	if dCPU < 0 {
		dCPU = 0
	}

	pct = 100 * dCPU.Seconds() / dWall.Seconds()
	if pct < 0 {
		pct = 0
	}
	return pct, true
}

// timevalToDuration converts a unix.Timeval (seconds + microseconds) to
// a time.Duration.
func timevalToDuration(tv unix.Timeval) time.Duration {
	return time.Duration(tv.Sec)*time.Second + time.Duration(tv.Usec)*time.Microsecond
}

// memTotalBytes returns total physical memory via the "hw.memsize"
// sysctl, caching the result since it is constant for the process
// lifetime.
func (s *darwinSampler) memTotalBytes() (int64, bool) {
	if s.memTotalKnown {
		return s.memTotal, true
	}
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, false
	}
	s.memTotal = int64(v)
	s.memTotalKnown = true
	return s.memTotal, true
}

// readMaxRSS returns this process's peak resident set size in bytes via
// Getrusage. On darwin, Maxrss is already reported in bytes (unlike
// Linux, where it is kB), so no scaling is applied.
func readMaxRSS() (int64, bool) {
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	if ru.Maxrss < 0 {
		return 0, false
	}
	return ru.Maxrss, true
}

// readLoadAvg reads the three system load averages via the "vm.loadavg"
// sysctl, which yields a fixed-point struct loadavg{ ldavg[3], fscale }
// that must be decoded and divided by fscale to get floating-point
// averages.
func readLoadAvg() (load [3]float64, ok bool) {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil {
		return load, false
	}

	var lv darwinLoadavg
	const want = 3*4 + 8 // 3x uint32 + int64, matching struct loadavg layout
	if len(raw) < want {
		return load, false
	}
	lv.Ldavg[0] = binary.LittleEndian.Uint32(raw[0:4])
	lv.Ldavg[1] = binary.LittleEndian.Uint32(raw[4:8])
	lv.Ldavg[2] = binary.LittleEndian.Uint32(raw[8:12])
	// The kernel struct is naturally aligned, so the int64 fscale field
	// starts at offset 16 (after 4 bytes of padding following the three
	// uint32 values), not immediately at offset 12.
	if len(raw) < 24 {
		return load, false
	}
	lv.Fscale = int64(binary.LittleEndian.Uint64(raw[16:24]))

	if lv.Fscale == 0 {
		return load, false
	}
	scale := float64(lv.Fscale)
	load[0] = float64(lv.Ldavg[0]) / scale
	load[1] = float64(lv.Ldavg[1]) / scale
	load[2] = float64(lv.Ldavg[2]) / scale
	return load, true
}
