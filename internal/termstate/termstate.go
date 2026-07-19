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

// Package termstate makes MailFerry safe to run on a BROKEN terminal.
//
// Root cause of the "stair-step" defect (each line beginning where the
// previous one ended): MailFerry writes ordinary "\n" line output and
// relies on the tty's ONLCR post-processing to produce CR+LF. When a
// previous full-screen program — any program, including an older
// MailFerry killed mid-TUI — exits without restoring the terminal, the
// shell session is left WITHOUT newline translation. From then on every
// "\n"-writing program stair-steps from its very first line, and a
// capture/restore pair only preserves the damage: it captures the
// poisoned state at startup and faithfully restores the poison at exit.
//
// Sanitize therefore repairs exactly the cooked-mode flags that line
// output depends on BEFORE the first byte is printed, so output always
// renders from column 1 and the session is healed rather than
// re-poisoned. Snapshot records per-stage termios state to the file
// named by MAILFERRY_TERM_DIAG for field diagnosis — flags only, never
// mailbox data, addresses or credentials.
package termstate

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ajsap/mailferry/v2/internal/identity"
)

// DiagEnv names the environment variable that enables the lifecycle
// diagnostic log. Set it to a writable file path.
const DiagEnv = "MAILFERRY_TERM_DIAG"

var (
	mu     sync.Mutex
	t0     = time.Now()
	header bool
)

// Sanitize repairs the newline/cooked-mode-critical termios flags on the
// controlling terminal when they arrive broken (inherited raw-ish state).
// It sets OPOST, ONLCR, ICRNL, ICANON, ECHO and ISIG and clears OCRNL,
// ONOCR, ONLRET, INLCR and IGNCR — the exact flag set "stty sane" uses
// for newline semantics — touching nothing else (no cc chars, no speed,
// no flow control). It returns the repairs applied ("+ONLCR", "-OCRNL",
// …) or nil when the terminal was already sane or there is no terminal.
func Sanitize() []string { return sanitize() }

// Snapshot appends one labelled termios snapshot to the DiagEnv file.
// No-op unless MAILFERRY_TERM_DIAG is set.
func Snapshot(stage string, notes ...string) {
	path := os.Getenv(DiagEnv)
	if path == "" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if !header {
		header = true
		fmt.Fprintf(f, "# MailFerry %s terminal diagnostic — %s/%s\n",
			identity.Version, runtime.GOOS, runtime.GOARCH)
		fmt.Fprintf(f, "# termios flags only — no mailbox data, no addresses, no credentials\n")
		fmt.Fprintf(f, "# TERM=%q TERM_PROGRAM=%q SSH_TTY=%q\n",
			os.Getenv("TERM"), os.Getenv("TERM_PROGRAM"), os.Getenv("SSH_TTY"))
	}
	line := fmt.Sprintf("t=%9.1fms stage=%-16s stdin_tty=%-5t stdout_tty=%-5t stderr_tty=%-5t %s",
		float64(time.Since(t0).Microseconds())/1000.0, stage,
		isTTY(int(os.Stdin.Fd())), isTTY(int(os.Stdout.Fd())), isTTY(int(os.Stderr.Fd())),
		describe())
	if len(notes) > 0 {
		line += " repairs=" + strings.Join(notes, ",")
	}
	fmt.Fprintln(f, line)
}
