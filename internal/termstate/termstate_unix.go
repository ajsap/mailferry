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

//go:build darwin || linux

package termstate

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ttyFd returns the first standard descriptor attached to a terminal,
// output-first (stdout, stderr, stdin): sanitising acts on that device.
// In practice all three share the session's tty.
func ttyFd() int {
	for _, f := range []*os.File{os.Stdout, os.Stderr, os.Stdin} {
		if fd := int(f.Fd()); isTTY(fd) {
			return fd
		}
	}
	return -1
}

func isTTY(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, reqGet)
	return err == nil
}

func sanitize() []string {
	fd := ttyFd()
	if fd < 0 {
		return nil
	}
	tio, err := unix.IoctlGetTermios(fd, reqGet)
	if err != nil {
		return nil
	}
	before := *tio
	tio.Oflag |= unix.OPOST | unix.ONLCR
	tio.Oflag &^= unix.OCRNL | unix.ONOCR | unix.ONLRET
	tio.Iflag |= unix.ICRNL
	tio.Iflag &^= unix.INLCR | unix.IGNCR
	tio.Lflag |= unix.ICANON | unix.ECHO | unix.ISIG
	if tio.Oflag == before.Oflag && tio.Iflag == before.Iflag && tio.Lflag == before.Lflag {
		return nil
	}
	var repairs []string
	diff := func(name string, was, now bool) {
		if was != now {
			sign := "+"
			if !now {
				sign = "-"
			}
			repairs = append(repairs, sign+name)
		}
	}
	diff("OPOST", before.Oflag&unix.OPOST != 0, tio.Oflag&unix.OPOST != 0)
	diff("ONLCR", before.Oflag&unix.ONLCR != 0, tio.Oflag&unix.ONLCR != 0)
	diff("OCRNL", before.Oflag&unix.OCRNL != 0, tio.Oflag&unix.OCRNL != 0)
	diff("ONOCR", before.Oflag&unix.ONOCR != 0, tio.Oflag&unix.ONOCR != 0)
	diff("ONLRET", before.Oflag&unix.ONLRET != 0, tio.Oflag&unix.ONLRET != 0)
	diff("ICRNL", before.Iflag&unix.ICRNL != 0, tio.Iflag&unix.ICRNL != 0)
	diff("INLCR", before.Iflag&unix.INLCR != 0, tio.Iflag&unix.INLCR != 0)
	diff("IGNCR", before.Iflag&unix.IGNCR != 0, tio.Iflag&unix.IGNCR != 0)
	diff("ICANON", before.Lflag&unix.ICANON != 0, tio.Lflag&unix.ICANON != 0)
	diff("ECHO", before.Lflag&unix.ECHO != 0, tio.Lflag&unix.ECHO != 0)
	diff("ISIG", before.Lflag&unix.ISIG != 0, tio.Lflag&unix.ISIG != 0)
	if err := unix.IoctlSetTermios(fd, reqSet, tio); err != nil {
		return nil
	}
	return repairs
}

func describe() string {
	fd := ttyFd()
	if fd < 0 {
		return "tty=none"
	}
	t, err := unix.IoctlGetTermios(fd, reqGet)
	if err != nil {
		return "tty=unreadable"
	}
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	return fmt.Sprintf("oflag[OPOST=%d ONLCR=%d OCRNL=%d ONOCR=%d ONLRET=%d] "+
		"iflag[ICRNL=%d INLCR=%d IGNCR=%d] lflag[ICANON=%d ECHO=%d ISIG=%d]",
		b(t.Oflag&unix.OPOST != 0), b(t.Oflag&unix.ONLCR != 0), b(t.Oflag&unix.OCRNL != 0),
		b(t.Oflag&unix.ONOCR != 0), b(t.Oflag&unix.ONLRET != 0),
		b(t.Iflag&unix.ICRNL != 0), b(t.Iflag&unix.INLCR != 0), b(t.Iflag&unix.IGNCR != 0),
		b(t.Lflag&unix.ICANON != 0), b(t.Lflag&unix.ECHO != 0), b(t.Lflag&unix.ISIG != 0))
}
