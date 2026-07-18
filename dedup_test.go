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

// Destination deduplication tests. The dedup engine is SAFE BY DESIGN, so
// these prove: genuine duplicates group correctly (keeper = lowest UID);
// near-misses (same MID/different size, same size/different MID, empty-MID
// with matching or differing fingerprint) are NEVER grouped; analysis mutates
// nothing (append/copy/move/expunge counters unchanged); --execute quarantines
// reversibly (copy or move, originals only flagged \Deleted, EXPUNGE never
// issued); a mid-execute cancel resumes without double-quarantining and with
// exact totals; and a post-execute analysis finds zero remaining duplicates.
package mailferry_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/engine"
	"github.com/ajsap/mailferry/v2/internal/fakeimap"
	"github.com/ajsap/mailferry/v2/internal/state"
)

// dupBody builds a message with an explicit Message-ID (empty mid → no
// Message-ID header at all) and pad bytes to tune the exact RFC822.SIZE.
func dupBody(mid, subject string, pad int) []byte {
	midLine := ""
	if mid != "" {
		midLine = "Message-ID: <" + mid + ">\r\n"
	}
	return []byte(fmt.Sprintf("%sFrom: alice@example.test\r\nTo: bob@example.test\r\n"+
		"Subject: %s\r\nDate: Thu, 16 Jul 2026 10:00:00 +0000\r\n\r\n"+
		"Body.%s", midLine, subject, strings.Repeat("X", pad)))
}

// seedDedupDst builds a destination account with a controlled mix of genuine
// duplicates and deliberate near-misses in INBOX, and a clean Sent folder.
// Returns the number of messages that are true duplicates (should quarantine).
func seedDedupDst() (*fakeimap.Account, int) {
	a := fakeimap.NewAccount("bob", "pw2")
	in := a.Folder("INBOX")

	// --- Genuine duplicate PAIR: identical MID + size + content. -----------
	dupA := dupBody("dup-1@x", "Invoice", 100)
	in.Add(dupA, []string{`\Seen`}, "16-Jul-2026 10:00:00 +0000") // UID 1 (keeper)
	in.Add(dupA, nil, "16-Jul-2026 10:00:00 +0000")               // UID 2 (dup)

	// --- Genuine duplicate TRIPLE: three identical copies. -----------------
	dupB := dupBody("dup-2@x", "Report", 250)
	in.Add(dupB, nil, "16-Jul-2026 10:00:00 +0000") // UID 3 (keeper)
	in.Add(dupB, nil, "16-Jul-2026 10:00:00 +0000") // UID 4 (dup)
	in.Add(dupB, nil, "16-Jul-2026 10:00:00 +0000") // UID 5 (dup)

	// --- Near-miss: SAME MID, DIFFERENT size → NOT a duplicate. ------------
	in.Add(dupBody("near-size@x", "Same MID diff size", 10), nil, "16-Jul-2026 10:00:00 +0000")  // UID 6
	in.Add(dupBody("near-size@x", "Same MID diff size", 999), nil, "16-Jul-2026 10:00:00 +0000") // UID 7

	// --- Near-miss: SAME size, DIFFERENT MID → NOT a duplicate. ------------
	// Give both the same pad so sizes match; different MID keeps them apart.
	in.Add(dupBody("near-mid-a@x", "S", 500), nil, "16-Jul-2026 10:00:00 +0000") // UID 8
	in.Add(dupBody("near-mid-b@x", "S", 500), nil, "16-Jul-2026 10:00:00 +0000") // UID 9

	// --- Near-miss: EMPTY MID, IDENTICAL fingerprint → NOT grouped. --------
	// (Conservative rule: an empty Message-ID can never anchor a group.)
	empSame := dupBody("", "No MID", 300)
	in.Add(empSame, nil, "16-Jul-2026 10:00:00 +0000") // UID 10
	in.Add(empSame, nil, "16-Jul-2026 10:00:00 +0000") // UID 11

	// --- Near-miss: EMPTY MID, DIFFERENT fingerprint → NOT grouped. --------
	in.Add(dupBody("", "No MID A", 40), nil, "16-Jul-2026 10:00:00 +0000") // UID 12
	in.Add(dupBody("", "No MID B", 41), nil, "16-Jul-2026 10:00:00 +0000") // UID 13

	// --- A second folder with its own duplicate pair (per-folder grouping). -
	sent := a.AddFolder(fakeimap.NewFolder("Sent", 1201, `\Sent`))
	dupC := dupBody("sent-dup@x", "Re: hi", 70)
	sent.Add(dupC, nil, "16-Jul-2026 10:00:00 +0000") // UID 1 (keeper)
	sent.Add(dupC, nil, "16-Jul-2026 10:00:00 +0000") // UID 2 (dup)

	// Duplicates that SHOULD be quarantined: UID2, UID4, UID5 (INBOX) + UID2 (Sent) = 4.
	return a, 4
}

// dedupSpec builds a spec whose DESTINATION points at dst; the source points
// at a bogus address to prove dedup never contacts it.
func dedupSpec(dst *fakeimap.Server, dstA *fakeimap.Account) config.MailboxSpec {
	return config.MailboxSpec{
		Index: 1,
		Src: config.Endpoint{Host: "127.0.0.1", Port: 1, Security: "none",
			User: "unused-source", Password: "nope"},
		Dst: config.Endpoint{Host: "127.0.0.1", Port: dst.Port(), Security: "none",
			User: dstA.User, Password: dstA.Password},
	}
}

func TestDedupAnalysisGroupsCorrectlyAndMutatesNothing(t *testing.T) {
	dstA, wantDups := seedDedupDst()
	dst := fakeimap.NewServer(dstA)
	if err := dst.Start(); err != nil {
		t.Fatal(err)
	}
	defer dst.Stop()

	before := dstA.TotalMsgs()
	spec := dedupSpec(dst, dstA)
	rep, err := engine.Dedup(context.Background(), spec,
		engine.DedupOptions{Execute: false, Timeout: 30}, nil, 0, nil)
	if err != nil {
		t.Fatalf("analysis: %v", err)
	}

	// Correct duplicate accounting.
	if rep.TotalDups != wantDups {
		t.Fatalf("analysis found %d duplicates, want %d", rep.TotalDups, wantDups)
	}
	if rep.TotalGroups != 3 { // INBOX dup pair + INBOX dup triple + Sent pair
		t.Fatalf("analysis found %d groups, want 3", rep.TotalGroups)
	}

	// Keeper is the LOWEST UID in every group, and near-misses are absent.
	byFolder := map[string]engine.FolderReport{}
	for _, fr := range rep.Folders {
		byFolder[fr.Folder] = fr
	}
	inbox, ok := byFolder["INBOX"]
	if !ok {
		t.Fatal("INBOX not reported")
	}
	// Collect keeper→dups mapping.
	keepers := map[int64][]int64{}
	for _, g := range inbox.Groups {
		var dl []int64
		for _, d := range g.Dups {
			dl = append(dl, d.UID)
		}
		keepers[g.KeeperUID] = dl
	}
	// UID 1 keeps, 2 is dup. UID 3 keeps, 4+5 are dup.
	if dl, ok := keepers[1]; !ok || len(dl) != 1 || dl[0] != 2 {
		t.Fatalf("pair group wrong: keeper 1 -> %v (want [2])", dl)
	}
	if dl, ok := keepers[3]; !ok || len(dl) != 2 || dl[0] != 4 || dl[1] != 5 {
		t.Fatalf("triple group wrong: keeper 3 -> %v (want [4 5])", dl)
	}
	// None of the near-miss UIDs (6..13) may appear as a keeper or a dup.
	for _, g := range inbox.Groups {
		for _, uid := range append([]int64{g.KeeperUID}, dupUIDsOf(g)...) {
			if uid >= 6 && uid <= 13 {
				t.Fatalf("near-miss UID %d was grouped (must be retained)", uid)
			}
		}
	}

	// ZERO mutations: append/copy/move/expunge counters all unchanged.
	if got := dstA.TotalMsgs(); got != before {
		t.Fatalf("analysis changed message count: %d -> %d", before, got)
	}
	if n := dst.AppendCount.Load(); n != 0 {
		t.Fatalf("analysis appended %d (want 0)", n)
	}
	if n := dst.CopyCount.Load(); n != 0 {
		t.Fatalf("analysis copied %d (want 0)", n)
	}
	if n := dst.MoveCount.Load(); n != 0 {
		t.Fatalf("analysis moved %d (want 0)", n)
	}
	if n := dst.ExpungeCount.Load(); n != 0 {
		t.Fatalf("analysis expunged %d (want 0)", n)
	}
	// No quarantine folder was created.
	if dstA.Folder(engine.QuarantineRoot+"/INBOX") != nil {
		t.Fatal("analysis created a quarantine folder")
	}
}

func dupUIDsOf(g engine.DupGroup) []int64 {
	var out []int64
	for _, d := range g.Dups {
		out = append(out, d.UID)
	}
	return out
}

func TestDedupExecuteQuarantinesReversiblyNoExpunge(t *testing.T) {
	dstA, wantDups := seedDedupDst()
	dst := fakeimap.NewServer(dstA)
	dst.NoMove.Store(true) // force the copy + \Deleted (reversible) path
	if err := dst.Start(); err != nil {
		t.Fatal(err)
	}
	defer dst.Stop()

	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "mailferry.db"), false, 300)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	spec := dedupSpec(dst, dstA)
	mid, _, _ := db.UpsertMailbox(spec.Key(), spec.Src.Host, spec.Src.User,
		spec.Dst.Host, spec.Dst.User)

	inboxKeepers := keeperBodies(dstA, "INBOX")
	rep, err := engine.Dedup(context.Background(), spec,
		engine.DedupOptions{Execute: true, Timeout: 30}, db, mid, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if rep.Quarantined != wantDups {
		t.Fatalf("quarantined %d, want %d", rep.Quarantined, wantDups)
	}

	// Copies landed in quarantine; MOVE was NOT used (NoMove).
	if rep.UsedMove {
		t.Fatal("UsedMove true despite NoMove server")
	}
	if n := dst.CopyCount.Load(); n != int64(wantDups) {
		t.Fatalf("UID COPY count %d, want %d", n, wantDups)
	}
	if n := dst.MoveCount.Load(); n != 0 {
		t.Fatalf("UID MOVE count %d, want 0 (server lacks MOVE)", n)
	}
	// THE reversibility guarantee: EXPUNGE was never issued.
	if n := dst.ExpungeCount.Load(); n != 0 {
		t.Fatalf("EXPUNGE issued %d time(s) — must be 0 (reversible design)", n)
	}

	// Quarantine folders exist and hold the copies.
	qIn := dstA.Folder(engine.QuarantineRoot + "/INBOX")
	if qIn == nil || len(qIn.Msgs) != 3 {
		t.Fatalf("quarantine INBOX has %v copies, want 3", folderLen(qIn))
	}
	qSent := dstA.Folder(engine.QuarantineRoot + "/Sent")
	if qSent == nil || len(qSent.Msgs) != 1 {
		t.Fatalf("quarantine Sent has %v copies, want 1", folderLen(qSent))
	}

	// Originals still present (copy, not move) and duplicates flagged \Deleted;
	// keepers untouched (no \Deleted).
	in := dstA.Folder("INBOX")
	if len(in.Msgs) != 13 {
		t.Fatalf("INBOX now has %d messages, want 13 (nothing expunged)", len(in.Msgs))
	}
	deleted := map[uint32]bool{}
	for _, m := range in.Msgs {
		if m.Flags[`\Deleted`] {
			deleted[m.UID] = true
		}
	}
	for _, uid := range []uint32{2, 4, 5} {
		if !deleted[uid] {
			t.Fatalf("duplicate UID %d not flagged \\Deleted", uid)
		}
	}
	for _, uid := range []uint32{1, 3, 6, 7, 8, 9, 10, 11, 12, 13} {
		if deleted[uid] {
			t.Fatalf("retained UID %d wrongly flagged \\Deleted", uid)
		}
	}
	// Keeper bodies are byte-identical to before (untouched).
	for body := range inboxKeepers {
		found := false
		for _, m := range in.Msgs {
			if string(m.Body) == body && !m.Flags[`\Deleted`] {
				found = true
			}
		}
		if !found {
			t.Fatal("a keeper body went missing or was flagged deleted")
		}
	}

	// dedup_state recorded every quarantined UID as done.
	if n := db.DedupDoneCount(mid); n != int64(wantDups) {
		t.Fatalf("dedup_state done count %d, want %d", n, wantDups)
	}

	// Re-running ANALYSIS after execute finds ZERO remaining duplicates: the
	// flagged-\Deleted originals are treated as pending removal and are not
	// re-grouped, and the copies live in the excluded quarantine tree.
	repA, err := engine.Dedup(context.Background(), spec,
		engine.DedupOptions{Execute: false, Timeout: 30}, nil, 0, nil)
	if err != nil {
		t.Fatalf("post-execute analysis: %v", err)
	}
	if repA.TotalDups != 0 || repA.TotalGroups != 0 {
		t.Fatalf("post-execute analysis found %d dups in %d groups, want 0/0",
			repA.TotalDups, repA.TotalGroups)
	}

	// Re-running EXECUTE is idempotent: no NEW quarantine actions, no extra
	// COPYs, and EXPUNGE still never issued.
	rep2, err := engine.Dedup(context.Background(), spec,
		engine.DedupOptions{Execute: true, Timeout: 30}, db, mid, nil)
	if err != nil {
		t.Fatalf("second execute: %v", err)
	}
	if rep2.Quarantined != 0 {
		t.Fatalf("second execute quarantined %d (want 0 — all already done or flagged)", rep2.Quarantined)
	}
	if n := dst.CopyCount.Load(); n != int64(wantDups) {
		t.Fatalf("second execute issued extra COPYs: total %d, want %d", n, wantDups)
	}
	if n := dst.ExpungeCount.Load(); n != 0 {
		t.Fatalf("second execute expunged %d (want 0)", n)
	}
}

func TestDedupExecuteUsesMoveWhenAdvertised(t *testing.T) {
	dstA, wantDups := seedDedupDst()
	dst := fakeimap.NewServer(dstA) // MOVE advertised by default
	if err := dst.Start(); err != nil {
		t.Fatal(err)
	}
	defer dst.Stop()

	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "mailferry.db"), false, 300)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	spec := dedupSpec(dst, dstA)
	mid, _, _ := db.UpsertMailbox(spec.Key(), spec.Src.Host, spec.Src.User,
		spec.Dst.Host, spec.Dst.User)

	rep, err := engine.Dedup(context.Background(), spec,
		engine.DedupOptions{Execute: true, Timeout: 30}, db, mid, nil)
	if err != nil {
		t.Fatalf("execute (move): %v", err)
	}
	if !rep.UsedMove {
		t.Fatal("UsedMove false despite MOVE-capable server")
	}
	if n := dst.MoveCount.Load(); n != int64(wantDups) {
		t.Fatalf("UID MOVE count %d, want %d", n, wantDups)
	}
	if n := dst.CopyCount.Load(); n != 0 {
		t.Fatalf("UID COPY count %d, want 0 (MOVE was available)", n)
	}
	if n := dst.ExpungeCount.Load(); n != 0 {
		t.Fatalf("EXPUNGE issued %d (want 0)", n)
	}
	// MOVE rehomes: duplicates leave the source folder, keepers remain.
	in := dstA.Folder("INBOX")
	if len(in.Msgs) != 10 { // 13 - 3 moved out
		t.Fatalf("INBOX after MOVE has %d, want 10", len(in.Msgs))
	}
	// Keepers 1 and 3 still present.
	present := map[uint32]bool{}
	for _, m := range in.Msgs {
		present[m.UID] = true
	}
	for _, uid := range []uint32{1, 3, 6, 7, 8, 9, 10, 11, 12, 13} {
		if !present[uid] {
			t.Fatalf("retained UID %d missing after MOVE", uid)
		}
	}
	for _, uid := range []uint32{2, 4, 5} {
		if present[uid] {
			t.Fatalf("duplicate UID %d still in source after MOVE", uid)
		}
	}
	if q := dstA.Folder(engine.QuarantineRoot + "/INBOX"); q == nil || len(q.Msgs) != 3 {
		t.Fatalf("quarantine INBOX has %v after MOVE, want 3", folderLen(q))
	}
}

// TestDedupInterruptResumesNoDoubleQuarantine cancels mid-execute, then
// re-runs and proves totals are exact and nothing is quarantined twice.
func TestDedupInterruptResumesNoDoubleQuarantine(t *testing.T) {
	dstA, wantDups := seedDedupDst()
	dst := fakeimap.NewServer(dstA)
	dst.NoMove.Store(true)    // copy + \Deleted path is easier to interrupt deterministically
	dst.CopyDelayMS.Store(60) // slow each COPY so the cancel lands mid-run, deterministically
	if err := dst.Start(); err != nil {
		t.Fatal(err)
	}
	defer dst.Stop()

	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "mailferry.db"), false, 300)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	spec := dedupSpec(dst, dstA)
	mid, _, _ := db.UpsertMailbox(spec.Key(), spec.Src.Host, spec.Src.User,
		spec.Dst.Host, spec.Dst.User)

	// Cancel as soon as the first quarantine COPY lands, mid-run. With a
	// per-COPY delay, the remaining duplicates are still pending when we cancel.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for dst.CopyCount.Load() < 1 {
			time.Sleep(2 * time.Millisecond)
		}
		cancel()
	}()
	_, _ = engine.Dedup(ctx, spec, engine.DedupOptions{Execute: true, Timeout: 30}, db, mid, nil)
	partial := db.DedupDoneCount(mid)
	if partial == 0 || partial >= int64(wantDups) {
		t.Fatalf("cancel did not land mid-run (done=%d of %d)", partial, wantDups)
	}
	copiesAfterCancel := dst.CopyCount.Load()
	dst.CopyDelayMS.Store(0) // resume at full speed

	// Resume: finish the remainder with no double-quarantining.
	rep, err := engine.Dedup(context.Background(), spec,
		engine.DedupOptions{Execute: true, Timeout: 30}, db, mid, nil)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	// Every duplicate ends up done exactly once.
	if n := db.DedupDoneCount(mid); n != int64(wantDups) {
		t.Fatalf("after resume dedup_state done=%d, want %d", n, wantDups)
	}
	// Total COPYs across both runs == wantDups (no UID copied twice).
	if n := dst.CopyCount.Load(); n != int64(wantDups) {
		t.Fatalf("total COPYs %d across cancel+resume, want %d (no double-quarantine)", n, wantDups)
	}
	if rep.Quarantined+int(copiesAfterCancel) != wantDups {
		t.Fatalf("resume quarantined %d + %d before = %d, want %d",
			rep.Quarantined, copiesAfterCancel, rep.Quarantined+int(copiesAfterCancel), wantDups)
	}
	if n := dst.ExpungeCount.Load(); n != 0 {
		t.Fatalf("EXPUNGE issued %d across cancel+resume (want 0)", n)
	}
	// Quarantine holds exactly wantDups copies (no duplicates of copies).
	total := folderLen(dstA.Folder(engine.QuarantineRoot+"/INBOX")) +
		folderLen(dstA.Folder(engine.QuarantineRoot+"/Sent"))
	if total != wantDups {
		t.Fatalf("quarantine holds %d copies after cancel+resume, want %d", total, wantDups)
	}
}

func keeperBodies(a *fakeimap.Account, folder string) map[string]bool {
	out := map[string]bool{}
	f := a.Folder(folder)
	if f == nil {
		return out
	}
	seen := map[string]uint32{}
	for _, m := range f.Msgs {
		if first, ok := seen[string(m.Body)]; !ok || m.UID < first {
			seen[string(m.Body)] = m.UID
		}
	}
	for body := range seen {
		out[body] = true
	}
	return out
}

func folderLen(f *fakeimap.Folder) int {
	if f == nil {
		return 0
	}
	return len(f.Msgs)
}
