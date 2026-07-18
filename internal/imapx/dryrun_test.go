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

import (
	"errors"
	"testing"
)

// TestDryRunClassifiesMutatingVerbs proves the choke-point classifier admits
// read-only verbs and blocks every server-state mutation, including the
// "UID <verb>" forms keyed on the sub-verb.
func TestDryRunClassifiesMutatingVerbs(t *testing.T) {
	readOnly := []string{
		"SELECT", "EXAMINE", "LIST", "LSUB", "STATUS", "SEARCH", "UID SEARCH",
		"FETCH", "UID FETCH", "NOOP", "CAPABILITY", "LOGIN", "LOGOUT", "ID",
	}
	for _, name := range readOnly {
		if isMutating(name) {
			t.Errorf("isMutating(%q)=true, want false (read-only command blocked under --dry-run)", name)
		}
	}
	mutating := []string{
		"APPEND", "CREATE", "DELETE", "RENAME", "STORE", "COPY", "MOVE", "EXPUNGE",
		"SUBSCRIBE", "UNSUBSCRIBE", "SETACL", "SETMETADATA",
		"UID STORE", "UID COPY", "UID MOVE", "UID EXPUNGE",
	}
	for _, name := range mutating {
		if !isMutating(name) {
			t.Errorf("isMutating(%q)=false, want true (mutation must be blocked under --dry-run)", name)
		}
	}
}

func TestDryRunClassifierIsCaseAndSpaceInsensitive(t *testing.T) {
	for _, name := range []string{"append", "  Append ", "uid store", "UiD sToRe"} {
		if !isMutating(name) {
			t.Errorf("isMutating(%q)=false; classification must ignore case/whitespace", name)
		}
	}
}

// TestDryRunBlockedErrorShape locks the sentinel error a blocked mutation
// returns: it never touches the socket and it is a *CommandErr with the DRYRUN
// status, so the engine can recognise and report it distinctly.
func TestDryRunBlockedErrorShape(t *testing.T) {
	err := blockedDryRun("APPEND")
	var ce *CommandErr
	if !errors.As(err, &ce) {
		t.Fatalf("blockedDryRun returned %T, want *CommandErr", err)
	}
	if ce.Status != "DRYRUN" {
		t.Fatalf("blocked error status=%q, want DRYRUN", ce.Status)
	}
	if ce.Name != "APPEND" {
		t.Fatalf("blocked error name=%q, want APPEND", ce.Name)
	}
}

// TestDryRunClientBlocksBeforeSocket verifies the actual choke point in
// CmdNowait: a ReadOnly client refuses a mutating command with the sentinel
// error and returns BEFORE touching the socket — the guard is the first line
// of CmdNowait, so a bare (unconnected) client is sufficient to prove nothing
// is ever written to the server under --dry-run.
func TestDryRunClientBlocksBeforeSocket(t *testing.T) {
	c := &Client{ReadOnly: true}
	for _, name := range []string{"APPEND", "UID STORE", "CREATE", "UID COPY"} {
		_, err := c.CmdNowait(name, "INBOX")
		var ce *CommandErr
		if !errors.As(err, &ce) || ce.Status != "DRYRUN" {
			t.Fatalf("ReadOnly CmdNowait(%q) err=%v, want a *CommandErr DRYRUN block", name, err)
		}
	}
}
