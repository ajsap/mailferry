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

// A strictly READ-ONLY handle for live monitors (`mailferry attach`).
//
// Unlike Open/OpenForTest, this opens the database with SQLite mode=ro and
// NEVER runs the schema DDL or the meta INSERT — so it writes nothing at all,
// not even at open time, and cannot compete with a live migration's own
// initialisation. WAL lets any number of such readers proceed concurrently
// with the single writer, so a monitor never blocks or perturbs a migration.

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

// OpenReadOnly opens an EXISTING State Database for reading only. It performs
// no writes (no DDL, no PRAGMA journal_mode change, no meta insert) and errors
// rather than creating a file that is absent. The returned *DB is safe for the
// read-only status/report queries used by the attach monitor.
func OpenReadOnly(path string) (*DB, error) {
	// file: URI with mode=ro; keep the busy timeout so a momentary writer
	// checkpoint never turns into a spurious error for the reader.
	dsn := "file:" + url.PathEscape(path) + "?mode=ro&_pragma=busy_timeout(10000)"
	con, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	con.SetMaxOpenConns(1)
	// A trivial read proves the database exists and is reachable; a missing
	// file surfaces here rather than lazily on the first status query.
	if err := con.Ping(); err != nil {
		con.Close()
		return nil, fmt.Errorf("open read-only state database: %w", err)
	}
	return &DB{con: con, Path: dsn, LeaseFresh: 300}, nil
}
