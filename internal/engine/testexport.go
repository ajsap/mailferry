// MailFerry — IMAP Migration & Sync
// A High-Performance Native IMAP Migration Engine
//
// Copyright (C) 2026 Andy Saputra
// Author: Andy Saputra <andy@saputra.org>
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This file is part of MailFerry (https://github.com/ajsap/mailferry).
// Licensed under the GNU Affero General Public License v3.0 or later;
// see the LICENSE file for details.

package engine

import "github.com/ajsap/mailferry/v2/internal/state"

// OpenStateForTest exposes the State Database to black-box e2e tests.
func OpenStateForTest(path string) (*state.DB, error) { return state.OpenForTest(path) }
