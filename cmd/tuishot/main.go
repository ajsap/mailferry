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

// tuishot: development tool — renders populated TUI frames for the
// documentation screenshots (the Go equivalent of tools/capture_views.py).
// Fixture data uses RFC-2606 example domains only.
package main

import (
	"fmt"
	"os"

	"github.com/ajsap/mailferry/v2/internal/tui"
)

func main() {
	view := "dashboard"
	if len(os.Args) > 1 {
		view = os.Args[1]
	}
	fmt.Print(tui.RenderShot(view))
}
