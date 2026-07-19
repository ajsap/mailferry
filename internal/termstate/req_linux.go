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

package termstate

import "golang.org/x/sys/unix"

// Linux reads/writes termios via TCGETS / TCSETS.
const (
	reqGet = unix.TCGETS
	reqSet = unix.TCSETS
)
