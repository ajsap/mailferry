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

// Automated repository check: every applicable MailFerry source file must
// carry the authorship / copyright / SPDX licence header. New files cannot
// land without it — this test fails the suite.
package mailferry_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var requiredHeaderLines = []string{
	"Copyright (C) 2026 Andy Saputra",
	"Author: Andy Saputra <andy@saputra.org>",
	"SPDX-License-Identifier: AGPL-3.0-or-later",
}

// headerExempt lists tracked source files that intentionally carry no
// MailFerry header (third-party or machine-generated). Everything here
// must have a documented reason.
var headerExempt = map[string]string{
	// (none currently — go.mod/go.sum, LICENSE, docs and images are not
	// source files and are outside the walk below)
}

func TestSourceFilesCarryLicenceHeader(t *testing.T) {
	var missing []string
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "dist" || name == ".legacy-python" ||
				strings.HasPrefix(name, ".") && name != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") && path != "build.sh" {
			return nil
		}
		if _, ok := headerExempt[filepath.ToSlash(path)]; ok {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		head := string(data)
		if len(head) > 1200 {
			head = head[:1200]
		}
		for _, want := range requiredHeaderLines {
			if !strings.Contains(head, want) {
				missing = append(missing, path+" (missing: "+want+")")
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Fatalf("source files without the required licence header:\n  %s\n\n"+
			"Add the standard MailFerry header (see any .go file) or record a "+
			"documented exemption in headers_test.go.", strings.Join(missing, "\n  "))
	}
}
