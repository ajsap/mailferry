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

// Per-mailbox orchestration: connection pair, folder loop, reconnect and
// retry policy. Authentication failures are NEVER auto-retried (lockouts).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/imapx"
	"github.com/ajsap/mailferry/v2/internal/state"
	"github.com/ajsap/mailferry/v2/internal/util"
)

type MailboxRunner struct {
	Spec    config.MailboxSpec
	Cfg     *config.Run
	DB      *state.DB
	MB      *MBStats
	Bus     *Bus
	Session func(string)
	Logf    func(string, ...any)
	Ctx     context.Context
	Owner   string
	MID     int64

	src, dst      *imapx.Client
	cliMu         sync.Mutex
	failedFolders []string
	firstError    string
	existing      map[string]int64
	poison        map[string]*poisonRec
	leaseLost     atomic.Bool
}

var errStaleFailed = fmt.Errorf("stale recovery exhausted")
var errLeaseLost = fmt.Errorf("lease lost — taken over by another worker")

func (r *MailboxRunner) poisonFor(folder string) *poisonRec {
	if r.poison == nil {
		r.poison = map[string]*poisonRec{}
	}
	if r.poison[folder] == nil {
		r.poison[folder] = newPoisonRec()
	}
	return r.poison[folder]
}

// HasLiveClients reports whether the runner currently holds open
// connections (a mailbox waiting in backoff has none — never "stalled").
func (r *MailboxRunner) HasLiveClients() bool {
	r.cliMu.Lock()
	defer r.cliMu.Unlock()
	return (r.src != nil && r.src.Alive()) || (r.dst != nil && r.dst.Alive())
}

// KickConnections force-closes the pair so stalled awaits unwind and the
// folder loop reconnects and resumes from the last confirmed checkpoint.
func (r *MailboxRunner) KickConnections(reason string) {
	r.cliMu.Lock()
	defer r.cliMu.Unlock()
	for _, c := range []*imapx.Client{r.src, r.dst} {
		if c != nil && c.Alive() {
			c.Abort(&imapx.StaleKick{Msg: reason})
		}
	}
}

// AbortClients hard-closes the pair (shutdown escalation: ConnLost, no
// resume semantics — the run is ending and per-UID state is committed).
func (r *MailboxRunner) AbortClients() int {
	r.cliMu.Lock()
	defer r.cliMu.Unlock()
	n := 0
	for _, c := range []*imapx.Client{r.src, r.dst} {
		if c != nil && c.Alive() {
			c.Abort(nil)
			n++
		}
	}
	return n
}

func (r *MailboxRunner) history(event, status, details string) {
	if r.Bus != nil {
		r.Bus.History(event, status, r.Spec.Label(), details)
	}
}

func (r *MailboxRunner) busLog(sev, msg string) {
	if r.Bus != nil {
		r.Bus.Log(sev, r.Spec.Label(), msg)
	}
}

func (r *MailboxRunner) SetExisting(folder string, n int64) {
	if r.existing == nil {
		r.existing = map[string]int64{}
	}
	r.existing[folder] = n
	var sum int64
	for _, v := range r.existing {
		sum += v
	}
	r.MB.Set(func(m *MBValues) { m.Dst.Existing = sum })
}

func (r *MailboxRunner) RefreshTotals() {
	mt, md, bt, bd := r.DB.MailboxTotals(r.MID)
	r.MB.Set(func(m *MBValues) {
		m.MsgsTotal, m.MsgsDone, m.BytesTotal, m.BytesDone = mt, md, bt, bd
	})
}

func (r *MailboxRunner) sleepInterruptible(d time.Duration) bool {
	select {
	case <-r.Ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

// Run drives the whole mailbox to a terminal status string.
func (r *MailboxRunner) Run() string {
	spec := r.Spec
	mid, _, err := r.DB.UpsertMailbox(spec.Key(), spec.Src.Host, spec.Src.User,
		spec.Dst.Host, spec.Dst.User)
	if err != nil {
		r.fail("state database: " + err.Error())
		return "FAILED"
	}
	r.MID = mid
	ok, other, age := r.DB.TryLease(mid, r.Owner)
	if ok && other != "" && other != r.Owner && age >= r.DB.LeaseFresh {
		r.Session(fmt.Sprintf("[%03d] %s: stale lock auto-reset: %s last heartbeat %ds ago — continuing",
			spec.Index, spec.Src.User, other, int(age)))
		r.history("Stale lock auto-reset", "OK", fmt.Sprintf(
			"%s last heartbeat %ds ago (> %ds) — that worker is dead; continuing",
			state.ShortWorker(other), int(age), int(r.DB.LeaseFresh)))
	}
	if !ok {
		// Reclaim conclusively dead workers here; anything fresher is REMOTE
		// (mirrored + reclaimed later by the cluster monitor when it dies).
		if hbAge, found := r.DB.WorkerHBAge(other); (found && hbAge > r.Cfg.WorkerTimeout) ||
			(!found && age > 150) {
			obsOwner, obsTS := r.DB.ReadLease(mid)
			if obsOwner == other && r.DB.ForceLease(mid, r.Owner, obsOwner, obsTS) {
				r.Session(fmt.Sprintf("[%03d] %s: reclaimed from offline worker %s — resuming from the last checkpoint",
					spec.Index, spec.Src.User, state.ShortWorker(other)))
				r.history("Worker takeover", "WARN", fmt.Sprintf(
					"reclaimed from offline worker %s — resuming from the last checkpoint",
					state.ShortWorker(other)))
				ok = true
			}
		}
		if !ok {
			ownerRun := r.DB.WorkerRun(other)
			if ownerRun == "" {
				ownerRun = "unknown"
			}
			msg := fmt.Sprintf("owned by worker %s (heartbeat %ds ago, run %s)",
				state.ShortWorker(other), int(age), ownerRun)
			r.MB.Set(func(m *MBValues) {
				m.Status = "REMOTE"
				m.Op = "worker " + state.ShortWorker(other)
				m.Detail = msg
				m.Start = time.Now()
			})
			r.Session(fmt.Sprintf("[%03d] %s -> %s: REMOTE — %s",
				spec.Index, spec.Src.User, spec.Dst.User, msg))
			r.history("Mailbox already active", "WARN", msg+" — standing by as failover")
			return "REMOTE"
		}
	}

	leaseStop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Duration(r.Cfg.DBHeartbeat * float64(time.Second)))
		defer t.Stop()
		for {
			select {
			case <-leaseStop:
				return
			case <-t.C:
				if !r.DB.RefreshLease(mid, r.Owner) {
					// Failover: another worker CAS-claimed this mailbox.
					// Stop ALL local work on it immediately — the new owner
					// resumes from the checkpoint; intent rows keep the
					// handover duplicate-safe.
					r.leaseLost.Store(true)
					r.Logf("lease lost — mailbox taken over by another worker; stopping local work")
					r.history("Mailbox taken over", "WARN",
						"another worker claimed it after our heartbeats went silent")
					r.KickConnections("lease lost — taken over by another worker")
					return
				}
			}
		}
	}()
	defer func() {
		close(leaseStop)
		r.DB.ClearLease(mid, r.Owner)
	}()

	maxAttempts := 1 + r.Cfg.Retries
	r.MB.Set(func(m *MBValues) {
		m.MaxAttempts = maxAttempts
		m.Start = time.Now()
		m.Status = "RUNNING"
	})
	r.DB.SetMailboxStatus(mid, "RUNNING", "")
	r.Logf("=== mailbox %s -> %s start", spec.Src.Label(), spec.Dst.Label())
	r.history("Migration started", "OK", spec.Src.User+" → "+spec.Dst.User)

	status := "FAILED"
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		r.MB.Set(func(m *MBValues) { m.Attempt = attempt })
		st, err := r.attempt()
		if err == nil {
			status = st
			break
		}
		if errors.Is(err, errStaleFailed) {
			r.fail(fmt.Sprintf("recovery exhausted after %d attempt(s) — marked STALE; rerun to resume",
				busAttempts(r.Bus, spec.Label())))
			status = "STALE"
			break
		}
		if errors.Is(err, errLeaseLost) {
			status = "REMOTE"
			break
		}
		if errors.Is(err, errStopRun) || r.Ctx.Err() != nil {
			status = "CANCELLED"
			break
		}
		var auth *imapx.AuthErr
		if errors.As(err, &auth) {
			r.fail(auth.Error())
			status = "FAILED"
			break
		}
		reason := err.Error()
		if r.firstError == "" {
			r.firstError = reason
		}
		if attempt >= maxAttempts {
			r.fail(reason)
			status = "FAILED"
			break
		}
		delay := util.BackoffDelay(r.Cfg.RetryDelay, attempt, 900)
		r.Logf("attempt %d/%d failed (%s) — retrying in %ds", attempt, maxAttempts,
			reason, int(delay))
		r.Session(fmt.Sprintf("[%03d] %s -> %s: attempt %d/%d failed (%s) — retrying in %ds",
			spec.Index, spec.Src.User, spec.Dst.User, attempt, maxAttempts, reason, int(delay)))
		r.MB.Set(func(m *MBValues) {
			m.Status = "RETRYING"
			m.Error = reason
			m.Retries++
		})
		if r.sleepInterruptible(time.Duration(delay * float64(time.Second))) {
			status = "CANCELLED"
			break
		}
		r.MB.Set(func(m *MBValues) { m.Status = "RUNNING" })
	}

	if status == "REMOTE" {
		ownerNow, _ := r.DB.ReadLease(mid)
		who := state.ShortWorker(ownerNow)
		r.MB.Set(func(m *MBValues) {
			m.Status = "REMOTE"
			m.Op = "worker " + who
			m.Detail = "taken over by " + ownerNow
		})
		r.Session(fmt.Sprintf("[%03d] %s -> %s: REMOTE — taken over by worker %s; watching",
			spec.Index, spec.Src.User, spec.Dst.User, who))
		return "REMOTE"
	}
	if r.Bus != nil {
		if att := r.Bus.StaleAttempts(spec.Label()); att > 0 && !r.Bus.IsStaleFailed(spec.Label()) &&
			(status == "SUCCESS" || status == "WARNINGS") {
			// recovery worked and the mailbox finished before the supervisor
			// could observe the progress — credit it here
			r.Bus.ClearStale(spec.Label())
			r.Session(fmt.Sprintf("transfer recovered: %s resumed and completed (reconnect %d)",
				spec.Label(), att))
			r.history("Transfer recovered", "OK",
				fmt.Sprintf("resumed and completed (reconnect %d)", att))
		}
	}
	mt, md, bt, bd := r.DB.MailboxTotals(mid)
	lastErr := ""
	if status != "SUCCESS" && status != "WARNINGS" {
		lastErr = r.firstError
	}
	snapAttempt := 1
	r.MB.Set(func(m *MBValues) {
		m.Status = status
		m.End = time.Now()
		snapAttempt = m.Attempt
	})
	r.DB.SetMailbox(mid, status, lastErr, snapAttempt, mt, md, bt, bd)
	v := r.MB.Snap()
	extra := ""
	if status != "SUCCESS" && r.firstError != "" {
		extra = " (" + r.firstError + ")"
	}
	r.Session(fmt.Sprintf("[%03d] %s -> %s: %s%s elapsed=%s new=%d adopted=%d skipped=%d data=%s",
		spec.Index, spec.Src.User, spec.Dst.User, status, extra,
		util.FmtDHMS(time.Since(v.Start).Seconds()), v.Appended, v.Adopted, v.Skipped,
		util.FmtBytes(float64(v.BytesDone))))
	r.Logf("=== mailbox end: %s%s", status, extra)
	word, hstat := map[string][2]string{
		"SUCCESS":   {"Migration completed", "OK"},
		"WARNINGS":  {"Completed with warnings", "WARN"},
		"PARTIAL":   {"Migration partial", "WARN"},
		"FAILED":    {"Migration failed", "FAIL"},
		"STALE":     {"Recovery exhausted — marked STALE", "FAIL"},
		"CANCELLED": {"Migration cancelled", "WARN"},
	}[status][0], map[string][2]string{
		"SUCCESS":   {"Migration completed", "OK"},
		"WARNINGS":  {"Completed with warnings", "WARN"},
		"PARTIAL":   {"Migration partial", "WARN"},
		"FAILED":    {"Migration failed", "FAIL"},
		"STALE":     {"Recovery exhausted — marked STALE", "FAIL"},
		"CANCELLED": {"Migration cancelled", "WARN"},
	}[status][1]
	det := fmt.Sprintf("total %s · %d msgs · elapsed %s",
		util.FmtBytes(float64(v.BytesDone)), v.MsgsDone,
		util.FmtDHMS(time.Since(v.Start).Seconds()))
	if status == "WARNINGS" {
		nf := v.Skipped + v.FailedMsgs
		tot := v.MsgsTotal
		if tot == 0 {
			tot = v.MsgsDone + nf
		}
		pctv := 100.0
		if tot > 0 {
			pctv = float64(v.MsgsDone) * 100 / float64(tot)
		}
		det = fmt.Sprintf("%d/%d migrated · %d failed · %.2f%% complete — see the Failed Message Registry",
			v.MsgsDone, tot, nf, pctv)
	} else if status != "SUCCESS" && r.firstError != "" {
		det += " · " + r.firstError
	}
	if word != "" {
		r.history(word, hstat, det)
	}
	return status
}

func busAttempts(b *Bus, label string) int {
	if b == nil {
		return 0
	}
	return b.StaleAttempts(label)
}

func (r *MailboxRunner) fail(reason string) {
	if r.firstError == "" {
		r.firstError = reason
	}
	r.MB.Set(func(m *MBValues) { m.Error = reason })
	r.Logf("FAILED: %s", reason)
}

// attempt opens the pair and works the folder queue.
func (r *MailboxRunner) attempt() (string, error) {
	cfg := r.Cfg
	r.MB.Set(func(m *MBValues) { m.Op = "CONNECT"; m.Error = "" })
	if err := r.openPair(); err != nil {
		r.closePair(false)
		return "", err
	}
	defer r.closePair(true)
	r.Logf("connected: src caps=%d dst caps=%d (src: %q)",
		len(r.src.Caps), len(r.dst.Caps), trunc(r.src.Greeting, 60))

	r.MB.Set(func(m *MBValues) { m.Op = "LIST folders" })
	plans, err := BuildPlan(r.src, r.dst, cfg, cfg.FolderMap(),
		func(s string) { r.Logf("%s", s) })
	if err != nil {
		return "", err
	}
	var est int64
	for _, p := range plans {
		est += p.EstMsgs
	}
	r.MB.Set(func(m *MBValues) {
		m.FoldersTotal = len(plans)
		m.FolderIndex = 0
	})
	r.Logf("plan: %d folder(s), est %d msgs", len(plans), est)

	r.failedFolders = nil
	started := map[string]bool{}
	for qi := 0; qi < len(plans); qi++ {
		if r.Ctx.Err() != nil {
			return "CANCELLED", nil
		}
		for r.Bus != nil && r.Bus.IsPaused() && r.Ctx.Err() == nil {
			time.Sleep(300 * time.Millisecond) // paused between folders
		}
		plan := plans[qi]
		if !started[plan.SrcDisplay] {
			started[plan.SrcDisplay] = true
			r.MB.Set(func(m *MBValues) { m.FolderIndex++ })
		}
		if err := r.syncFolderWithReconnect(plan); err != nil {
			if errors.Is(err, errStopRun) {
				return "CANCELLED", nil
			}
			return "", err
		}
	}

	if r.Ctx.Err() != nil {
		return "CANCELLED", nil
	}
	v := r.MB.Snap()
	if len(r.failedFolders) > 0 {
		if v.MsgsDone > 0 {
			return "PARTIAL", nil
		}
		return "FAILED", nil
	}
	// DB-authoritative: a resume where the poison is already recorded still
	// reports WARNINGS even though no NEW failure happened this run.
	dbFailed, dbSkipped := r.DB.MailboxFailedSkipped(r.MID)
	if v.Skipped > 0 || v.FailedMsgs > 0 || dbFailed > 0 || dbSkipped > 0 {
		r.MB.Set(func(m *MBValues) {
			if dbFailed > m.FailedMsgs {
				m.FailedMsgs = dbFailed
			}
		})
		return "WARNINGS", nil
	}
	return "SUCCESS", nil
}

func (r *MailboxRunner) openPair() error {
	to := time.Duration(r.Cfg.Timeout * float64(time.Second))
	r.cliMu.Lock()
	r.src = imapx.NewClient(imapx.Endpoint(r.Spec.Src), to, r.Cfg.TLSVerify,
		sideAdapter{r.MB, false}, r.Spec.Label(), func(s string) { r.Logf("src: %s", s) })
	r.dst = imapx.NewClient(imapx.Endpoint(r.Spec.Dst), to, r.Cfg.TLSVerify,
		sideAdapter{r.MB, true}, r.Spec.Label(), func(s string) { r.Logf("dst: %s", s) })
	for _, c := range []*imapx.Client{r.src, r.dst} {
		c.Trace = r.Cfg.Trace
		c.Baseline = r.Cfg.Baseline
		c.ReadOnly = r.Cfg.DryRun // --dry-run: the client blocks every mutation
	}
	r.cliMu.Unlock()
	type res struct{ err error }
	ch := make(chan res, 2)
	for _, cli := range []*imapx.Client{r.src, r.dst} {
		go func(c *imapx.Client) {
			if err := c.Connect(); err != nil {
				ch <- res{err}
				return
			}
			if err := c.Login(); err != nil {
				ch <- res{err}
				return
			}
			if r.Cfg.Compress != "off" {
				c.StartCompress() // best-effort; refusal just stays uncompressed
			}
			ch <- res{nil}
		}(cli)
	}
	var firstErr error
	for i := 0; i < 2; i++ {
		if r := <-ch; r.err != nil && firstErr == nil {
			firstErr = r.err
		}
	}
	if firstErr != nil {
		return firstErr
	}
	r.badges()
	return nil
}

func (r *MailboxRunner) badges() {
	badge := func(c *imapx.Client, sec string) []string {
		b := []string{map[string]string{"ssl": "SSL", "tls": "STARTTLS", "none": "PLAIN"}[sec]}
		if c.Has("UIDPLUS") {
			b = append(b, "UID+")
		}
		if c.Has("LITERAL+") && !c.Baseline {
			b = append(b, "LIT+")
		}
		if c.Compressed() {
			b = append(b, "ZIP")
		}
		return b
	}
	sb := badge(r.src, r.Spec.Src.Security)
	db := badge(r.dst, r.Spec.Dst.Security)
	r.MB.Set(func(m *MBValues) { m.Src.Caps = sb; m.Dst.Caps = db })
}

func (r *MailboxRunner) closePair(graceful bool) {
	for _, c := range []*imapx.Client{r.src, r.dst} {
		if c == nil {
			continue
		}
		if graceful && c.Alive() && r.Ctx.Err() == nil {
			c.Logout(5 * time.Second)
		} else {
			c.Abort(nil)
		}
	}
}

// syncFolderWithReconnect: transient trouble reconnects and re-enters; the
// syncer resumes from the State Database checkpoint.
func (r *MailboxRunner) syncFolderWithReconnect(plan FolderPlan) error {
	rc := 0
	for {
		s := &folderSyncer{r: r, plan: plan, src: r.src, dst: r.dst}
		outcome, err := s.run()
		if err == nil {
			if !outcome.OK {
				r.failedFolders = append(r.failedFolders, plan.SrcDisplay)
				if r.firstError == "" {
					r.firstError = outcome.Err
				}
			}
			return nil
		}
		if errors.Is(err, errStopRun) || r.Ctx.Err() != nil {
			return errStopRun
		}
		var auth *imapx.AuthErr
		if errors.As(err, &auth) {
			return err
		}
		if !imapx.IsConnLost(err) && !imapx.IsStaleKick(err) {
			var ce *imapx.CommandErr
			if !errors.As(err, &ce) {
				return err
			}
		}
		if r.leaseLost.Load() {
			return errLeaseLost
		}
		reason := err.Error()
		staleKick := imapx.IsStaleKick(err)
		rec := r.poisonFor(plan.SrcDisplay)
		isolating := len(rec.ladder) > 0
		if staleKick && r.Bus != nil && r.Bus.IsStaleFailed(r.Spec.Label()) {
			return errStaleFailed
		}
		delay := 1.0
		switch {
		case staleKick:
			// supervisor kicks never consume the reconnect budget
		case isolating && rec.isoLosses <= 60:
			// isolation losses are the ladder doing its job
		default:
			rc++
			if rc > r.Cfg.ReconnectAttempts {
				r.Logf("[%s] giving up after %d reconnect(s): %s", plan.SrcDisplay, rc-1, reason)
				r.failedFolders = append(r.failedFolders, plan.SrcDisplay)
				if r.firstError == "" {
					r.firstError = reason
				}
				return nil
			}
			delay = util.BackoffDelay(2, rc, 60)
			r.history("Connection reconnect", "WARN",
				fmt.Sprintf("%s: attempt %d — %s", plan.SrcDisplay, rc, trunc(reason, 90)))
		}
		r.Logf("[%s] connection trouble (%s) — reconnecting in %ds",
			plan.SrcDisplay, reason, int(delay))
		r.MB.Set(func(m *MBValues) {
			m.Src.Reconnects++
			m.Op = fmt.Sprintf("RECONNECT in %ds", int(delay))
			m.Detail = trunc(reason, 120)
		})
		r.closePair(false)
		if r.sleepInterruptible(time.Duration(delay * float64(time.Second))) {
			return errStopRun
		}
		r.MB.Set(func(m *MBValues) { m.Op = "RECONNECT" })
		if err := r.openPair(); err != nil {
			return err
		}
	}
}
