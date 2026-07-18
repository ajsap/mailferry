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

// Maintenance operations: compact, wrapper-state import and the small
// read-only helpers used by the verify / failed / benchmark commands.
package state

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// Compact removes per-message rows for folders recorded DONE (aggregates
// are kept) and VACUUMs the file. Mirrors the Python engine's `compact`.
func (d *DB) Compact() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.con.Exec("DELETE FROM messages WHERE state='done' AND folder_id IN " +
		"(SELECT id FROM folders WHERE status='DONE')")
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	d.con.Exec("VACUUM")
	return n
}

// ImportWrapperState imports the old wrapper's migration.state (JSONL,
// mailbox-granular; the wrapper's historical field names oldhost/olduser
// etc. are part of THAT format and are intentionally preserved here):
// every {"type":"result","status":"SUCCESS"} record
// becomes a SUCCESS mailbox row, so those mailboxes get a cheap incremental
// pass (or are skipped with --skip-completed).
func (d *DB) ImportWrapperState(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec["type"] != "result" || rec["status"] != "SUCCESS" {
			continue
		}
		gs := func(k string) string {
			if s, ok := rec[k].(string); ok {
				return s
			}
			return ""
		}
		key := gs("key")
		if key == "" {
			key = strings.Join([]string{gs("oldhost"), gs("olduser"),
				gs("newhost"), gs("newuser")}, "\x1f")
		}
		d.con.Exec("INSERT OR IGNORE INTO mailboxes(key,src_host,src_user,dst_host,dst_user) "+
			"VALUES(?,?,?,?,?)", key, gs("oldhost"), gs("olduser"), gs("newhost"), gs("newuser"))
		d.con.Exec("UPDATE mailboxes SET status='SUCCESS', last_error='' WHERE key=?", key)
		n++
	}
	return n, sc.Err()
}

// FolderInfo is a per-folder state summary for the verify command.
type FolderInfo struct {
	SrcName  string
	DstName  string
	MsgsDone int64
	Status   string
}

// FoldersOf lists the folders recorded for a mailbox key ("" src user
// matches are resolved by MailboxIDByUser).
func (d *DB) FoldersOf(mid int64) []FolderInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.con.Query("SELECT src_name, dst_name, COALESCE(msgs_done,0), "+
		"COALESCE(status,'') FROM folders WHERE mailbox_id=?", mid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []FolderInfo
	for rows.Next() {
		var fi FolderInfo
		if rows.Scan(&fi.SrcName, &fi.DstName, &fi.MsgsDone, &fi.Status) == nil {
			out = append(out, fi)
		}
	}
	return out
}

// MailboxIDByKey returns the mailbox row id for a CSV spec key, or 0.
func (d *DB) MailboxIDByKey(key string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	var id int64
	d.con.QueryRow("SELECT id FROM mailboxes WHERE key=?", key).Scan(&id)
	return id
}

// MailboxIDByUser resolves --mailbox USER to a row id: 0 = no filter given,
// -1 = not found.
func (d *DB) MailboxIDByUser(user string) int64 {
	if user == "" {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var id int64
	err := d.con.QueryRow("SELECT id FROM mailboxes WHERE src_user=? OR key LIKE ?",
		user, "%"+user+"%").Scan(&id)
	if err != nil {
		return -1
	}
	return id
}

// UpdateFlags records the source flags now mirrored on the destination
// (--sync-flags backup mode).
func (d *DB) UpdateFlags(fid int64, srcUID uint32, flags string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.Exec("UPDATE messages SET flags=? WHERE folder_id=? AND src_uid=?",
		flags, fid, srcUID)
}

// WorkerRun returns the run id a worker registered with ("" if unknown).
func (d *DB) WorkerRun(owner string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var run string
	d.con.QueryRow("SELECT COALESCE(run_id,'') FROM workers WHERE id=?", owner).Scan(&run)
	return run
}

// SizeOfKey returns bytes_total recorded for a mailbox key (0 if unknown) —
// used by --order size admission.
func (d *DB) SizeOfKey(key string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int64
	d.con.QueryRow("SELECT COALESCE(bytes_total,0) FROM mailboxes WHERE key=?", key).Scan(&n)
	return n
}
