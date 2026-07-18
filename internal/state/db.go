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

// Package state: the per-message State Database (SQLite, WAL). The schema
// is IDENTICAL to the Python v1.x engine — a migration.db written by
// either implementation is read and resumed by the other.
package state

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ajsap/mailferry/v2/internal/util"
	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA busy_timeout=10000;
CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT);
CREATE TABLE IF NOT EXISTS runs(
  id TEXT PRIMARY KEY, started REAL, finished REAL, argv TEXT, result TEXT);
CREATE TABLE IF NOT EXISTS mailboxes(
  id INTEGER PRIMARY KEY, key TEXT UNIQUE,
  src_host TEXT, src_user TEXT, dst_host TEXT, dst_user TEXT,
  status TEXT DEFAULT 'NEW', attempts INTEGER DEFAULT 0, last_error TEXT DEFAULT '',
  lease_ts REAL DEFAULT 0, lease_owner TEXT DEFAULT '',
  msgs_total INTEGER DEFAULT 0, msgs_done INTEGER DEFAULT 0,
  bytes_total INTEGER DEFAULT 0, bytes_done INTEGER DEFAULT 0,
  updated REAL DEFAULT 0);
CREATE TABLE IF NOT EXISTS folders(
  id INTEGER PRIMARY KEY, mailbox_id INTEGER NOT NULL,
  src_name TEXT NOT NULL, dst_name TEXT DEFAULT '',
  uv_src INTEGER DEFAULT 0, uv_dst INTEGER DEFAULT 0,
  last_uidnext_src INTEGER DEFAULT 0, last_uidnext_dst INTEGER DEFAULT 0,
  highestmodseq INTEGER DEFAULT 0, adopt_done INTEGER DEFAULT 0,
  msgs_total INTEGER DEFAULT 0, bytes_total INTEGER DEFAULT 0,
  msgs_done INTEGER DEFAULT 0, bytes_done INTEGER DEFAULT 0,
  status TEXT DEFAULT 'PENDING', last_error TEXT DEFAULT '',
  UNIQUE(mailbox_id, src_name));
CREATE TABLE IF NOT EXISTS messages(
  folder_id INTEGER NOT NULL, src_uid INTEGER NOT NULL,
  dst_uid INTEGER, size INTEGER DEFAULT 0, flags TEXT DEFAULT '',
  internaldate TEXT DEFAULT '', fp TEXT, state TEXT DEFAULT 'planned',
  origin TEXT DEFAULT '', error TEXT DEFAULT '',
  PRIMARY KEY(folder_id, src_uid)) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS ix_msg_state ON messages(folder_id, state);
CREATE TABLE IF NOT EXISTS events(
  ts REAL, mailbox_id INTEGER, folder_id INTEGER, kind TEXT, detail TEXT);
CREATE TABLE IF NOT EXISTS workers(
  id TEXT PRIMARY KEY, host TEXT, pid INTEGER, run_id TEXT,
  started REAL DEFAULT 0, heartbeat REAL DEFAULT 0,
  status TEXT DEFAULT 'active');
CREATE TABLE IF NOT EXISTS failed_messages(
  id INTEGER PRIMARY KEY,
  mailbox_id INTEGER NOT NULL, folder TEXT NOT NULL, src_uid INTEGER NOT NULL,
  message_id TEXT DEFAULT '', subject TEXT DEFAULT '',
  sender TEXT DEFAULT '', date TEXT DEFAULT '', size INTEGER DEFAULT 0,
  ftype TEXT DEFAULT 'UNKNOWN', reason TEXT DEFAULT '',
  first_ts REAL DEFAULT 0, last_ts REAL DEFAULT 0,
  fail_count INTEGER DEFAULT 0, recovered_ts REAL DEFAULT 0,
  status TEXT DEFAULT 'FAILED',
  UNIQUE(mailbox_id, folder, src_uid));
CREATE TABLE IF NOT EXISTS run_ranges(
  run_id TEXT PRIMARY KEY, from_utc TEXT DEFAULT '', to_utc TEXT DEFAULT '',
  tz TEXT DEFAULT '');
CREATE TABLE IF NOT EXISTS dedup_state(
  mailbox_id INTEGER, folder TEXT, duplicate_uid INTEGER,
  keeper_uid INTEGER, action TEXT, done INTEGER,
  PRIMARY KEY(mailbox_id, folder, duplicate_uid));
`

// LeaseOwnerID: unique Worker ID hostname:pid:uuid.
func LeaseOwnerID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s:%d:%08x", host, os.Getpid(), time.Now().UnixNano()&0xffffffff)
}

// ShortWorker: compact host:pid display form.
func ShortWorker(owner string) string {
	bits := strings.Split(owner, ":")
	host, pid := "?", "?"
	if len(bits) > 0 && bits[0] != "" {
		host = strings.Split(bits[0], ".")[0]
	}
	if len(bits) > 1 {
		pid = bits[1]
	}
	return host + ":" + pid
}

type MsgRow struct {
	SrcUID       uint32
	DstUID       int64 // 0 = NULL
	Size         int64
	Flags        string
	InternalDate string
	FP           string
	State        string
}

type DB struct {
	mu         sync.Mutex
	con        *sql.DB
	Path       string
	LeaseFresh float64
}

func Open(path string, ephemeral bool, leaseFresh float64) (*DB, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)"
	if ephemeral {
		dsn = ":memory:"
	}
	con, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	con.SetMaxOpenConns(1) // single connection: serialised, WAL-safe
	if _, err := con.Exec(schema); err != nil {
		con.Close()
		return nil, fmt.Errorf("state database init: %w", err)
	}
	con.Exec("INSERT OR IGNORE INTO meta(k,v) VALUES('schema_version','1')")
	if leaseFresh < 30 {
		leaseFresh = 300
	}
	return &DB{con: con, Path: dsn, LeaseFresh: leaseFresh}, nil
}

func (d *DB) Close() { d.con.Close() }

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// ---------------------------------------------------------------- runs --

func (d *DB) StartRun(runID, argv string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.Exec("INSERT OR REPLACE INTO runs(id,started,argv) VALUES(?,?,?)", runID, now(), argv)
}

func (d *DB) EndRun(runID, result string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.Exec("UPDATE runs SET finished=?, result=? WHERE id=?", now(), result, runID)
}

// rangeKey is the fixed row key under which the ISO 8601 date-range window is
// stored. It is DB-WIDE, not per-invocation: a State Database represents one
// migration, and the window must survive across resume invocations (each of
// which gets a fresh RunID). Keying on a constant makes LoadRange find the
// window a later resume must honour, regardless of the resume's RunID.
const rangeKey = "__migration__"

// SaveRange persists the resolved ISO 8601 date-range window for this State
// Database ONCE. INSERT OR IGNORE means the first write wins — a resume never
// overwrites the window it must honour, even if different --from/--to flags are
// supplied again. from/to are RFC 3339 UTC ("" = unbounded on that side). An
// inactive range (both empty) is not stored, so an unrestricted first run does
// not pin later resumes to "no range".
func (d *DB) SaveRange(fromUTC, toUTC, tz string) {
	if fromUTC == "" && toUTC == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.Exec("INSERT OR IGNORE INTO run_ranges(run_id, from_utc, to_utc, tz) "+
		"VALUES(?,?,?,?)", rangeKey, fromUTC, toUTC, tz)
}

// LoadRange returns the persisted DB-wide window and whether one was stored. On
// resume the stored range wins over any flags supplied again, so the selection
// is identical to the original run.
func (d *DB) LoadRange() (fromUTC, toUTC, tz string, found bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	err := d.con.QueryRow("SELECT from_utc, to_utc, tz FROM run_ranges WHERE run_id=?",
		rangeKey).Scan(&fromUTC, &toUTC, &tz)
	return fromUTC, toUTC, tz, err == nil
}

// ------------------------------------------------------------ mailboxes --

func (d *DB) UpsertMailbox(key, sh, su, dh, du string) (int64, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.Exec("INSERT OR IGNORE INTO mailboxes(key,src_host,src_user,dst_host,dst_user) "+
		"VALUES(?,?,?,?,?)", key, sh, su, dh, du)
	var id int64
	var status string
	err := d.con.QueryRow("SELECT id, status FROM mailboxes WHERE key=?", key).
		Scan(&id, &status)
	return id, status, err
}

func (d *DB) SetMailbox(mid int64, status, lastError string, attempts int,
	msgsTotal, msgsDone, bytesTotal, bytesDone int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.Exec("UPDATE mailboxes SET status=?, last_error=?, attempts=?, "+
		"msgs_total=?, msgs_done=?, bytes_total=?, bytes_done=?, updated=? WHERE id=?",
		status, lastError, attempts, msgsTotal, msgsDone, bytesTotal, bytesDone, now(), mid)
}

func (d *DB) SetMailboxStatus(mid int64, status, lastError string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.Exec("UPDATE mailboxes SET status=?, last_error=?, updated=? WHERE id=?",
		status, lastError, now(), mid)
}

// ---------------------------------------------------------------- leases --

func (d *DB) TryLease(mid int64, owner string) (ok bool, other string, age float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ts float64
	d.con.QueryRow("SELECT lease_ts, lease_owner FROM mailboxes WHERE id=?", mid).
		Scan(&ts, &other)
	age = now() - ts
	if other != "" && other != owner && age < d.LeaseFresh {
		return false, other, age
	}
	d.con.Exec("UPDATE mailboxes SET lease_ts=?, lease_owner=? WHERE id=?", now(), owner, mid)
	return true, other, age
}

func (d *DB) RefreshLease(mid int64, owner string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.con.Exec("UPDATE mailboxes SET lease_ts=? WHERE id=? AND lease_owner=?",
		now(), mid, owner)
	if err != nil {
		return true // transient DB trouble is not a lost lease
	}
	n, _ := res.RowsAffected()
	return n == 1
}

func (d *DB) ClearLease(mid int64, owner string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.Exec("UPDATE mailboxes SET lease_ts=0, lease_owner='' WHERE id=? AND lease_owner=?",
		mid, owner)
}

func (d *DB) ReadLease(mid int64) (owner string, ts float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.QueryRow("SELECT lease_owner, lease_ts FROM mailboxes WHERE id=?", mid).
		Scan(&owner, &ts)
	return
}

// ForceLease: atomic compare-and-swap takeover of a verified-dead worker's
// lease. Fails if the owner heartbeat advanced (it is alive).
func (d *DB) ForceLease(mid int64, owner, observedOwner string, observedTS float64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.con.Exec("UPDATE mailboxes SET lease_ts=?, lease_owner=? "+
		"WHERE id=? AND lease_owner=? AND lease_ts<=?",
		now(), owner, mid, observedOwner, observedTS+0.001)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// ---------------------------------------------------------------- workers --

func (d *DB) RegisterWorker(owner, runID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	host, _ := os.Hostname()
	d.con.Exec("DELETE FROM workers WHERE heartbeat < ?", now()-86400)
	d.con.Exec("INSERT OR REPLACE INTO workers(id,host,pid,run_id,started,heartbeat,status) "+
		"VALUES(?,?,?,?,?,?, 'active')", owner, host, os.Getpid(), runID, now(), now())
}

func (d *DB) WorkerHeartbeat(owner string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.Exec("UPDATE workers SET heartbeat=? WHERE id=?", now(), owner)
}

func (d *DB) DeregisterWorker(owner string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.Exec("UPDATE mailboxes SET lease_ts=0, lease_owner='' WHERE lease_owner=?", owner)
	d.con.Exec("DELETE FROM workers WHERE id=?", owner)
}

func (d *DB) WorkerHBAge(owner string) (float64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var hb float64
	err := d.con.QueryRow("SELECT heartbeat FROM workers WHERE id=?", owner).Scan(&hb)
	if err != nil {
		return 0, false
	}
	age := now() - hb
	if age < 0 {
		age = 0
	}
	return age, true
}

// ---------------------------------------------------------------- folders --

func (d *DB) FolderRow(mid int64, srcName, dstName string) (fid int64,
	uvSrc, uvDst int64, lastUIDNextDst int64, adoptDone bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.Exec("INSERT OR IGNORE INTO folders(mailbox_id, src_name, dst_name) VALUES(?,?,?)",
		mid, srcName, dstName)
	d.con.Exec("UPDATE folders SET dst_name=? WHERE mailbox_id=? AND src_name=?",
		dstName, mid, srcName)
	var adopt int64
	err = d.con.QueryRow("SELECT id, uv_src, uv_dst, last_uidnext_dst, adopt_done "+
		"FROM folders WHERE mailbox_id=? AND src_name=?", mid, srcName).
		Scan(&fid, &uvSrc, &uvDst, &lastUIDNextDst, &adopt)
	adoptDone = adopt != 0
	return
}

func (d *DB) UpdateFolder(fid int64, sets map[string]any) {
	if len(sets) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var cols []string
	var args []any
	for k, v := range sets {
		cols = append(cols, k+"=?")
		args = append(args, v)
	}
	args = append(args, fid)
	d.con.Exec("UPDATE folders SET "+strings.Join(cols, ", ")+" WHERE id=?", args...)
}

// ResetFolderMessages: UIDVALIDITY churn handling (same semantics as v1.x).
func (d *DB) ResetFolderMessages(fid int64, keepDoneAsPlanned bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if keepDoneAsPlanned {
		d.con.Exec("UPDATE messages SET state='planned', dst_uid=NULL, origin='' "+
			"WHERE folder_id=? AND state IN ('done','inflight')", fid)
		d.con.Exec("UPDATE folders SET adopt_done=0, uv_dst=0 WHERE id=?", fid)
	} else {
		d.con.Exec("DELETE FROM messages WHERE folder_id=?", fid)
		d.con.Exec("UPDATE folders SET adopt_done=0, uv_src=0, last_uidnext_src=0, "+
			"msgs_total=0, bytes_total=0, msgs_done=0, bytes_done=0 WHERE id=?", fid)
	}
}

// ---------------------------------------------------------------- messages --

func (d *DB) KnownUIDIntervals(fid int64) []util.Interval {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.con.Query("SELECT src_uid FROM messages WHERE folder_id=? ORDER BY src_uid", fid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var uids []uint32
	for rows.Next() {
		var u uint32
		rows.Scan(&u)
		uids = append(uids, u)
	}
	return util.ToIntervals(uids)
}

func (d *DB) InsertPlanned(fid int64, rows []MsgRow) {
	if len(rows) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.con.Begin()
	if err != nil {
		return
	}
	st, _ := tx.Prepare("INSERT OR IGNORE INTO messages" +
		"(folder_id,src_uid,size,flags,internaldate,fp) VALUES(?,?,?,?,?,?)")
	for _, r := range rows {
		var fp any
		if r.FP != "" {
			fp = r.FP
		}
		st.Exec(fid, r.SrcUID, r.Size, r.Flags, r.InternalDate, fp)
	}
	st.Close()
	tx.Commit()
}

func (d *DB) SetFP(fid int64, pairs map[uint32]string) {
	if len(pairs) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, _ := d.con.Begin()
	st, _ := tx.Prepare("UPDATE messages SET fp=? WHERE folder_id=? AND src_uid=?")
	for uid, fp := range pairs {
		st.Exec(fp, fid, uid)
	}
	st.Close()
	tx.Commit()
}

func (d *DB) RowsByState(fid int64, stateName string) []MsgRow {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.con.Query("SELECT src_uid, COALESCE(dst_uid,0), size, flags, "+
		"internaldate, COALESCE(fp,''), state FROM messages "+
		"WHERE folder_id=? AND state=? ORDER BY src_uid", fid, stateName)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []MsgRow
	for rows.Next() {
		var r MsgRow
		rows.Scan(&r.SrcUID, &r.DstUID, &r.Size, &r.Flags, &r.InternalDate, &r.FP, &r.State)
		out = append(out, r)
	}
	return out
}

func (d *DB) MarkState(fid int64, uids []uint32, stateName, errText string) {
	if len(uids) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, _ := d.con.Begin()
	st, _ := tx.Prepare("UPDATE messages SET state=?, error=? WHERE folder_id=? AND src_uid=?")
	for _, u := range uids {
		st.Exec(stateName, errText, fid, u)
	}
	st.Close()
	tx.Commit()
}

type DoneTriple struct {
	SrcUID uint32
	DstUID int64 // 0 = unknown
	FP     string
}

func (d *DB) MarkDone(fid int64, triples []DoneTriple, origin string) {
	if len(triples) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, _ := d.con.Begin()
	st, _ := tx.Prepare("UPDATE messages SET state='done', dst_uid=?, origin=?, error='', " +
		"fp=COALESCE(?, fp) WHERE folder_id=? AND src_uid=?")
	for _, t := range triples {
		var du any
		if t.DstUID > 0 {
			du = t.DstUID
		}
		var fp any
		if t.FP != "" {
			fp = t.FP
		}
		st.Exec(du, origin, fp, fid, t.SrcUID)
	}
	st.Close()
	tx.Commit()
}

// FolderCounts: state -> (count, bytes).
func (d *DB) FolderCounts(fid int64) map[string][2]int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string][2]int64{}
	rows, err := d.con.Query("SELECT state, COUNT(*), COALESCE(SUM(size),0) "+
		"FROM messages WHERE folder_id=? GROUP BY state", fid)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n, b int64
		rows.Scan(&s, &n, &b)
		out[s] = [2]int64{n, b}
	}
	return out
}

// MailboxTotals: done + all counters for the live dashboard (DB-authoritative).
func (d *DB) MailboxTotals(mid int64) (msgsTotal, msgsDone, bytesTotal, bytesDone int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.QueryRow("SELECT COUNT(*), COALESCE(SUM(m.size),0) FROM messages m "+
		"JOIN folders fo ON fo.id=m.folder_id WHERE fo.mailbox_id=?", mid).
		Scan(&msgsTotal, &bytesTotal)
	d.con.QueryRow("SELECT COUNT(*), COALESCE(SUM(m.size),0) FROM messages m "+
		"JOIN folders fo ON fo.id=m.folder_id WHERE fo.mailbox_id=? AND m.state='done'", mid).
		Scan(&msgsDone, &bytesDone)
	return
}

// ------------------------------------------- failed message registry --

type FailedRow struct {
	ID        int64
	MailboxID int64
	Mailbox   string
	Folder    string
	SrcUID    uint32
	MessageID string
	Subject   string
	Sender    string
	Date      string
	Size      int64
	FType     string
	Reason    string
	FirstTS   float64
	LastTS    float64
	FailCount int64
	Status    string
}

// RecordFailed upserts a permanently failed message (bumps count/last_ts;
// a previously RECOVERED/IGNORED row that fails again returns to FAILED).
func (d *DB) RecordFailed(mid int64, folder string, uid uint32, messageID,
	subject, sender, date string, size int64, ftype, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := now()
	d.con.Exec(`INSERT INTO failed_messages(mailbox_id,folder,src_uid,message_id,
subject,sender,date,size,ftype,reason,first_ts,last_ts,fail_count,status)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,1,'FAILED')
ON CONFLICT(mailbox_id,folder,src_uid) DO UPDATE SET
message_id=CASE WHEN excluded.message_id!='' THEN excluded.message_id ELSE message_id END,
subject=CASE WHEN excluded.subject!='' THEN excluded.subject ELSE subject END,
sender=CASE WHEN excluded.sender!='' THEN excluded.sender ELSE sender END,
date=CASE WHEN excluded.date!='' THEN excluded.date ELSE date END,
size=MAX(size, excluded.size), ftype=excluded.ftype,
reason=excluded.reason, last_ts=excluded.last_ts,
fail_count=fail_count+1, status='FAILED'`,
		mid, folder, uid, trunc(messageID, 200), trunc(subject, 200),
		trunc(sender, 200), trunc(date, 80), size, ftype, trunc(reason, 300), n, n)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// RegistryUIDs: uid -> status for one folder (skip + recovery decisions).
func (d *DB) RegistryUIDs(mid int64, folder string) map[uint32]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[uint32]string{}
	rows, err := d.con.Query("SELECT src_uid, status FROM failed_messages "+
		"WHERE mailbox_id=? AND folder=?", mid, folder)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var u uint32
		var s string
		rows.Scan(&u, &s)
		out[u] = s
	}
	return out
}

func (d *DB) FailedRows(mid int64, outstandingOnly bool) []FailedRow {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := `SELECT f.id, f.mailbox_id, m.src_user, f.folder, f.src_uid, f.message_id,
f.subject, f.sender, f.date, f.size, f.ftype, f.reason, f.first_ts, f.last_ts,
f.fail_count, f.status FROM failed_messages f
JOIN mailboxes m ON m.id = f.mailbox_id`
	var conds []string
	var args []any
	if mid > 0 {
		conds = append(conds, "f.mailbox_id=?")
		args = append(args, mid)
	}
	if outstandingOnly {
		conds = append(conds, "f.status IN ('FAILED','RETRY_PENDING','RETRYING')")
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY m.src_user, f.folder, f.src_uid"
	rows, err := d.con.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []FailedRow
	for rows.Next() {
		var r FailedRow
		rows.Scan(&r.ID, &r.MailboxID, &r.Mailbox, &r.Folder, &r.SrcUID, &r.MessageID,
			&r.Subject, &r.Sender, &r.Date, &r.Size, &r.FType, &r.Reason,
			&r.FirstTS, &r.LastTS, &r.FailCount, &r.Status)
		out = append(out, r)
	}
	return out
}

func (d *DB) OutstandingFailed() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int64
	d.con.QueryRow("SELECT COUNT(*) FROM failed_messages " +
		"WHERE status IN ('FAILED','RETRY_PENDING','RETRYING')").Scan(&n)
	return n
}

// SetFailedStatus transitions matching registry rows; RETRY_PENDING also
// re-plans the message rows so the next run picks them up.
func (d *DB) SetFailedStatus(status, mailboxUser, folder string, uid int64) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	conds := []string{"status != 'RECOVERED'"}
	var args []any
	if mailboxUser != "" {
		conds = append(conds, "mailbox_id IN (SELECT id FROM mailboxes WHERE src_user=?)")
		args = append(args, mailboxUser)
	}
	if folder != "" {
		conds = append(conds, "folder=?")
		args = append(args, folder)
	}
	if uid > 0 {
		conds = append(conds, "src_uid=?")
		args = append(args, uid)
	}
	where := strings.Join(conds, " AND ")
	type key struct {
		mid int64
		fol string
		uid int64
	}
	var keys []key
	rows, _ := d.con.Query("SELECT mailbox_id, folder, src_uid FROM failed_messages WHERE "+where, args...)
	if rows != nil {
		for rows.Next() {
			var k key
			rows.Scan(&k.mid, &k.fol, &k.uid)
			keys = append(keys, k)
		}
		rows.Close()
	}
	res, err := d.con.Exec("UPDATE failed_messages SET status=? WHERE "+where,
		append([]any{status}, args...)...)
	if err != nil {
		return 0
	}
	if status == "RETRY_PENDING" {
		for _, k := range keys {
			d.con.Exec(`UPDATE messages SET state='planned', error='' WHERE src_uid=?
AND state='failed' AND folder_id IN
(SELECT id FROM folders WHERE mailbox_id=? AND src_name=?)`, k.uid, k.mid, k.fol)
		}
	}
	n, _ := res.RowsAffected()
	return n
}

func (d *DB) MarkRecovered(mid int64, folder string, uid uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.Exec("UPDATE failed_messages SET status='RECOVERED', recovered_ts=? "+
		"WHERE mailbox_id=? AND folder=? AND src_uid=?", now(), mid, folder, uid)
}

// ---------------------------------------------------------------- status --

// ListWorkers: cluster roster with liveness + active mailbox counts.
func (d *DB) ListWorkers(offlineAfter float64) []WorkerRow {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.con.Query(`SELECT w.id, w.host, w.pid, w.run_id, w.started, w.heartbeat,
(SELECT COUNT(*) FROM mailboxes m WHERE m.lease_owner = w.id AND m.lease_ts > 0)
FROM workers w ORDER BY w.started`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []WorkerRow
	for rows.Next() {
		var r WorkerRow
		rows.Scan(&r.ID, &r.Host, &r.PID, &r.RunID, &r.Started, &r.Heartbeat, &r.Active)
		r.HBAge = now() - r.Heartbeat
		if r.HBAge < 0 {
			r.HBAge = 0
		}
		if r.HBAge > offlineAfter {
			r.Status = "OFFLINE"
		} else if r.Active > 0 {
			r.Status = "WORKING"
		} else {
			r.Status = "IDLE"
		}
		out = append(out, r)
	}
	return out
}

type WorkerRow struct {
	ID        string
	Host      string
	PID       int64
	RunID     string
	Started   float64
	Heartbeat float64
	HBAge     float64
	Active    int64
	Status    string
}

// StatusReport: read-only snapshot for `mailferry status` (never competes
// destructively with active workers).
type StatusReport struct {
	LastRunID     string
	LastRunStart  float64
	LastRunEnd    float64
	LastResult    string
	Counts        map[string]int64
	MsgsDone      int64
	MsgsTotal     int64
	BytesDone     int64
	BytesTotal    int64
	Outstanding   int64
	Workers       []WorkerRow
	RunningLabels []string
	// Mailboxes carries a per-mailbox row for live monitors (e.g. the
	// `mailferry attach` read-only view). Additive: existing callers that
	// only read the aggregate counters are unaffected.
	Mailboxes []MailboxRow
}

// MailboxRow is a per-mailbox status snapshot for read-only monitors.
type MailboxRow struct {
	Label  string
	Status string
	Done   int64
	Total  int64
	Failed int64
}

func (d *DB) Status(offlineAfter float64) StatusReport {
	d.mu.Lock()
	rep := StatusReport{Counts: map[string]int64{}}
	d.con.QueryRow("SELECT id, started, COALESCE(finished,0), COALESCE(result,'') "+
		"FROM runs ORDER BY started DESC LIMIT 1").
		Scan(&rep.LastRunID, &rep.LastRunStart, &rep.LastRunEnd, &rep.LastResult)
	rows, err := d.con.Query("SELECT status, COUNT(*) FROM mailboxes GROUP BY status")
	if err == nil {
		for rows.Next() {
			var s string
			var n int64
			rows.Scan(&s, &n)
			rep.Counts[s] = n
		}
		rows.Close()
	}
	rows, err = d.con.Query("SELECT src_user FROM mailboxes WHERE status='RUNNING' " +
		"AND lease_ts > 0 ORDER BY src_user LIMIT 20")
	if err == nil {
		for rows.Next() {
			var s string
			rows.Scan(&s)
			rep.RunningLabels = append(rep.RunningLabels, s)
		}
		rows.Close()
	}
	d.con.QueryRow("SELECT COALESCE(SUM(msgs_done),0), COALESCE(SUM(msgs_total),0), "+
		"COALESCE(SUM(bytes_done),0), COALESCE(SUM(bytes_total),0) FROM mailboxes").
		Scan(&rep.MsgsDone, &rep.MsgsTotal, &rep.BytesDone, &rep.BytesTotal)
	d.con.QueryRow("SELECT COUNT(*) FROM failed_messages " +
		"WHERE status IN ('FAILED','RETRY_PENDING','RETRYING')").Scan(&rep.Outstanding)
	// Per-mailbox rows for read-only monitors: done/total from the mailbox
	// aggregates, outstanding-failed joined from the registry.
	mrows, merr := d.con.Query(`SELECT m.src_user, m.status,
COALESCE(m.msgs_done,0), COALESCE(m.msgs_total,0),
(SELECT COUNT(*) FROM failed_messages f WHERE f.mailbox_id=m.id
 AND f.status IN ('FAILED','RETRY_PENDING','RETRYING'))
FROM mailboxes m ORDER BY m.id`)
	if merr == nil {
		for mrows.Next() {
			var mr MailboxRow
			mrows.Scan(&mr.Label, &mr.Status, &mr.Done, &mr.Total, &mr.Failed)
			rep.Mailboxes = append(rep.Mailboxes, mr)
		}
		mrows.Close()
	}
	d.mu.Unlock()
	rep.Workers = d.ListWorkers(offlineAfter)
	return rep
}

// MailboxFailedSkipped: count of message rows in failed/skipped state across
// the mailbox — DB-authoritative so a resume still reports WARNINGS even
// when no NEW failure happened this run.
func (d *DB) MailboxFailedSkipped(mid int64) (failed, skipped int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.con.QueryRow("SELECT "+
		"COALESCE(SUM(CASE WHEN m.state='failed' THEN 1 ELSE 0 END),0), "+
		"COALESCE(SUM(CASE WHEN m.state='skipped' THEN 1 ELSE 0 END),0) "+
		"FROM messages m JOIN folders fo ON fo.id=m.folder_id WHERE fo.mailbox_id=?", mid).
		Scan(&failed, &skipped)
	return
}
