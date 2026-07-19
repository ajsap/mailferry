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

//go:build darwin

package termstate

import "golang.org/x/sys/unix"

// macOS reads/writes termios via TIOCGETA / TIOCSETA.
const (
	reqGet = unix.TIOCGETA
	reqSet = unix.TIOCSETA
)
