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

package sysmon

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// linuxSampler samples system resources via /proc. Every read is
// best-effort: any parse or I/O error simply leaves the corresponding
// Snap field at its zero value, never spawning a process and never
// panicking.
type linuxSampler struct {
	// prevBusy/prevTotal hold the previous /proc/stat aggregate "cpu"
	// tick counts so CPU% can be computed as a delta between samples.
	// prevValid is false until the first sample has been taken, since a
	// single /proc/stat reading alone cannot yield a percentage.
	prevBusy  uint64
	prevTotal uint64
	prevValid bool
}

func newSampler() sampler {
	return &linuxSampler{}
}

func (s *linuxSampler) Sample() Snap {
	var snap Snap

	if cpu, ok := s.cpuPercent(); ok {
		snap.CPU = cpu
		snap.HasCPU = true
	}

	if load, ok := readLoadAvg(); ok {
		snap.Load = load
		snap.HasLoad = true
	}

	total, used, ok := readMemInfo()
	if ok {
		snap.MemTotal = total
		snap.MemUsed = used
	}

	if rss, ok := readRSS(); ok {
		snap.RSS = rss
	}

	return snap
}

// cpuPercent reads the aggregate "cpu" line of /proc/stat and returns the
// busy percentage since the previous sample. The very first call never
// has a prior sample to diff against, so it always returns ok=false.
func (s *linuxSampler) cpuPercent() (pct float64, ok bool) {
	busy, total, ok := readProcStatCPU()
	if !ok {
		return 0, false
	}

	prevBusy, prevTotal, prevValid := s.prevBusy, s.prevTotal, s.prevValid
	s.prevBusy, s.prevTotal, s.prevValid = busy, total, true

	if !prevValid {
		return 0, false
	}

	// Guard against counters that didn't move (or, in principle, went
	// backwards on a /proc oddity) so we never divide by zero or return
	// a nonsensical negative percentage.
	if total <= prevTotal {
		return 0, false
	}
	dTotal := total - prevTotal
	dBusy := busy - prevBusy

	pct = 100 * float64(dBusy) / float64(dTotal)
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	return pct, true
}

// readProcStatCPU parses the first line of /proc/stat, e.g.:
//
//	cpu  123 45 678 9012 34 0 5 0 0 0
//
// Fields (in order) are: user, nice, system, idle, iowait, irq, softirq,
// steal, guest, guest_nice. total is the sum of all present fields; busy
// is total minus idle and iowait (when present).
func readProcStatCPU() (busy, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 2 || fields[0] != "cpu" {
		return 0, 0, false
	}

	ticks := fields[1:]
	var sum uint64
	var idle, iowait uint64
	for i, f := range ticks {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		sum += v
		switch i {
		case 3:
			idle = v
		case 4:
			iowait = v
		}
	}
	if sum == 0 {
		return 0, 0, false
	}
	return sum - idle - iowait, sum, true
}

// readMemInfo parses /proc/meminfo for MemTotal and MemAvailable (both
// reported in kB) and returns total and used (total - available) bytes.
func readMemInfo() (total, used int64, ok bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	var memTotalKB, memAvailKB int64
	var haveTotal, haveAvail bool

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		key, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(key) {
		case "MemTotal":
			memTotalKB, haveTotal = v, true
		case "MemAvailable":
			memAvailKB, haveAvail = v, true
		}
		if haveTotal && haveAvail {
			break
		}
	}
	if !haveTotal || !haveAvail {
		return 0, 0, false
	}

	totalBytes := memTotalKB * 1024
	usedBytes := totalBytes - memAvailKB*1024
	if usedBytes < 0 {
		usedBytes = 0
	}
	return totalBytes, usedBytes, true
}

// readRSS parses VmRSS (reported in kB) out of /proc/self/status and
// returns it in bytes.
func readRSS() (int64, bool) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		key, rest, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(key) != "VmRSS" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

// readLoadAvg parses the three load-average fields from /proc/loadavg,
// e.g. "0.12 0.34 0.56 1/234 5678".
func readLoadAvg() (load [3]float64, ok bool) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return load, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return load, false
	}
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return [3]float64{}, false
		}
		load[i] = v
	}
	return load, true
}
