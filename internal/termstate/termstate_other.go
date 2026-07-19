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

//go:build !darwin && !linux

package termstate

// Windows and other platforms have no POSIX termios; the stair-step
// mechanism this package repairs cannot occur there. Everything is a
// no-op so callers stay platform-agnostic.

func sanitize() []string { return nil }

func isTTY(int) bool { return false }

func describe() string { return "tty=unsupported-platform" }
