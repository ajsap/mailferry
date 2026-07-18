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

// Cluster coordination: several MailFerry instances sharing one State
// Database cooperate as workers. Mailboxes are claimed atomically through
// leases; a worker silent for --worker-timeout is offline and its
// mailboxes are reclaimed with a CAS takeover, resuming from the last
// confirmed checkpoint. A mailbox owned by a live peer shows as REMOTE
// with progress mirrored from the State Database.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/state"
)

type remoteWatch struct {
	mu sync.Mutex
	m  map[string]remoteEntry // label -> entry
}

type remoteEntry struct {
	spec  config.MailboxSpec
	mid   int64
	owner string
}

func newRemoteWatch() *remoteWatch { return &remoteWatch{m: map[string]remoteEntry{}} }

func (r *remoteWatch) Add(spec config.MailboxSpec, mid int64, owner string) {
	r.mu.Lock()
	r.m[spec.Label()] = remoteEntry{spec, mid, owner}
	r.mu.Unlock()
}

func (r *remoteWatch) Remove(label string) {
	r.mu.Lock()
	delete(r.m, label)
	r.mu.Unlock()
}

func (r *remoteWatch) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.m)
}

func (r *remoteWatch) Entries() []remoteEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]remoteEntry, 0, len(r.m))
	for _, e := range r.m {
		out = append(out, e)
	}
	return out
}

// clusterMonitor heartbeats this worker, keeps the roster fresh, mirrors
// REMOTE progress and performs automatic failover reclaim.
func clusterMonitor(ctx context.Context, cfg *config.Run, stats *Stats, bus *Bus,
	db *state.DB, owner string, session func(string), watch *remoteWatch,
	respawn func(config.MailboxSpec)) {
	beat := time.Time{}
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if time.Since(beat).Seconds() >= cfg.DBHeartbeat*0.8 {
			beat = time.Now()
			db.WorkerHeartbeat(owner)
			var infos []WorkerInfo
			for _, w := range db.ListWorkers(cfg.WorkerTimeout) {
				infos = append(infos, WorkerInfo{ID: w.ID, Host: w.Host, PID: w.PID,
					Started:   time.Unix(int64(w.Started), 0),
					Heartbeat: time.Unix(int64(w.Heartbeat), 0),
					HBAge:     w.HBAge, Active: w.Active, Status: w.Status})
			}
			bus.SetCluster(infos)
		}
		for _, e := range watch.Entries() {
			mb := stats.Mailbox(e.spec.Index, e.spec.Label(), e.spec.Src.Host,
				e.spec.Dst.Host, e.spec.Dst.User)
			leaseOwner, leaseTS := db.ReadLease(e.mid)
			// finished remotely -> reflect and stop watching
			if leaseOwner == "" || leaseOwner == owner {
				_, status, err := db.UpsertMailbox(e.spec.Key(), e.spec.Src.Host,
					e.spec.Src.User, e.spec.Dst.Host, e.spec.Dst.User)
				if err == nil && (status == "SUCCESS" || status == "WARNINGS" ||
					status == "PARTIAL" || status == "FAILED" || status == "STALE" ||
					status == "CANCELLED") {
					watch.Remove(e.spec.Label())
					mt, md, bt, bd := db.MailboxTotals(e.mid)
					who := state.ShortWorker(e.owner)
					mb.Set(func(m *MBValues) {
						m.Status = status
						m.End = time.Now()
						m.Op = "worker " + who
						m.MsgsTotal, m.MsgsDone, m.BytesTotal, m.BytesDone = mt, md, bt, bd
					})
					session(fmt.Sprintf("[%03d] %s: %s (completed by worker %s)",
						e.spec.Index, e.spec.Src.User, status, who))
					st := "OK"
					if status != "SUCCESS" {
						st = "WARN"
					}
					bus.History("Completed by another worker", st, e.spec.Label(),
						fmt.Sprintf("%s finished with %s", who, status))
					continue
				}
				// released but unfinished (peer exited gracefully) -> resume here
				watch.Remove(e.spec.Label())
				session(fmt.Sprintf("[%03d] %s: released by worker %s — resuming here",
					e.spec.Index, e.spec.Src.User, state.ShortWorker(e.owner)))
				bus.History("Job released — resuming", "OK", e.spec.Label(),
					fmt.Sprintf("worker %s released the mailbox", state.ShortWorker(e.owner)))
				mb.Set(func(m *MBValues) { m.Status = "QUEUED"; m.Op = ""; m.Detail = "" })
				respawn(e.spec)
				continue
			}
			if leaseOwner != e.owner { // changed hands — keep watching new owner
				e.owner = leaseOwner
				watch.Add(e.spec, e.mid, leaseOwner)
				mb.Set(func(m *MBValues) {
					m.Op = "worker " + state.ShortWorker(leaseOwner)
					m.Detail = "processed by " + leaseOwner
				})
			}
			// owner offline? verified CAS reclaim (a live worker survives it)
			hbAge, found := db.WorkerHBAge(leaseOwner)
			leaseAge := float64(time.Now().Unix()) - leaseTS
			dead := (found && hbAge > cfg.WorkerTimeout) || (!found && leaseAge > 150)
			if dead {
				if db.ForceLease(e.mid, owner, leaseOwner, leaseTS) {
					watch.Remove(e.spec.Label())
					who := state.ShortWorker(leaseOwner)
					silent := hbAge
					if !found {
						silent = leaseAge
					}
					session(fmt.Sprintf("[%03d] %s: worker %s went offline — reclaimed; "+
						"resuming from the last checkpoint", e.spec.Index, e.spec.Src.User, who))
					bus.History("Worker takeover", "WARN", e.spec.Label(),
						fmt.Sprintf("worker %s silent for %ds — resuming from the last "+
							"confirmed checkpoint", who, int(silent)))
					mb.Set(func(m *MBValues) {
						m.Status = "QUEUED"
						m.Op = ""
						m.Detail = ""
						m.Error = ""
					})
					respawn(e.spec)
				}
				continue
			}
			// alive and working: mirror progress for the local dashboard
			mt, md, bt, bd := db.MailboxTotals(e.mid)
			hbTxt := fmt.Sprintf("hb %ds", int(hbAge))
			if !found {
				hbTxt = fmt.Sprintf("lease %ds", int(leaseAge))
			}
			mb.Set(func(m *MBValues) {
				m.Op = fmt.Sprintf("worker %s · %s", state.ShortWorker(leaseOwner), hbTxt)
				if mt > 0 {
					m.MsgsTotal = mt
				}
				m.MsgsDone = md
				if bt > 0 {
					m.BytesTotal = bt
				}
				m.BytesDone = bd
			})
		}
	}
}
