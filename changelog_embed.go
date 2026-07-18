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

// Package mailferry is the module root: it embeds the canonical
// repository documents so the single static binary can show them
// (`mailferry changelog`) without shipping duplicate copies that could
// drift out of sync.
package mailferry

import _ "embed"

// Changelog is the canonical CHANGELOG.md, embedded at build time.
//
//go:embed CHANGELOG.md
var Changelog string
