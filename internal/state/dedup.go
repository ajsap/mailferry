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

package state

// Interruption-safe progress for `mailferry dedup --execute`. The
// dedup_state table (created additively by the schema) records, per
// destination mailbox+folder+duplicate UID, which quarantine action has
// already completed so a re-run skips finished work — no message is ever
// quarantined twice, and totals stay exact across a cancel+resume.

// DedupPlanned pre-records a duplicate that is about to be quarantined
// (done=0). Idempotent: re-planning an already-recorded pair leaves an
// existing done=1 row untouched, so a resume never reopens finished work.
func (d *DB) DedupPlanned(mid int64, folder string, dupUID, keeperUID int64, action string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.Exec("INSERT OR IGNORE INTO dedup_state"+
		"(mailbox_id,folder,duplicate_uid,keeper_uid,action,done) VALUES(?,?,?,?,?,0)",
		mid, folder, dupUID, keeperUID, action)
}

// DedupDone marks a duplicate quarantined (done=1) after the copy/move and
// the reversible \Deleted flag (if any) have both committed on the server.
func (d *DB) DedupDone(mid int64, folder string, dupUID, keeperUID int64, action string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.Exec("INSERT INTO dedup_state"+
		"(mailbox_id,folder,duplicate_uid,keeper_uid,action,done) VALUES(?,?,?,?,?,1) "+
		"ON CONFLICT(mailbox_id,folder,duplicate_uid) DO UPDATE SET "+
		"keeper_uid=excluded.keeper_uid, action=excluded.action, done=1",
		mid, folder, dupUID, keeperUID, action)
}

// DedupIsDone reports whether this duplicate UID was already quarantined in
// a previous (possibly interrupted) run — the resume-skip predicate.
func (d *DB) DedupIsDone(mid int64, folder string, dupUID int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	var done int64
	err := d.con.QueryRow("SELECT done FROM dedup_state "+
		"WHERE mailbox_id=? AND folder=? AND duplicate_uid=?", mid, folder, dupUID).Scan(&done)
	return err == nil && done == 1
}

// DedupDoneCount returns how many duplicates have been quarantined for a
// mailbox (used by tests and the resume summary).
func (d *DB) DedupDoneCount(mid int64) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int64
	d.con.QueryRow("SELECT COUNT(*) FROM dedup_state WHERE mailbox_id=? AND done=1", mid).Scan(&n)
	return n
}

// IsMailboxLeased reports whether a mailbox key currently carries a FRESH
// lease held by a live worker (heartbeat within LeaseFresh). dedup uses this
// to skip mailboxes a migration is actively touching — it never takes a
// lease itself. Returns (busy, owner). A mailbox with no row is not busy.
func (d *DB) IsMailboxLeased(key string) (bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var owner string
	var ts float64
	err := d.con.QueryRow("SELECT lease_owner, lease_ts FROM mailboxes WHERE key=?", key).
		Scan(&owner, &ts)
	if err != nil || owner == "" || ts <= 0 {
		return false, ""
	}
	if now()-ts >= d.LeaseFresh {
		return false, owner // stale lease: a dead worker, not active
	}
	return true, owner
}
