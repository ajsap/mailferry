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

// Batch scheduler: bounded worker pool, per-host connection budgets,
// cluster worker registration, graceful cancellation.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/state"
)

type hostSems struct {
	mu      sync.Mutex
	perHost int
	sems    map[string]chan struct{}
}

func newHostSems(perHost int) *hostSems {
	if perHost < 1 {
		perHost = 1
	}
	return &hostSems{perHost: perHost, sems: map[string]chan struct{}{}}
}

func (h *hostSems) sem(host string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := strings.ToLower(host)
	if h.sems[k] == nil {
		h.sems[k] = make(chan struct{}, h.perHost)
	}
	return h.sems[k]
}

func (h *hostSems) acquirePair(ctx context.Context, a, b string) bool {
	keys := []string{strings.ToLower(a), strings.ToLower(b)}
	if keys[1] < keys[0] {
		keys[0], keys[1] = keys[1], keys[0]
	}
	for i, k := range keys {
		select {
		case h.sem(k) <- struct{}{}:
		case <-ctx.Done():
			for j := 0; j < i; j++ {
				<-h.sem(keys[j])
			}
			return false
		}
	}
	return true
}

func (h *hostSems) releasePair(a, b string) {
	<-h.sem(a)
	<-h.sem(b)
}

type RunResult struct {
	Counts         map[string]int
	Outstanding    int64
	FailedRegistry []state.FailedRow
}

type LoggerFactory func(spec config.MailboxSpec) func(string, ...any)

// RunMigration executes the whole batch. ctx cancellation = graceful stop.
// bus may be nil (tests); a Bus wires history/logs/recovery to any UI.
func RunMigration(ctx context.Context, cfg *config.Run, specs []config.MailboxSpec,
	stats *Stats, session func(string), logFactory LoggerFactory) (RunResult, error) {
	return RunMigrationBus(ctx, cfg, specs, stats, NewBus(), session, logFactory)
}

func RunMigrationBus(ctx context.Context, cfg *config.Run, specs []config.MailboxSpec,
	stats *Stats, bus *Bus, session func(string), logFactory LoggerFactory) (RunResult, error) {
	db, err := state.Open(cfg.DBPath, cfg.Ephemeral, cfg.LockTimeout)
	if err != nil {
		return RunResult{}, err
	}
	defer db.Close()
	owner := state.LeaseOwnerID()
	bus.WorkerID = owner
	db.StartRun(cfg.RunID, cfg.CSVFile)
	db.RegisterWorker(owner, cfg.RunID)
	session(fmt.Sprintf("cluster: joined as worker %s (offline threshold %ds)",
		owner, int(cfg.WorkerTimeout)))
	bus.History("Run started", "OK", "-",
		fmt.Sprintf("%s · %d mailbox(es) · run %s", cfg.CSVFile, len(specs), cfg.RunID))
	bus.History("Worker joined", "OK", "-", owner+" — cluster on "+cfg.DBPath)
	defer db.DeregisterWorker(owner)

	hbStop := make(chan struct{})
	go func() {
		t := timeTicker(cfg.DBHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-t.C:
				db.WorkerHeartbeat(owner)
			}
		}
	}()
	defer close(hbStop)

	var pending []config.MailboxSpec
	for _, spec := range specs {
		mb := stats.Mailbox(spec.Index, spec.Label(), spec.Src.Host, spec.Dst.Host,
			spec.Dst.User)
		_, status, err := db.UpsertMailbox(spec.Key(), spec.Src.Host, spec.Src.User,
			spec.Dst.Host, spec.Dst.User)
		if err != nil {
			return RunResult{}, err
		}
		if cfg.SkipCompleted && !cfg.Force && status == "SUCCESS" {
			mb.Set(func(m *MBValues) { m.Status = "SKIPPED" })
			continue
		}
		pending = append(pending, spec)
	}

	if cfg.Order == "size" {
		// Largest known mailbox first (unknown sizes keep CSV order at the end).
		sort.SliceStable(pending, func(i, j int) bool {
			return db.SizeOfKey(pending[i].Key()) > db.SizeOfKey(pending[j].Key())
		})
	}

	workers := cfg.Workers
	if workers > len(pending) {
		workers = len(pending)
	}
	if workers < 1 {
		workers = 1
	}
	stats.mu.Lock()
	stats.Workers = workers
	stats.mu.Unlock()

	hosts := newHostSems(cfg.PerHostConns)
	slots := make(chan struct{}, workers)
	reg := NewRegistry()
	watch := newRemoteWatch()
	var wg sync.WaitGroup

	var runOne func(spec config.MailboxSpec)
	runOne = func(spec config.MailboxSpec) {
		defer wg.Done()
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-ctx.Done():
			stats.Mailbox(spec.Index, spec.Label(), spec.Src.Host, spec.Dst.Host,
				spec.Dst.User).Set(func(m *MBValues) { m.Status = "CANCELLED" })
			return
		}
		if ctx.Err() != nil {
			stats.Mailbox(spec.Index, spec.Label(), spec.Src.Host, spec.Dst.Host,
				spec.Dst.User).Set(func(m *MBValues) { m.Status = "CANCELLED" })
			return
		}
		if !hosts.acquirePair(ctx, spec.Src.Host, spec.Dst.Host) {
			stats.Mailbox(spec.Index, spec.Label(), spec.Src.Host, spec.Dst.Host,
				spec.Dst.User).Set(func(m *MBValues) { m.Status = "CANCELLED" })
			return
		}
		defer hosts.releasePair(spec.Src.Host, spec.Dst.Host)
		mb := stats.Mailbox(spec.Index, spec.Label(), spec.Src.Host, spec.Dst.Host,
			spec.Dst.User)
		runner := &MailboxRunner{Spec: spec, Cfg: cfg, DB: db, MB: mb, Bus: bus,
			Session: session, Logf: logFactory(spec), Ctx: ctx, Owner: owner}
		reg.Add(spec.Label(), runner)
		st := runner.Run()
		reg.Remove(spec.Label())
		if st == "REMOTE" && ctx.Err() == nil {
			watch.Add(spec, runner.MID, "")
			if o, _ := db.ReadLease(runner.MID); o != "" {
				watch.Add(spec, runner.MID, o)
			}
		}
	}
	respawn := func(spec config.MailboxSpec) {
		wg.Add(1)
		go runOne(spec)
	}
	for _, spec := range pending {
		spec := spec
		wg.Add(1)
		go runOne(spec)
	}

	// Control hooks for the presentation layer (shutdown escalation, manual
	// retry, CSV reload). The engine stays authoritative: hooks re-enter the
	// same admission path as everything else.
	bus.SetAborter(func() int {
		n := reg.AbortAll()
		if n > 0 {
			session(fmt.Sprintf("shutdown: closed %d IMAP connection(s) to stop promptly", n))
		}
		return n
	})
	specByLabel := map[string]config.MailboxSpec{}
	for _, sp := range specs {
		specByLabel[sp.Label()] = sp
	}
	bus.SetRequeue(func(label string, all bool) int {
		n := 0
		for _, m := range stats.Snapshot().Mailboxes {
			if m.Status != "FAILED" && m.Status != "PARTIAL" && m.Status != "STALE" {
				continue
			}
			if !all && m.Label != label {
				continue
			}
			sp, okSp := specByLabel[m.Label]
			if !okSp || reg.Get(m.Label) != nil {
				continue
			}
			bus.ClearStale(m.Label)
			bus.Log("INFO", m.Label, "re-queued for retry")
			bus.History("Mailbox re-queued", "OK", m.Label, "manual retry from the console")
			respawn(sp)
			n++
		}
		return n
	})
	bus.SetReloader(func() (int, int) {
		newSpecs, err := config.ParseCSV(cfg.CSVFile)
		if err != nil {
			bus.Log("WARN", "-", "CSV reload failed: "+err.Error())
			return 0, len(specByLabel)
		}
		added := 0
		for _, sp := range newSpecs {
			if _, known := specByLabel[sp.Label()]; known {
				continue
			}
			sp.Index = len(specByLabel) + added + 1
			specByLabel[sp.Label()] = sp
			db.UpsertMailbox(sp.Key(), sp.Src.Host, sp.Src.User, sp.Dst.Host, sp.Dst.User)
			bus.History("Mailbox added", "OK", sp.Label(), "picked up from the CSV")
			respawn(sp)
			added++
		}
		if added > 0 {
			session(fmt.Sprintf("csv reload: %d new mailbox(es) admitted", added))
		}
		return added, len(newSpecs)
	})

	supCtx, supCancel := context.WithCancel(context.Background())
	defer supCancel()
	go StaleSupervisor(supCtx, cfg, stats, bus, reg, session)
	go clusterMonitor(supCtx, cfg, stats, bus, db, owner, session, watch, respawn)

	// wait for local work; while peers still hold project mailboxes, stay
	// alive — mirror their progress and stand by as a hot failover
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			if watch.Len() > 0 && ctx.Err() == nil {
				continue
			}
			select {
			case <-done:
			default:
				continue
			}
		}
		if watch.Len() > 0 && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
			continue
		}
		break
	}
	wg.Wait()

	snap := stats.Snapshot()
	agg := snap.Agg()
	res := RunResult{Counts: agg.Counts, Outstanding: db.OutstandingFailed(),
		FailedRegistry: db.FailedRows(0, true)}
	result := fmt.Sprintf("ok=%d warnings=%d partial=%d failed=%d stale=%d cancelled=%d",
		agg.Counts["SUCCESS"], agg.Counts["WARNINGS"], agg.Counts["PARTIAL"],
		agg.Counts["FAILED"], agg.Counts["STALE"], agg.Counts["CANCELLED"])
	bus.History("Run finished", "OK", "-", result)
	db.EndRun(cfg.RunID, result)
	return res, nil
}

func timeTicker(secs float64) *tickerWrap {
	if secs < 5 {
		secs = 5
	}
	return newTicker(secs)
}
