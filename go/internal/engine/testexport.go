// MailFerry - IMAP Migration & Sync
// A High-Performance Native IMAP Migration Engine
//
// Copyright (C) 2026 Andy Saputra <andy@saputra.org>
//
// Licensed under the GNU Affero General Public License v3.0 (AGPL-3.0).

package engine

import "github.com/ajsap/mailferry/v2/internal/state"

// OpenStateForTest exposes the State Database to black-box e2e tests.
func OpenStateForTest(path string) (*state.DB, error) { return state.OpenForTest(path) }
