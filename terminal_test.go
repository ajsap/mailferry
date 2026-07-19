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

//go:build linux

// Terminal newline-semantics tests. These assert what the previous
// release battery could not: the CURSOR SEMANTICS of the bytes a real
// terminal receives — every rendered line must begin at column 1 — and
// that a tty inherited in a broken raw-ish state (ONLCR off, exactly
// what a crashed full-screen program leaves behind) is HEALED before the
// first byte of output, not preserved. Termios before/after EQUALITY is
// deliberately asserted nowhere: v2.0.1 proved equality passes while the
// rendering is broken (poison in, poison faithfully restored).
package mailferry_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ajsap/mailferry/v2/internal/fakeimap"
)

func openPTY(t *testing.T) (mfd int, s *os.File) {
	t.Helper()
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx available: %v", err)
	}
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		unix.Close(mfd)
		t.Fatalf("unlock pty: %v", err)
	}
	n, err := unix.IoctlGetInt(mfd, unix.TIOCGPTN)
	if err != nil {
		unix.Close(mfd)
		t.Fatalf("pty number: %v", err)
	}
	s, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR, 0)
	if err != nil {
		unix.Close(mfd)
		t.Fatalf("open slave: %v", err)
	}
	return mfd, s
}

// drainPTY reads everything the terminal side receives until the child
// has exited and the master has been idle, using a raw non-blocking fd —
// immune to poller blocking. Fails the test on timeout.
func drainPTY(t *testing.T, mfd int, waitCh <-chan error, kill func(), timeout time.Duration) []byte {
	t.Helper()
	var buf bytes.Buffer
	tmp := make([]byte, 65536)
	deadline := time.Now().Add(timeout)
	finished, idle := false, 0
	for {
		n, err := unix.Read(mfd, tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			idle = 0
		} else {
			if err != nil && err != unix.EAGAIN && err != unix.EINTR {
				break // EIO etc: nothing more will arrive
			}
			if finished {
				if idle++; idle >= 5 {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !finished {
			select {
			case <-waitCh:
				finished = true
			default:
			}
		}
		if time.Now().After(deadline) {
			kill()
			t.Fatalf("PTY run timed out; captured %d bytes:\n%s", buf.Len(), buf.String())
		}
	}
	return buf.Bytes()
}

// poisonPTY clears ONLCR — the state any raw-mode crash leaves behind and
// the proven trigger of the reported macOS stair-step.
func poisonPTY(t *testing.T, s *os.File) {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(s.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	tio.Oflag &^= unix.ONLCR
	if err := unix.IoctlSetTermios(int(s.Fd()), unix.TCSETS, tio); err != nil {
		t.Fatal(err)
	}
}

func onlcrActive(t *testing.T, s *os.File) bool {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(s.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return tio.Oflag&unix.ONLCR != 0
}

// runOnPTY executes the binary on a fresh PTY (optionally pre-poisoned)
// and returns the bytes the TERMINAL SIDE received — after kernel output
// post-processing, exactly what a real terminal renders — plus whether
// the tty ended with newline translation active (healed).
func runOnPTY(t *testing.T, home string, poison bool, bin string, args ...string) (out []byte, healed bool) {
	t.Helper()
	mfd, s := openPTY(t)
	defer unix.Close(mfd)
	if poison {
		poisonPTY(t, s)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s, s, s
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = append(os.Environ(), "HOME="+home, "TERM=xterm-256color",
		"XDG_CONFIG_HOME="+home+"/.config", "XDG_STATE_HOME="+home+"/.local/state",
		"XDG_CACHE_HOME="+home+"/.cache", "MAILFERRY_CONFIG_DIR=")
	if err := cmd.Start(); err != nil {
		s.Close()
		t.Fatal(err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	out = drainPTY(t, mfd, waitCh, func() { _ = cmd.Process.Kill() }, 120*time.Second)
	healed = onlcrActive(t, s)
	s.Close()
	return out, healed
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-_]`)

// stripAltScreen removes the alternate-screen segment: inside it the TUI
// positions its own cursor. The assertions target line-oriented output.
func stripAltScreen(b []byte) []byte {
	i := bytes.Index(b, []byte("\x1b[?1049h"))
	j := bytes.LastIndex(b, []byte("\x1b[?1049l"))
	if i == -1 || j <= i {
		return b
	}
	out := append([]byte{}, b[:i]...)
	return append(out, b[j+len("\x1b[?1049l"):]...)
}

// assertLeftMargin fails if any rendered line would not start at column 1.
// Two independent checks: (1) every LF the terminal received was
// translated to CRLF — i.e. newline translation was active AT THE MOMENT
// of each write; (2) a cursor-column simulation over the printable stream
// confirms the first character of every line lands at column 0.
func assertLeftMargin(t *testing.T, tag string, raw []byte) {
	t.Helper()
	b := stripAltScreen(raw)
	if n := bytes.Count(b, []byte("\n")); n > 0 {
		if crlf := bytes.Count(b, []byte("\r\n")); crlf != n {
			at := bytes.Index(b, []byte("\n"))
			for at > 0 && at < len(b) && at != -1 {
				if b[at-1] != '\r' {
					break
				}
				next := bytes.Index(b[at+1:], []byte("\n"))
				if next == -1 {
					break
				}
				at += 1 + next
			}
			lo := at - 60
			if lo < 0 {
				lo = 0
			}
			t.Fatalf("%s: %d of %d LFs reached the terminal without CR — newline "+
				"translation was OFF while writing (stair-step). Context: %q",
				tag, n-crlf, n, b[lo:at+1])
		}
	}
	col, lineStart := 0, true
	for _, r := range string(ansiRE.ReplaceAll(b, nil)) {
		switch r {
		case '\r':
			col = 0
		case '\n':
			lineStart = true
		case '\b':
			if col > 0 {
				col--
			}
		default:
			if lineStart {
				if col != 0 {
					t.Fatalf("%s: a rendered line begins at column %d, not column 1", tag, col+1)
				}
				lineStart = false
			}
			col++
		}
	}
}

func TestTerminalVersionCleanPTY(t *testing.T) {
	bin := buildBinary(t)
	out, _ := runOnPTY(t, t.TempDir(), false, bin, "version")
	if !bytes.Contains(out, []byte("IMAP Migration & Sync")) {
		t.Fatalf("banner missing:\n%s", out)
	}
	assertLeftMargin(t, "version/clean", out)
}

func TestTerminalVersionHealsPoisonedPTY(t *testing.T) {
	bin := buildBinary(t)
	out, healed := runOnPTY(t, t.TempDir(), true, bin, "version")
	assertLeftMargin(t, "version/poisoned", out) // v2.0.1 fails here: bare LFs
	if !healed {
		t.Fatal("tty still lacks ONLCR after exit — poison preserved, not healed")
	}
	if !bytes.Contains(out, []byte("repaired inherited terminal state")) {
		t.Fatalf("repair notice missing:\n%s", out)
	}
}

func TestTerminalRunTUIPoisonedPTYAndRepeat(t *testing.T) {
	bin := buildBinary(t)
	dir, home := t.TempDir(), t.TempDir()
	src := fakeimap.NewServer(acct("dora", 6))
	dst := fakeimap.NewServer(fakeimap.NewAccount("dora2", "pw"))
	if err := src.Start(); err != nil {
		t.Fatal(err)
	}
	if err := dst.Start(); err != nil {
		t.Fatal(err)
	}
	defer src.Stop()
	defer dst.Stop()
	csv := csvFor(t, dir, "t.csv", src.Port(), dst.Port(), [][2]string{{"dora", "dora2"}})

	// First run: full TUI migration on a poisoned terminal. Banner and
	// completion report must both render from column 1; the tty must be
	// handed back healed.
	out, healed := runOnPTY(t, home, true, bin,
		"run", csv, "--stale-timeout", "0", "--retry-delay", "1")
	if !bytes.Contains(out, []byte("\x1b[?1049h")) {
		t.Fatalf("TUI never started (no alternate screen):\n%s", out)
	}
	assertLeftMargin(t, "run-tui/poisoned", out)
	if !healed {
		t.Fatal("run left the tty without ONLCR — poison preserved")
	}

	// Repeated execution: the instant idempotent re-run (copies nothing)
	// on ANOTHER poisoned terminal — the exact situation of a user whose
	// session is still broken. Must render correctly and heal again.
	out2, healed2 := runOnPTY(t, home, true, bin,
		"run", csv, "--stale-timeout", "0", "--retry-delay", "1")
	assertLeftMargin(t, "run-tui/instant-poisoned", out2)
	if !healed2 {
		t.Fatal("instant re-run left the tty without ONLCR")
	}
	if got := len(dst.Accounts["dora2"].Folder("INBOX").Msgs); got != 6 {
		t.Fatalf("dora2 has %d messages, want exactly 6 (no duplicates)", got)
	}
}

func TestTerminalHeadlessPoisonedPTY(t *testing.T) {
	bin := buildBinary(t)
	dir, home := t.TempDir(), t.TempDir()
	src := fakeimap.NewServer(acct("ella", 5))
	dst := fakeimap.NewServer(fakeimap.NewAccount("ella2", "pw"))
	if err := src.Start(); err != nil {
		t.Fatal(err)
	}
	if err := dst.Start(); err != nil {
		t.Fatal(err)
	}
	defer src.Stop()
	defer dst.Stop()
	csv := csvFor(t, dir, "h.csv", src.Port(), dst.Port(), [][2]string{{"ella", "ella2"}})

	out, healed := runOnPTY(t, home, true, bin,
		"run", csv, "--no-tui", "--stale-timeout", "0", "--retry-delay", "1")
	if bytes.Contains(out, []byte("\x1b[?1049h")) {
		t.Fatalf("headless run must not enter the alternate screen:\n%s", out)
	}
	assertLeftMargin(t, "headless/poisoned", out)
	if !healed {
		t.Fatal("headless run left the tty without ONLCR")
	}
}

func TestTerminalTermDiagPoisonedPTY(t *testing.T) {
	bin := buildBinary(t)
	home := t.TempDir()
	diag := home + "/diag.log"
	mfd, s := openPTY(t)
	defer unix.Close(mfd)
	poisonPTY(t, s)
	cmd := exec.Command(bin, "term-diag")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s, s, s
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = append(os.Environ(), "HOME="+home, "TERM=xterm-256color",
		"MAILFERRY_TERM_DIAG="+diag, "MAILFERRY_CONFIG_DIR=")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	buf := bytes.NewBuffer(drainPTY(t, mfd, waitCh, func() { _ = cmd.Process.Kill() }, 60*time.Second))
	healed := onlcrActive(t, s)
	s.Close()
	for i := 1; i <= 6; i++ {
		if !bytes.Contains(buf.Bytes(), []byte(fmt.Sprintf("MailFerry test line %d", i))) {
			t.Fatalf("test line %d missing:\n%s", i, buf.String())
		}
	}
	assertLeftMargin(t, "term-diag/poisoned", buf.Bytes())
	if !healed {
		t.Fatal("term-diag left the tty without ONLCR")
	}
	log, err := os.ReadFile(diag)
	if err != nil {
		t.Fatalf("diag log missing: %v", err)
	}
	for _, stage := range []string{"entry", "post-sanitise", "termdiag-pre-tui",
		"termdiag-post-tui", "termdiag-end", "pre-exit"} {
		if !strings.Contains(string(log), "stage="+stage) {
			t.Fatalf("diag log lacks stage %q:\n%s", stage, log)
		}
	}
	if !strings.Contains(string(log), "repairs=+ONLCR") {
		t.Fatalf("diag log must record the +ONLCR repair:\n%s", log)
	}
	if strings.Contains(string(log), "@") {
		t.Fatalf("diag log must never contain addresses/user data:\n%s", log)
	}
}
