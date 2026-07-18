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

import "time"

type tickerWrap struct {
	C <-chan time.Time
	t *time.Ticker
}

func newTicker(secs float64) *tickerWrap {
	t := time.NewTicker(time.Duration(secs * float64(time.Second)))
	return &tickerWrap{C: t.C, t: t}
}

func (w *tickerWrap) Stop() { w.t.Stop() }
