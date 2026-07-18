// MailFerry - IMAP Migration & Sync
// A High-Performance Native IMAP Migration Engine
//
// Copyright (C) 2026 Andy Saputra <andy@saputra.org>
//
// Licensed under the GNU Affero General Public License v3.0 (AGPL-3.0).

package state

// OpenForTest opens an existing State Database with default lease freshness
// (used by the recovery test suite to drive registry transitions directly).
func OpenForTest(path string) (*DB, error) { return Open(path, false, 300) }
