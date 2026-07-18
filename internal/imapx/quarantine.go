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

package imapx

// Reversible relocation primitives used by `mailferry dedup --execute`:
// UID COPY (RFC 3501) and UID MOVE (RFC 6851). These are the ONLY mutating
// verbs dedup issues on a destination, and both are non-destructive to the
// message payload — a copy leaves the original in place, a move rehomes it
// to the quarantine folder. Permanent deletion (EXPUNGE) is deliberately
// never wired up here.

import (
	"errors"
	"fmt"
	"strings"
)

// HasMove reports whether the server advertises the MOVE extension so a
// true IMAP UID MOVE can be used instead of copy-then-flag.
func (c *Client) HasMove() bool { return c.Has("MOVE") }

// UIDCopy copies one UID to destWire (RFC 3501 §6.4.8). The original is left
// untouched — the caller flags it \Deleted separately (without expunging) so
// the operation stays reversible.
func (c *Client) UIDCopy(uid int64, destWire string) error {
	_, err := c.Cmd("UID COPY", fmt.Sprintf("%d %s", uid, quoteIMAP(destWire)))
	return err
}

// UIDMove moves one UID to destWire (RFC 6851 §3.1). The server atomically
// copies then removes the source, so no separate expunge is required. Only
// call this when HasMove() is true.
func (c *Client) UIDMove(uid int64, destWire string) error {
	_, err := c.Cmd("UID MOVE", fmt.Sprintf("%d %s", uid, quoteIMAP(destWire)))
	return err
}

// FlagDeleted marks a UID \Deleted WITHOUT expunging — the message stays on
// the server and can be undeleted with any mail client. This is how dedup
// keeps copy-based quarantine reversible on servers without MOVE.
func (c *Client) FlagDeleted(uid int64) error {
	return c.UIDStoreFlags(uid, `\Deleted`)
}

// isTryCreate reports whether a failed COPY/MOVE asked us to create the
// target mailbox first ([TRYCREATE], RFC 3501 §6.4.7/§6.4.8).
func isTryCreate(err error) bool {
	var ce *CommandErr
	if !errors.As(err, &ce) {
		return false
	}
	if len(ce.Code) > 0 {
		if k, _ := TokStr(ce.Code[0]); strings.EqualFold(k, "TRYCREATE") {
			return true
		}
	}
	return strings.Contains(strings.ToUpper(ce.Text), "TRYCREATE")
}
