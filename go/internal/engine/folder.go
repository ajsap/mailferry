// MailFerry - IMAP Migration & Sync
// High-Performance Native IMAP Migration Engine
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

// Per-folder sync: scan -> reconcile -> adopt -> stream transfer -> verify.
//
// Idempotency invariants (identical to the Python engine):
//   - no APPEND without an `inflight` intent row
//   - no `done` without APPENDUID or a positive destination probe
//   - UIDVALIDITY churn resets rows to re-verification, never blind re-copy
//   - losing the DB re-enters adoption: duplicates are impossible by design

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ajsap/mailferry/v2/internal/imapx"
	"github.com/ajsap/mailferry/v2/internal/state"
	"github.com/ajsap/mailferry/v2/internal/util"
)

type FolderOutcome struct {
	OK      bool
	Copied  int64
	Adopted int64
	Skipped int64
	Failed  int64
	Err     string
}

var systemFlags = map[string]bool{
	"\\SEEN": true, "\\ANSWERED": true, "\\FLAGGED": true,
	"\\DELETED": true, "\\DRAFT": true,
}

func cleanFlags(flags string, stripKeywords bool) string {
	var out []string
	for _, f := range strings.Fields(flags) {
		up := strings.ToUpper(f)
		if up == "\\RECENT" {
			continue
		}
		if stripKeywords && (!strings.HasPrefix(f, "\\") || !systemFlags[up]) {
			continue
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

type folderSyncer struct {
	r          *MailboxRunner
	plan       FolderPlan
	fid        int64
	src        *imapx.Client
	dst        *imapx.Client
	rec        *poisonRec
	registry   map[uint32]string
	copied     []uint32
	lastAppUID uint32
	lossPhase  string // "fetch" (source) | "append" (destination)
}

func (s *folderSyncer) logf(format string, a ...any) {
	s.r.Logf("[%s] "+format, append([]any{s.plan.SrcDisplay}, a...)...)
}

func (s *folderSyncer) op(verb string) {
	s.r.MB.Set(func(m *MBValues) { m.Op = verb; m.Folder = s.plan.SrcDisplay })
}

func (s *folderSyncer) stopped() bool {
	select {
	case <-s.r.Ctx.Done():
		return true
	default:
		return false
	}
}

var errStopRun = fmt.Errorf("stop requested")

func (s *folderSyncer) run() (FolderOutcome, error) {
	out := FolderOutcome{OK: true}
	db, plan := s.r.DB, s.plan
	if s.stopped() {
		return out, errStopRun
	}
	s.rec = s.r.poisonFor(plan.SrcDisplay)
	s.op("SELECT src")
	fid, uvSrcOld, uvDstOld, prevBracket, adoptDone, err := db.FolderRow(
		s.r.MID, plan.SrcDisplay, plan.DstDisplay)
	if err != nil {
		return out, err
	}
	s.fid = fid
	sel, err := s.src.Select(plan.SrcWire, true)
	if err != nil {
		return out, err
	}
	if uvSrcOld != 0 && sel.UIDValidity != 0 && uvSrcOld != int64(sel.UIDValidity) {
		s.logf("source UIDVALIDITY changed %d -> %d: replanning folder", uvSrcOld, sel.UIDValidity)
		db.ResetFolderMessages(fid, false)
		adoptDone = false
	}

	// destination folder: STATUS (create when missing)
	s.op("STATUS dst")
	st, err := s.dst.Status(plan.DstWire)
	if err != nil {
		s.op("CREATE dst")
		s.logf("creating destination folder %s", plan.DstDisplay)
		if err := s.dst.Create(plan.DstWire); err != nil {
			return out, err
		}
		if s.r.Cfg.Subscribe {
			s.dst.Subscribe(plan.DstWire)
		}
		if st, err = s.dst.Status(plan.DstWire); err != nil {
			return out, err
		}
	}
	uvDst := st["UIDVALIDITY"]
	dstMsgs := st["MESSAGES"]
	dstUIDNext := st["UIDNEXT"]
	s.r.SetExisting(plan.SrcDisplay, dstMsgs)
	if uvDstOld != 0 && uvDst != 0 && uvDstOld != uvDst {
		s.logf("destination UIDVALIDITY changed %d -> %d: re-verifying presence via adoption",
			uvDstOld, uvDst)
		db.ResetFolderMessages(fid, true)
		adoptDone = false
		prevBracket = 0
	}

	// ---- crash reconciliation (inflight rows) ----
	if inflight := db.RowsByState(fid, "inflight"); len(inflight) > 0 {
		if err := s.reconcileInflight(inflight, prevBracket, &out); err != nil {
			return out, err
		}
	}

	// ---- source scan (new/unknown UIDs only) ----
	s.op("SCAN src")
	srcUIDs, err := s.src.UIDSearchAll()
	if err != nil {
		return out, err
	}
	srcIV := util.ToIntervals(srcUIDs)
	knownIV := db.KnownUIDIntervals(fid)
	missing := util.IntervalsDiff(srcIV, knownIV)
	totalMissing := util.IntervalsCount(missing)
	adoptionNeeded := dstMsgs > 0 && !s.r.Cfg.NoDedupScan &&
		(!adoptDone || s.r.Cfg.RescanDest)
	var scanned int64
	for _, ss := range util.SetStrings(missing) {
		if s.stopped() {
			return out, errStopRun
		}
		metas, err := s.src.UIDFetchMeta(ss, adoptionNeeded)
		if err != nil {
			return out, err
		}
		rows := make([]state.MsgRow, 0, len(metas))
		for _, m := range metas {
			fp := ""
			if adoptionNeeded {
				fp = util.FingerprintFromHeaders(m.Header, m.Size)
			}
			rows = append(rows, state.MsgRow{SrcUID: m.UID, Size: m.Size,
				Flags: strings.Join(m.Flags, " "), InternalDate: m.InternalDate, FP: fp})
		}
		db.InsertPlanned(fid, rows)
		scanned += int64(len(metas))
		s.op(fmt.Sprintf("SCAN src %d/%d", scanned, totalMissing))
	}
	db.UpdateFolder(fid, map[string]any{
		"uv_src": sel.UIDValidity, "last_uidnext_src": sel.UIDNext})

	// fingerprints for older planned rows (crash before adoption etc.)
	if adoptionNeeded {
		planned := db.RowsByState(fid, "planned")
		var nofp []uint32
		for _, r := range planned {
			if r.FP == "" {
				nofp = append(nofp, r.SrcUID)
			}
		}
		if len(nofp) > 0 {
			for _, ss := range util.SetStrings(util.ToIntervals(nofp)) {
				metas, err := s.src.UIDFetchMeta(ss, true)
				if err != nil {
					return out, err
				}
				pairs := map[uint32]string{}
				for _, m := range metas {
					pairs[m.UID] = util.FingerprintFromHeaders(m.Header, m.Size)
				}
				db.SetFP(fid, pairs)
			}
		}
	}

	s.r.RefreshTotals()

	// ---- adoption of pre-synced destination content ----
	if adoptionNeeded {
		planned := db.RowsByState(fid, "planned")
		if len(planned) > 0 {
			n, err := s.adopt(planned, dstMsgs)
			if err != nil {
				return out, err
			}
			out.Adopted += n
		}
		db.UpdateFolder(fid, map[string]any{"adopt_done": 1, "uv_dst": uvDst})
	} else {
		db.UpdateFolder(fid, map[string]any{"uv_dst": uvDst})
	}
	db.UpdateFolder(fid, map[string]any{"last_uidnext_dst": dstUIDNext})

	// ---- transfer ----
	s.registry = db.RegistryUIDs(s.r.MID, plan.SrcDisplay)
	if n := db.FolderCounts(fid)["failed"][0]; n > 0 && s.r.Cfg.SkipKnownFailed {
		s.logf("skipping %d previously failed message(s) — recorded in the Failed "+
			"Message Registry (mailferry retry-failed re-queues them)", n)
	}
	// supervisor hand-off: a stalled transfer that reconnects did not fix
	// enters Recovery Mode targeting the current front of the queue
	if s.r.Bus != nil && s.r.Cfg.IsolateFailed && len(s.rec.ladder) == 0 &&
		s.r.Bus.TakeRecoveryHint(s.r.Spec.Label()) {
		if head := db.RowsByState(fid, "planned"); len(head) > 0 {
			n := s.r.Cfg.AppendWindow
			if n < 1 {
				n = 8
			}
			if n > len(head) {
				n = len(head)
			}
			uids := make([]uint32, n)
			for i := 0; i < n; i++ {
				uids[i] = head[i].SrcUID
			}
			s.rec.ladder = append(s.rec.ladder, &ladderEntry{uids: uids})
			s.announceRecovery(n)
		}
	}
	if len(s.rec.ladder) > 0 && s.r.Cfg.IsolateFailed {
		if err := s.isolationPhase(&out); err != nil {
			s.noteTransportLoss(err)
			return out, err
		}
	}
	toCopy := db.RowsByState(fid, "planned")
	var failures []failedRow
	if len(toCopy) > 0 {
		var totalBytes int64
		for _, r := range toCopy {
			totalBytes += r.Size
		}
		s.logf("migrating %d message(s), %s", len(toCopy), util.FmtBytes(float64(totalBytes)))
		rows := toCopy
		for attempt := 1; attempt <= s.r.Cfg.MsgRetries; attempt++ {
			if s.stopped() {
				return out, errStopRun
			}
			var err error
			failures, err = s.transferPass(rows, attempt > 1, &out)
			if err != nil {
				s.noteTransportLoss(err)
				return out, err
			}
			s.notePass(rows, failures)
			if len(failures) == 0 {
				break
			}
			rows = rows[:0]
			for _, f := range failures {
				rows = append(rows, f.row)
			}
			if attempt < s.r.Cfg.MsgRetries {
				s.logf("%d message(s) failed (pass %d); retrying%s", len(rows), attempt,
					map[bool]string{true: " without keywords", false: ""}[attempt == 1])
				time.Sleep(time.Duration(min64(10, int64(2*attempt))) * time.Second)
			}
		}
		// deterministic per-message rejections after all passes -> registry
		for _, f := range failures {
			s.permanentFail(f.row, classifyFailure(f.err), f.err, &out)
		}
	}
	s.markRecoveries()

	if s.r.Cfg.SyncFlags {
		if err := s.syncFlagsPass(); err != nil {
			return out, err
		}
	}

	// ---- finalize + count verification ----
	counts := db.FolderCounts(fid)
	nDone, bDone := counts["done"][0], counts["done"][1]
	nSkip := counts["skipped"][0]
	nFail := counts["failed"][0]
	status := "DONE"
	if nSkip+nFail > 0 {
		status = "WARNINGS"
	}
	var nAll, bAll int64
	for _, cb := range counts {
		nAll += cb[0]
		bAll += cb[1]
	}
	lastErr := ""
	if status != "DONE" {
		lastErr = fmt.Sprintf("%d message(s) could not be migrated", nSkip+nFail)
	}
	adFlag := 0
	if status == "DONE" || adoptionNeeded {
		adFlag = 1
	}
	db.UpdateFolder(fid, map[string]any{
		"status": status, "msgs_total": nAll, "bytes_total": bAll,
		"msgs_done": nDone, "bytes_done": bDone, "adopt_done": adFlag,
		"last_error": lastErr})
	if st2, err := s.dst.Status(plan.DstWire); err == nil {
		s.logf("done: src=%d dst=%d synced=%d adopted=%d copied=%d skipped=%d failed=%d",
			util.IntervalsCount(srcIV), st2["MESSAGES"], nDone, out.Adopted, out.Copied, nSkip, nFail)
	}
	out.OK = true
	return out, nil
}

// canonFlags: order-independent canonical form for flag comparison — the
// wire order of a FLAGS list is not significant (RFC 3501).
func canonFlags(s string) string {
	var keep []string
	for _, f := range strings.Fields(s) {
		if !strings.EqualFold(f, `\Recent`) {
			keep = append(keep, f)
		}
	}
	sort.Strings(keep)
	return strings.Join(keep, " ")
}

// syncFlagsPass (--sync-flags, backup mode): re-apply flags that changed on
// the source to messages already synced. Deletions are never propagated.
func (s *folderSyncer) syncFlagsPass() error {
	db := s.r.DB
	done := db.RowsByState(s.fid, "done")
	var withDst []state.MsgRow
	for _, r := range done {
		if r.DstUID > 0 {
			withDst = append(withDst, r)
		}
	}
	if len(withDst) == 0 {
		return nil
	}
	s.op(fmt.Sprintf("FLAGS-SYNC 0/%d", len(withDst)))
	uids := make([]uint32, len(withDst))
	for i, r := range withDst {
		uids[i] = r.SrcUID
	}
	current := map[uint32]string{}
	for _, ss := range util.SetStrings(util.ToIntervals(uids)) {
		metas, err := s.src.UIDFetchMeta(ss, false)
		if err != nil {
			return err
		}
		for _, m := range metas {
			current[m.UID] = canonFlags(strings.Join(m.Flags, " "))
		}
	}
	type change struct {
		row state.MsgRow
		new string
	}
	var changed []change
	for _, r := range withDst {
		if nf, ok := current[r.SrcUID]; ok && nf != canonFlags(r.Flags) {
			changed = append(changed, change{r, nf})
		}
	}
	if len(changed) == 0 {
		return nil
	}
	s.logf("flag re-sync: %d message(s) changed", len(changed))
	if _, err := s.dst.Select(s.plan.DstWire, false); err != nil {
		return err
	}
	n := 0
	for _, ch := range changed {
		if s.stopped() {
			return errStopRun
		}
		if err := s.dst.UIDStoreFlags(ch.row.DstUID, cleanFlags(ch.new, false)); err != nil {
			if imapx.IsConnLost(err) {
				return err
			}
			s.logf("flag sync failed for uid %d: %v", ch.row.SrcUID, err)
			continue
		}
		db.UpdateFlags(s.fid, ch.row.SrcUID, ch.new)
		n++
		s.op(fmt.Sprintf("FLAGS-SYNC %d/%d", n, len(changed)))
	}
	return nil
}

// ------------------------------------------------------------ reconcile --

func (s *folderSyncer) reconcileInflight(inflight []state.MsgRow, prevBracket int64,
	out *FolderOutcome) error {
	db := s.r.DB
	s.op(fmt.Sprintf("RECONCILE %d", len(inflight)))
	s.logf("reconciling %d in-flight message(s) from an interrupted run", len(inflight))
	// ensure fingerprints (from SOURCE) for all inflight rows
	var nofp []uint32
	for _, r := range inflight {
		if r.FP == "" {
			nofp = append(nofp, r.SrcUID)
		}
	}
	if len(nofp) > 0 {
		for _, ss := range util.SetStrings(util.ToIntervals(nofp)) {
			metas, err := s.src.UIDFetchMeta(ss, true)
			if err != nil {
				return err
			}
			pairs := map[uint32]string{}
			for _, m := range metas {
				pairs[m.UID] = util.FingerprintFromHeaders(m.Header, m.Size)
			}
			db.SetFP(s.fid, pairs)
		}
		inflight = db.RowsByState(s.fid, "inflight")
	}
	// scan the destination tail appended since the recorded bracket
	if _, err := s.dst.Select(s.plan.DstWire, true); err != nil {
		return err
	}
	dstUIDs, err := s.dst.UIDSearchAll()
	if err != nil {
		return err
	}
	lo := uint32(1)
	if prevBracket > 1 {
		lo = uint32(prevBracket)
	}
	var tail []uint32
	for _, u := range dstUIDs {
		if u >= lo {
			tail = append(tail, u)
		}
	}
	fpmap := map[string][]uint32{}
	for _, ss := range util.SetStrings(util.ToIntervals(tail)) {
		metas, err := s.dst.UIDFetchMeta(ss, true)
		if err != nil {
			return err
		}
		for _, m := range metas {
			fp := util.FingerprintFromHeaders(m.Header, m.Size)
			fpmap[fp] = append(fpmap[fp], m.UID)
		}
	}
	var done []state.DoneTriple
	var back []uint32
	for _, r := range inflight {
		if q := fpmap[r.FP]; len(q) > 0 && r.FP != "" {
			done = append(done, state.DoneTriple{SrcUID: r.SrcUID, DstUID: int64(q[0])})
			fpmap[r.FP] = q[1:]
		} else {
			back = append(back, r.SrcUID)
		}
	}
	if len(done) > 0 {
		db.MarkDone(s.fid, done, "adopted")
		s.r.MB.Add(func(m *MBValues) {
			m.Adopted += int64(len(done))
			m.DupSkipped += int64(len(done))
		})
		out.Adopted += int64(len(done))
		s.logf("reconcile: %d were already delivered (adopted), %d will be re-copied",
			len(done), len(back))
	}
	if len(back) > 0 {
		db.MarkState(s.fid, back, "planned", "")
	}
	return nil
}

// ---------------------------------------------------------------- adopt --

func (s *folderSyncer) adopt(planned []state.MsgRow, dstMsgs int64) (int64, error) {
	db := s.r.DB
	s.op(fmt.Sprintf("ADOPT scan dst 0/%d", dstMsgs))
	s.logf("destination has %d pre-existing message(s): fingerprint-matching to prevent duplicates",
		dstMsgs)
	if _, err := s.dst.Select(s.plan.DstWire, true); err != nil {
		return 0, err
	}
	dstUIDs, err := s.dst.UIDSearchAll()
	if err != nil {
		return 0, err
	}
	fpmap := map[string][]uint32{}
	var scanned int64
	for _, ss := range util.SetStrings(util.ToIntervals(dstUIDs)) {
		if s.stopped() {
			return 0, errStopRun
		}
		metas, err := s.dst.UIDFetchMeta(ss, true)
		if err != nil {
			return 0, err
		}
		for _, m := range metas {
			fp := util.FingerprintFromHeaders(m.Header, m.Size)
			fpmap[fp] = append(fpmap[fp], m.UID)
		}
		scanned += int64(len(metas))
		s.op(fmt.Sprintf("ADOPT scan dst %d/%d", scanned, dstMsgs))
	}
	var triples []state.DoneTriple
	var adoptedBytes int64
	for _, r := range planned {
		if q := fpmap[r.FP]; len(q) > 0 && r.FP != "" {
			triples = append(triples, state.DoneTriple{SrcUID: r.SrcUID, DstUID: int64(q[0])})
			fpmap[r.FP] = q[1:]
			adoptedBytes += r.Size
		}
	}
	if len(triples) > 0 {
		db.MarkDone(s.fid, triples, "adopted")
		s.r.MB.Add(func(m *MBValues) {
			m.Adopted += int64(len(triples))
			m.DupSkipped += int64(len(triples))
			m.MsgsDone += int64(len(triples))
			m.BytesDone += adoptedBytes
		})
		s.logf("adopted %d message(s) already present on the Destination Server (%s) — not migrated again",
			len(triples), util.FmtBytes(float64(adoptedBytes)))
	}
	return int64(len(triples)), nil
}

// -------------------------------------------------------------- transfer --

type failedRow struct {
	row state.MsgRow
	err string
}

type pendAppend struct {
	row  state.MsgRow
	fp   string
	pend *imapx.Pending
	size int64
}

// transferPass streams a pipelined FETCH->APPEND window over rows.
func (s *folderSyncer) transferPass(rows []state.MsgRow, stripKeywords bool,
	out *FolderOutcome) ([]failedRow, error) {
	cfg, db, mb := s.r.Cfg, s.r.DB, s.r.MB
	var failures []failedRow
	var pend []pendAppend
	var doneBatch []state.DoneTriple
	lastFlush := time.Now()

	flush := func(force bool) {
		if len(doneBatch) > 0 && (force || len(doneBatch) >= 200 ||
			time.Since(lastFlush) > 1500*time.Millisecond) {
			db.MarkDone(s.fid, doneBatch, "copied")
			doneBatch = nil
			lastFlush = time.Now()
		}
	}
	settleOne := func() error {
		pa := pend[0]
		pend = pend[1:]
		res, err := pa.pend.Wait()
		s.dst.AppendDone()
		if err != nil {
			if imapx.IsConnLost(err) {
				return err
			}
			failures = append(failures, failedRow{pa.row, err.Error()})
			return nil
		}
		duid := imapx.AppendUIDOf(res)
		doneBatch = append(doneBatch, state.DoneTriple{SrcUID: pa.row.SrcUID,
			DstUID: duid, FP: pa.fp})
		s.copied = append(s.copied, pa.row.SrcUID)
		mb.Add(func(m *MBValues) {
			m.MsgsDone++
			m.BytesDone += pa.size
			m.Appended++
		})
		out.Copied++
		return nil
	}

	type fetchItem struct {
		row state.MsgRow
		bh  *imapx.BodyHandle
	}
	var fetchQ []fetchItem
	next := 0
	var newlyWindowed []uint32
	startMore := func() error {
		for next < len(rows) && len(fetchQ) < cfg.FetchWindow {
			r := rows[next]
			next++
			bh, err := s.src.BodyFetch(r.SrcUID)
			if err != nil {
				return err
			}
			fetchQ = append(fetchQ, fetchItem{r, bh})
			newlyWindowed = append(newlyWindowed, r.SrcUID)
		}
		return nil
	}

	fail := func(err error) ([]failedRow, error) {
		flush(true)
		return failures, err
	}

	if err := startMore(); err != nil {
		return fail(err)
	}
	for len(fetchQ) > 0 {
		if len(newlyWindowed) > 0 {
			// intent rows BEFORE any of their appends can start
			db.MarkState(s.fid, newlyWindowed, "inflight", "")
			newlyWindowed = nil
		}
		if s.stopped() {
			flush(true)
			return failures, errStopRun
		}
		for s.r.Bus != nil && s.r.Bus.IsPaused() && !s.stopped() {
			time.Sleep(300 * time.Millisecond) // paused: finish nothing new
		}
		item := fetchQ[0]
		fetchQ = fetchQ[1:]
		if err := startMore(); err != nil {
			return fail(err)
		}
		s.lossPhase = "fetch" // a loss here blames the source
		size, err := item.bh.WaitSize(time.Duration(cfg.Timeout * float64(time.Second)))
		if err != nil {
			if imapx.IsConnLost(err) {
				return fail(err)
			}
			failures = append(failures, failedRow{item.row, "fetch failed: " + err.Error()})
			continue
		}
		s.op(fmt.Sprintf("MIGRATE uid %d (%s)", item.row.SrcUID, util.FmtBytes(float64(size))))
		flags := cleanFlags(item.row.Flags, stripKeywords)
		s.lastAppUID = item.row.SrcUID // poison attribution
		s.lossPhase = "append"         // a loss here blames the message
		sink, err := s.dst.AppendBegin(s.plan.DstWire, flags, item.row.InternalDate, size)
		if err != nil {
			drainBody(item.bh)
			return fail(err)
		}
		var sniffer util.HeaderSniffer
		chunks, errs := item.bh.Chunks()
	stream:
		for {
			select {
			case chunk, ok := <-chunks:
				if !ok {
					break stream
				}
				sniffer.Feed(chunk)
				if werr := sink.Write(chunk); werr != nil {
					return fail(werr)
				}
			case rerr := <-errs:
				return fail(rerr)
			}
		}
		pending, err := sink.Finish()
		if err != nil {
			return fail(err)
		}
		fp := item.row.FP
		if fp == "" {
			fp = sniffer.Fingerprint(size)
		}
		pend = append(pend, pendAppend{item.row, fp, pending, size})
		for len(pend) >= cfg.AppendWindow {
			if err := settleOne(); err != nil {
				return fail(err)
			}
		}
		flush(false)
	}
	for len(pend) > 0 {
		if err := settleOne(); err != nil {
			return fail(err)
		}
	}
	flush(true)
	return failures, nil
}

func drainBody(bh *imapx.BodyHandle) {
	go func() {
		ch, _ := bh.Chunks()
		for range ch {
		}
	}()
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------- failure isolation (recovery) --

func (s *folderSyncer) hist(event, status, details string) {
	if s.r.Bus != nil {
		d := s.plan.SrcDisplay
		if details != "" {
			d += ": " + details
		}
		s.r.Bus.History(event, status, s.r.Spec.Label(), d)
	}
}

func (s *folderSyncer) announceRecovery(n int) {
	if s.rec.announced {
		return
	}
	s.rec.announced = true
	s.logf("Recovery Mode — investigating repeated failures (%d suspect message(s)); "+
		"isolating problematic messages", n)
	s.hist("Entering Recovery Mode", "OK",
		fmt.Sprintf("repeated failures — isolating %d suspect message(s)", n))
}

// notePass: deterministic (tagged NO) failures accumulate per UID across
// reconnect re-entries; a fully clean pass clears the suspicion state.
func (s *folderSyncer) notePass(rows []state.MsgRow, failures []failedRow) {
	failed := map[uint32]bool{}
	for _, f := range failures {
		failed[f.row.SrcUID] = true
	}
	for _, r := range rows {
		if failed[r.SrcUID] {
			s.rec.noCounts[r.SrcUID]++
		} else {
			delete(s.rec.noCounts, r.SrcUID)
		}
	}
	if len(failures) == 0 {
		s.rec.losses = 0
		s.rec.suspects = map[uint32]bool{}
	}
}

// noteTransportLoss distinguishes transport trouble (retry the same batch
// after reconnecting) from a poison message (repeated deaths on the same
// window -> Recovery Mode). Source-side losses never blame a message.
func (s *folderSyncer) noteTransportLoss(err error) {
	if !s.r.Cfg.IsolateFailed || !imapx.IsConnLost(err) && !imapx.IsStaleKick(err) {
		return
	}
	if s.lossPhase != "append" {
		return
	}
	rec := s.rec
	window := map[uint32]bool{}
	for _, r := range s.r.DB.RowsByState(s.fid, "inflight") {
		window[r.SrcUID] = true
	}
	if s.lastAppUID != 0 {
		window[s.lastAppUID] = true
	}
	if len(window) == 0 {
		return
	}
	inter := map[uint32]bool{}
	for u := range rec.suspects {
		if window[u] {
			inter[u] = true
		}
	}
	if len(rec.suspects) == 0 {
		inter = window
	}
	if len(inter) > 0 {
		rec.losses++
		rec.suspects = inter
	} else {
		rec.losses = 1 // progress moved on — flaky transport, fresh window
		rec.suspects = window
	}
	// every suspect already rejected in an earlier pass? the server is
	// killing the line on messages it already refuses — isolate NOW.
	allRejected := len(inter) > 0
	for u := range inter {
		if s.rec.noCounts[u] < 1 {
			allRejected = false
			break
		}
	}
	if len(rec.ladder) > 0 {
		return // isolation already in progress
	}
	if rec.losses >= s.r.Cfg.BatchAttempts || (allRejected && rec.losses >= 1) {
		var suspects []uint32
		for u := range rec.suspects {
			suspects = append(suspects, u)
		}
		sortU32(suspects)
		prime := s.lastAppUID
		if rec.suspects[prime] && len(suspects) > 1 {
			rest := make([]uint32, 0, len(suspects)-1)
			for _, u := range suspects {
				if u != prime {
					rest = append(rest, u)
				}
			}
			rec.ladder = append(rec.ladder,
				&ladderEntry{uids: []uint32{prime}}, &ladderEntry{uids: rest})
		} else {
			rec.ladder = append(rec.ladder, &ladderEntry{uids: suspects})
		}
		s.announceRecovery(len(suspects))
		s.hist("Batch isolation", "OK",
			fmt.Sprintf("analysing failed batch (%d message(s))", len(suspects)))
	}
}

// isolationPhase works the ladder: batches get BatchAttempts tries; a batch
// that keeps killing the connection is split in half (poison hints jump
// straight to singles); a single that still fails is recorded permanently
// and the migration moves on.
func (s *folderSyncer) isolationPhase(out *FolderOutcome) error {
	db := s.r.DB
	rec := s.rec
	for len(rec.ladder) > 0 {
		if s.stopped() {
			return errStopRun
		}
		entry := rec.ladder[0]
		want := map[uint32]bool{}
		for _, u := range entry.uids {
			want[u] = true
		}
		pool := map[uint32]state.MsgRow{}
		for _, r := range db.RowsByState(s.fid, "planned") {
			if want[r.SrcUID] {
				pool[r.SrcUID] = r
			}
		}
		// NO-failed rows keep their intent state — include them
		for _, r := range db.RowsByState(s.fid, "inflight") {
			if want[r.SrcUID] {
				if _, ok := pool[r.SrcUID]; !ok {
					pool[r.SrcUID] = r
				}
			}
		}
		var rows []state.MsgRow
		for _, u := range entry.uids {
			if r, ok := pool[u]; ok {
				rows = append(rows, r)
			}
		}
		if len(rows) == 0 {
			rec.ladder = rec.ladder[1:]
			continue
		}
		s.op(fmt.Sprintf("RECOVERY isolate %d msg(s)", len(rows)))
		failures, err := s.transferPass(rows, true, out)
		if err != nil {
			rec.isoLosses++
			if s.lossPhase != "append" {
				return err // source/transport loss: never message evidence
			}
			entry.tries++
			if len(rows) == 1 {
				if entry.tries >= s.r.Cfg.BatchAttempts {
					s.permanentFail(rows[0], "CONNECTION_RESET",
						fmt.Sprintf("server drops the connection on this message (%d attempts)",
							entry.tries), out)
					rec.ladder = rec.ladder[1:]
				}
			} else if entry.tries >= s.r.Cfg.BatchAttempts {
				half := len(entry.uids) / 2
				if half < 1 {
					half = 1
				}
				a, b := entry.uids[:half], entry.uids[half:]
				rec.ladder = append([]*ladderEntry{{uids: a}, {uids: b}}, rec.ladder[1:]...)
				s.hist("Batch isolation", "OK",
					fmt.Sprintf("reduced batch to %d message(s)", half))
				s.logf("isolation: splitting batch of %d into %d + %d",
					len(entry.uids), half, len(entry.uids)-half)
			} else if pu := s.lastAppUID; want[pu] && len(entry.uids) > 1 {
				// probe the append that killed the line individually first
				rest := make([]uint32, 0, len(entry.uids)-1)
				for _, u := range entry.uids {
					if u != pu {
						rest = append(rest, u)
					}
				}
				rec.ladder = append([]*ladderEntry{{uids: []uint32{pu}},
					{uids: rest, tries: entry.tries}}, rec.ladder[1:]...)
				s.hist("Batch isolation", "OK",
					fmt.Sprintf("suspect UID %d probed individually", pu))
			}
			return err // reconnect; the ladder is persistent
		}
		s.notePass(rows, failures)
		rec.ladder = rec.ladder[1:]
		for _, f := range failures {
			u := f.row.SrcUID
			if len(rows) == 1 || s.rec.noCounts[u] >= s.r.Cfg.MsgRetries {
				s.permanentFail(f.row, classifyFailure(f.err), f.err, out)
			} else {
				rec.ladder = append(rec.ladder, &ladderEntry{uids: []uint32{u}})
			}
		}
	}
	rec.losses = 0
	rec.suspects = map[uint32]bool{}
	if rec.announced {
		rec.announced = false
		s.logf("Recovery Mode complete — continuing migration")
		s.hist("Migration resumed", "OK",
			fmt.Sprintf("continuing remaining messages (%d failed message(s) recorded)",
				out.Failed))
	}
	return nil
}

// permanentFail records one message in the Failed Message Registry and
// moves on. The mailbox migration NEVER stops for a bad message.
func (s *folderSyncer) permanentFail(row state.MsgRow, ftype, reason string,
	out *FolderOutcome) {
	uid := row.SrcUID
	msgID, subj, sender, date := "", "", "", row.InternalDate
	if metas, err := s.src.UIDFetchMeta(fmt.Sprintf("%d", uid), true); err == nil && len(metas) > 0 {
		h := metas[0].Header
		msgID = hdrField(h, "Message-ID")
		subj = hdrField(h, "Subject")
		sender = hdrField(h, "From")
		if d := hdrField(h, "Date"); d != "" {
			date = d
		}
	}
	s.r.DB.RecordFailed(s.r.MID, s.plan.SrcDisplay, uid, msgID, subj, sender,
		date, row.Size, ftype, reason)
	s.r.DB.MarkState(s.fid, []uint32{uid}, "failed", trunc(reason, 200))
	s.r.MB.Add(func(m *MBValues) { m.FailedMsgs++ })
	out.Failed++
	delete(s.rec.noCounts, uid)
	s.logf("Recovery Mode — message recorded in the Failed Message Registry:\n"+
		"    Folder: %s\n    UID: %d\n    Message-ID: %s\n    Subject: %s\n"+
		"    From: %s\n    Size: %s\n    Failure: %s — %s\n"+
		"    Continuing mailbox migration...",
		s.plan.SrcDisplay, uid, orDash(msgID), orDash(subj), orDash(sender),
		util.FmtBytes(float64(row.Size)), ftype, trunc(reason, 160))
	s.hist("Failed message isolated", "WARN",
		fmt.Sprintf("UID %d · %s%s", uid, ftype,
			map[bool]string{true: " · " + trunc(subj, 40), false: ""}[subj != ""]))
	s.hist("Message recorded", "WARN",
		"continuing migration (recorded in the Failed Message Registry)")
	if s.r.Bus != nil {
		s.r.Bus.Log("ERROR", s.r.Spec.Label(),
			fmt.Sprintf("[%s] UID %d failed permanently: %s — %s",
				s.plan.SrcDisplay, uid, ftype, trunc(reason, 120)))
	}
}

// markRecoveries: a previously failed message that has now been copied is
// RECOVERED (keeps its historical record).
func (s *folderSyncer) markRecoveries() {
	if len(s.registry) == 0 || len(s.copied) == 0 {
		return
	}
	for _, uid := range s.copied {
		switch s.registry[uid] {
		case "FAILED", "RETRY_PENDING", "RETRYING":
			s.r.DB.MarkRecovered(s.r.MID, s.plan.SrcDisplay, uid)
			s.logf("previously failed message uid %d successfully migrated — "+
				"status updated to RECOVERED", uid)
			s.hist("Failed message recovered", "OK",
				fmt.Sprintf("UID %d migrated on retry — status RECOVERED", uid))
		}
	}
}

func hdrField(header []byte, name string) string {
	want := lower(name) + ":"
	var lines [][]byte
	for _, l := range splitLines(header) {
		lines = append(lines, l)
	}
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if len(l) < len(want) || lower(string(l[:len(want)])) != want {
			continue
		}
		val := string(l[len(want):])
		for i+1 < len(lines) && len(lines[i+1]) > 0 &&
			(lines[i+1][0] == ' ' || lines[i+1][0] == '\t') {
			i++
			val += " " + string(trimLeftWS(lines[i]))
		}
		return trunc(trimWS(val), 300)
	}
	return ""
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			end := i
			if end > start && b[end-1] == '\r' {
				end--
			}
			out = append(out, b[start:end])
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

func trimLeftWS(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}

func trimWS(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func sortU32(a []uint32) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
