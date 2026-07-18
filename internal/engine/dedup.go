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

// Destination-only deduplication (`mailferry dedup`). SAFE BY DESIGN:
//
//   - The Source Server is never contacted; only each CSV row's DESTINATION.
//   - Two messages are duplicates ONLY when all three of a strong, multi-
//     factor key agree, WITHIN the same folder:
//       1. a non-empty, normalised Message-ID (empty MID is never grouped),
//       2. the exact RFC822.SIZE, and
//       3. the Python-compatible util fingerprint (which folds in
//          Date/From/To/Subject when the MID is absent).
//     Anything weaker is retained — uncertain matches always favour keeping
//     mail. The canonical keeper is the LOWEST UID in each group.
//   - Analysis is the default (a dry run): it reports and writes a CSV, and
//     mutates nothing.
//   - Execution is reversible: duplicates are MOVED to a quarantine folder
//     (true IMAP UID MOVE when the server offers MOVE, otherwise UID COPY +
//     \Deleted WITHOUT expunge). Permanent deletion is deliberately NOT
//     implemented in v2.0.0.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/imapx"
	"github.com/ajsap/mailferry/v2/internal/state"
	"github.com/ajsap/mailferry/v2/internal/util"
)

// QuarantineRoot is the top-level destination folder duplicates are moved
// under; the original folder name is appended: "MailFerry-Quarantine/<folder>".
const QuarantineRoot = "MailFerry-Quarantine"

var msgidNorm = strings.NewReplacer("<", "", ">", "", " ", "", "\t", "", "\r", "", "\n", "")

// normaliseMID strips angle brackets and whitespace from a Message-ID so
// cosmetic differences never split a genuine duplicate group. Empty in →
// empty out (an empty MID can never anchor a group).
func normaliseMID(raw string) string {
	return msgidNorm.Replace(strings.TrimSpace(raw))
}

// DupMessage is one destination message considered for deduplication.
type DupMessage struct {
	UID       int64
	Size      int64
	MessageID string // normalised (may be "")
	Subject   string
	FP        string // util fingerprint
}

// DupGroup is a set of ≥2 messages proven duplicate within one folder.
type DupGroup struct {
	Folder    string
	KeeperUID int64 // lowest UID — always retained
	Dups      []DupMessage
}

// FolderReport summarises one destination folder.
type FolderReport struct {
	Folder   string
	Messages int
	Groups   []DupGroup
	DupCount int   // messages that would be quarantined
	DupBytes int64 // reclaimable bytes (duplicates only; keeper kept)
}

// DedupReport is the whole-mailbox analysis result.
type DedupReport struct {
	Mailbox     string // dst label
	Skipped     bool   // mailbox busy: not analysed
	SkipReason  string
	Folders     []FolderReport
	TotalGroups int
	TotalDups   int
	TotalBytes  int64
	TotalMsgs   int
	// Execution accounting (zero in analysis mode).
	Quarantined int
	AlreadyDone int
	UsedMove    bool
}

// DedupOptions controls one dedup pass.
type DedupOptions struct {
	Execute   bool // false = analysis only (default, a dry run)
	TLSVerify bool
	Timeout   float64 // seconds
}

// subjectOf pulls a short Subject from the fetched header for the report.
func subjectOf(header []byte) string {
	for _, sep := range [][]byte{[]byte("\r\n"), []byte("\n")} {
		for _, line := range headerLines(header, sep) {
			low := strings.ToLower(line)
			if strings.HasPrefix(low, "subject:") {
				return strings.TrimSpace(line[len("subject:"):])
			}
		}
		if len(headerLines(header, sep)) > 1 {
			break
		}
	}
	return ""
}

func headerLines(b []byte, sep []byte) []string {
	return strings.Split(string(b), string(sep))
}

func midOf(header []byte) string {
	for _, sep := range [][]byte{[]byte("\r\n"), []byte("\n")} {
		lines := headerLines(header, sep)
		for _, line := range lines {
			low := strings.ToLower(line)
			if strings.HasPrefix(low, "message-id:") {
				return normaliseMID(line[len("message-id:"):])
			}
		}
		if len(lines) > 1 {
			break
		}
	}
	return ""
}

// groupFolder builds the duplicate groups for one folder's messages under
// the strong three-factor rule. A group forms ONLY among messages that share
// a non-empty normalised Message-ID AND the exact size AND the fingerprint.
func groupFolder(folder string, msgs []DupMessage) FolderReport {
	fr := FolderReport{Folder: folder, Messages: len(msgs)}
	// Composite key: only messages with a non-empty MID are eligible. This
	// guarantees empty-MID messages are NEVER grouped by MID, and the size +
	// fingerprint terms keep near-misses apart.
	type key struct {
		mid  string
		size int64
		fp   string
	}
	buckets := map[key][]DupMessage{}
	for _, m := range msgs {
		if m.MessageID == "" {
			continue // no MID → never a duplicate; retained
		}
		k := key{m.MessageID, m.Size, m.FP}
		buckets[k] = append(buckets[k], m)
	}
	// Deterministic group order for a stable report/CSV.
	var keys []key
	for k, v := range buckets {
		if len(v) >= 2 {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].size != keys[j].size {
			return keys[i].size < keys[j].size
		}
		return keys[i].mid < keys[j].mid
	})
	for _, k := range keys {
		grp := buckets[k]
		sort.Slice(grp, func(i, j int) bool { return grp[i].UID < grp[j].UID })
		g := DupGroup{Folder: folder, KeeperUID: grp[0].UID, Dups: grp[1:]}
		fr.Groups = append(fr.Groups, g)
		for _, d := range g.Dups {
			fr.DupCount++
			fr.DupBytes += d.Size
		}
	}
	return fr
}

// dstFolders lists the selectable folders on the destination (display names),
// skipping \Noselect / \NonExistent containers.
func dstFolders(dst *imapx.Client) ([]string, error) {
	entries, err := dst.ListAll()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		noselect := false
		for _, a := range e.Attrs {
			up := strings.ToUpper(a)
			if up == "\\NOSELECT" || up == "\\NONEXISTENT" {
				noselect = true
			}
		}
		if noselect {
			continue
		}
		out = append(out, imapx.DecodeMUTF7(e.Wire))
	}
	return out, nil
}

// Dedup analyses (and optionally quarantines) duplicates on ONE spec's
// destination. db may be nil for analysis; it is required for --execute
// (resume/skip bookkeeping). logf is optional.
func Dedup(ctx context.Context, spec config.MailboxSpec, opt DedupOptions,
	db *state.DB, mid int64, logf func(string)) (DedupReport, error) {
	if logf == nil {
		logf = func(string) {}
	}
	rep := DedupReport{Mailbox: spec.Dst.Label()}
	to := time.Duration(opt.Timeout * float64(time.Second))
	if to <= 0 {
		to = 60 * time.Second
	}
	dst := imapx.NewClient(imapx.Endpoint(spec.Dst), to, opt.TLSVerify, nil,
		spec.Dst.Label(), func(s string) { logf("dst: " + s) })
	// Belt-and-suspenders for the safe-by-design dry run: in analysis mode the
	// client is put in ReadOnly, so every mutating verb is blocked at the
	// protocol choke point — a bug could not leak a write even in principle.
	dst.ReadOnly = !opt.Execute
	if err := dst.Connect(); err != nil {
		return rep, fmt.Errorf("destination connect: %w", err)
	}
	defer dst.Logout(5 * time.Second)
	if err := dst.Login(); err != nil {
		return rep, fmt.Errorf("destination login: %w", err)
	}
	rep.UsedMove = opt.Execute && dst.HasMove()

	folders, err := dstFolders(dst)
	if err != nil {
		return rep, fmt.Errorf("list destination folders: %w", err)
	}
	// Never scan our own quarantine tree.
	var scan []string
	for _, f := range folders {
		if f == QuarantineRoot || strings.HasPrefix(f, QuarantineRoot+"/") {
			continue
		}
		scan = append(scan, f)
	}
	sort.Strings(scan)

	for _, folder := range scan {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		fr, groups, err := analyseFolder(dst, folder)
		if err != nil {
			logf(fmt.Sprintf("dedup: folder %q skipped (%v)", folder, err))
			continue
		}
		rep.TotalMsgs += fr.Messages
		if len(groups) == 0 {
			continue
		}
		rep.Folders = append(rep.Folders, fr)
		rep.TotalGroups += len(fr.Groups)
		rep.TotalDups += fr.DupCount
		rep.TotalBytes += fr.DupBytes

		if opt.Execute {
			if err := quarantineFolder(ctx, dst, db, mid, folder, groups, rep.UsedMove, logf, &rep); err != nil {
				return rep, err
			}
		}
	}
	return rep, nil
}

// analyseFolder selects a folder read-only, fetches per-message metadata and
// groups duplicates. EXAMINE (read-only) is used so analysis cannot mutate.
func analyseFolder(dst *imapx.Client, folder string) (FolderReport, []DupGroup, error) {
	wire := imapx.EncodeMUTF7(folder)
	si, err := dst.Select(wire, true) // EXAMINE — read-only
	if err != nil {
		return FolderReport{Folder: folder}, nil, err
	}
	if si.Exists == 0 {
		return FolderReport{Folder: folder}, nil, nil
	}
	uids, err := dst.UIDSearchAll()
	if err != nil {
		return FolderReport{Folder: folder}, nil, err
	}
	if len(uids) == 0 {
		return FolderReport{Folder: folder}, nil, nil
	}
	var msgs []DupMessage
	for _, chunk := range util.SetStrings(util.ToIntervals(uids)) {
		metas, err := dst.UIDFetchMeta(chunk, true)
		if err != nil {
			return FolderReport{Folder: folder}, nil, err
		}
		for _, m := range metas {
			// A message already flagged \Deleted is pending removal — treat it
			// as effectively quarantined and never re-group it. This is what
			// makes a re-run of analysis after a copy+flag execute report zero
			// remaining duplicates, and prevents re-quarantining flagged mail.
			if hasDeletedFlag(m.Flags) {
				continue
			}
			msgs = append(msgs, DupMessage{
				UID:       int64(m.UID),
				Size:      m.Size,
				MessageID: midOf(m.Header),
				Subject:   subjectOf(m.Header),
				FP:        util.FingerprintFromHeaders(m.Header, m.Size),
			})
		}
	}
	fr := groupFolder(folder, msgs)
	return fr, fr.Groups, nil
}

// hasDeletedFlag reports whether the IMAP \Deleted flag is set.
func hasDeletedFlag(flags []string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, `\Deleted`) {
			return true
		}
	}
	return false
}

// quarantineFolder performs the reversible relocation for one folder's
// duplicate groups. It is resumable: rows already recorded done in
// dedup_state are skipped, so a cancel+rerun never double-quarantines and
// keeps totals exact. The keeper (lowest UID) is never touched.
func quarantineFolder(ctx context.Context, dst *imapx.Client, db *state.DB, mid int64,
	folder string, groups []DupGroup, useMove bool, logf func(string), rep *DedupReport) error {
	quarName := QuarantineRoot + "/" + folder
	quarWire := imapx.EncodeMUTF7(quarName)
	// The source folder must be writable for COPY/MOVE/STORE.
	folderWire := imapx.EncodeMUTF7(folder)
	if _, err := dst.Select(folderWire, false); err != nil {
		return fmt.Errorf("select %q for quarantine: %w", folder, err)
	}
	created := false
	ensureQuar := func() error {
		if created {
			return nil
		}
		if err := dst.Create(quarWire); err != nil {
			return fmt.Errorf("create quarantine %q: %w", quarName, err)
		}
		created = true
		return nil
	}
	// Interruption safety: cancellation is only observed BETWEEN complete
	// per-duplicate operations (the ctx check at the top of each iteration). A
	// ctx cancel never aborts an in-flight COPY/MOVE/STORE, so the copy+flag
	// (or move) for one duplicate always finishes atomically before the next
	// check — a resume then skips done rows and never double-quarantines. The
	// only residual edge is a genuine connection drop between COPY and the
	// done marker, whose worst case is ONE redundant copy in the reversible
	// quarantine folder — never lost mail, and the keeper is always retained.
	for _, g := range groups {
		for _, d := range g.Dups {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			action := "copy+flag"
			if useMove {
				action = "move"
			}
			if db != nil && db.DedupIsDone(mid, folder, d.UID) {
				rep.AlreadyDone++
				continue // resumed: already quarantined in a prior run
			}
			if err := ensureQuar(); err != nil {
				return err
			}
			if db != nil {
				db.DedupPlanned(mid, folder, d.UID, g.KeeperUID, action)
			}
			if useMove {
				if err := dst.UIDMove(d.UID, quarWire); err != nil {
					return fmt.Errorf("UID MOVE %d -> %q: %w", d.UID, quarName, err)
				}
			} else {
				if err := dst.UIDCopy(d.UID, quarWire); err != nil {
					return fmt.Errorf("UID COPY %d -> %q: %w", d.UID, quarName, err)
				}
				// Reversible: flag \Deleted WITHOUT expunge. The original
				// stays on the server; the operator compacts when satisfied.
				if err := dst.FlagDeleted(d.UID); err != nil {
					return fmt.Errorf("flag \\Deleted UID %d: %w", d.UID, err)
				}
			}
			if db != nil {
				db.DedupDone(mid, folder, d.UID, g.KeeperUID, action)
			}
			rep.Quarantined++
			logf(fmt.Sprintf("dedup: quarantined UID %d (keeper %d) in %q via %s",
				d.UID, g.KeeperUID, folder, action))
		}
	}
	return nil
}
