// MailFerry - IMAP Migration & Sync
// A High-Performance Native IMAP Migration Engine
//
// Copyright (C) 2026 Andy Saputra <andy@saputra.org>
//
// https://saputra.org
// https://github.com/ajsap/mailferry
//
// Licensed under the GNU Affero General Public License v3.0 (AGPL-3.0).
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at
// your option) any later version.
//
// Contributions welcome: submit issues, feature requests and pull requests
// at https://github.com/ajsap/mailferry

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
