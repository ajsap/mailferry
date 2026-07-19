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

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCSV(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mailboxes.csv")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodHeader = "srchost,srcport,srcsecurity,srcuser,srcpassword," +
	"dsthost,dstport,dstsecurity,dstuser,dstpassword\n"

func TestCSVValidFileParses(t *testing.T) {
	body := goodHeader +
		"imap.example.com,993,ssl,ann@example.com,pw1,imap.example.org,993,ssl,ann@example.org,pw2\n" +
		"imap.example.com,143,starttls,bob@example.com,pw3,imap.example.org,,none,bob@example.org,pw4\n"
	specs, err := ValidateCSV(writeCSV(t, body))
	if err != nil {
		t.Fatalf("valid CSV rejected: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("got %d specs, want 2", len(specs))
	}
	// starttls normalises to internal "tls"; default port for none is 143.
	if specs[1].Src.Security != "tls" {
		t.Fatalf("starttls should normalise to tls, got %q", specs[1].Src.Security)
	}
	if specs[1].Dst.Port != 143 {
		t.Fatalf("empty port with none should default to 143, got %d", specs[1].Dst.Port)
	}
	if specs[0].Src.Port != 993 || specs[0].Dst.User != "ann@example.org" {
		t.Fatalf("row 1 parsed wrong: %+v", specs[0])
	}
}

func TestCSVSecurityAliasTLSMeansStartTLS(t *testing.T) {
	body := goodHeader +
		"imap.example.com,143,tls,ann@example.com,pw1,imap.example.org,143,tls,ann@example.org,pw2\n"
	specs, err := ValidateCSV(writeCSV(t, body))
	if err != nil {
		t.Fatalf("legacy 'tls' alias rejected: %v", err)
	}
	if specs[0].Src.Security != "tls" || specs[0].Dst.Security != "tls" {
		t.Fatalf("alias tls should map to tls, got %+v", specs[0])
	}
}

func TestCSVDefaultPortForSSL(t *testing.T) {
	body := goodHeader +
		"imap.example.com,,ssl,ann@example.com,pw1,imap.example.org,,ssl,ann@example.org,pw2\n"
	specs, err := ValidateCSV(writeCSV(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].Src.Port != 993 || specs[0].Dst.Port != 993 {
		t.Fatalf("empty port with ssl should default to 993, got %+v", specs[0])
	}
}

func TestCSVEmptyFile(t *testing.T) {
	if _, err := ValidateCSV(writeCSV(t, "")); err == nil {
		t.Fatal("empty CSV accepted; want error")
	}
}

func TestCSVHeaderOnlyHasNoRows(t *testing.T) {
	_, err := ValidateCSV(writeCSV(t, goodHeader))
	if err == nil || !strings.Contains(err.Error(), "no mailbox rows") {
		t.Fatalf("header-only CSV should complain about no rows, got: %v", err)
	}
}

func TestCSVAggregatesEveryError(t *testing.T) {
	// Two bad rows, each with multiple problems, plus one good row. The good
	// row must NOT rescue the file: any error blocks the whole migration, and
	// every problem is reported at once.
	body := goodHeader +
		"imap.example.com,993,ssl,ann@example.com,pw1,imap.example.org,993,ssl,ann@example.org,pw2\n" + // good
		",70000,bogus,,pw3,imap.example.org,993,ssl,bob@example.org,pw4\n" + // bad: host empty, port range, security, user empty
		"imap.example.com,143,ssl,carl@example.com,pw5,imap.example.org,143,ssl,,\n" // bad: dstuser empty, dstpassword empty
	_, err := ValidateCSV(writeCSV(t, body))
	if err == nil {
		t.Fatal("CSV with errors was accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "CSV validation failed") || !strings.Contains(msg, "Migration was not started") {
		t.Fatalf("aggregated error missing framing: %v", msg)
	}
	// Passwords must never be echoed; problems refer to columns by name.
	for _, secret := range []string{"pw1", "pw2", "pw3", "pw4", "pw5"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("a password value leaked into the error output: %q\n%s", secret, msg)
		}
	}
	// Several distinct problems should be present.
	for _, want := range []string{"srchost is empty", "srcport", "srcsecurity", "srcuser is empty", "dstuser is empty"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected error to mention %q; got:\n%s", want, msg)
		}
	}
}

func TestCSVMissingColumnHeader(t *testing.T) {
	// drop dstpassword
	bad := "srchost,srcport,srcsecurity,srcuser,srcpassword,dsthost,dstport,dstsecurity,dstuser\n" +
		"imap.example.com,993,ssl,ann@example.com,pw1,imap.example.org,993,ssl,ann@example.org\n"
	_, err := ValidateCSV(writeCSV(t, bad))
	if err == nil || !strings.Contains(err.Error(), "dstpassword") {
		t.Fatalf("missing header column not reported: %v", err)
	}
}

func TestCSVDuplicateColumnHeader(t *testing.T) {
	bad := "srchost,srchost,srcsecurity,srcuser,srcpassword,dsthost,dstport,dstsecurity,dstuser,dstpassword\n" +
		"a,b,c,d,e,f,g,h,i,j\n"
	_, err := ValidateCSV(writeCSV(t, bad))
	if err == nil || !strings.Contains(err.Error(), "appears") {
		t.Fatalf("duplicate header column not reported: %v", err)
	}
}

func TestCSVUnknownColumnHeader(t *testing.T) {
	bad := "srchost,srcport,srcsecurity,srcuser,srcpassword,dsthost,dstport,dstsecurity,dstuser,dstpassword,extra\n" +
		"a,993,ssl,d,e,f,993,ssl,i,j,k\n"
	_, err := ValidateCSV(writeCSV(t, bad))
	if err == nil || !strings.Contains(err.Error(), "unknown column") {
		t.Fatalf("unknown header column not reported: %v", err)
	}
}

func TestCSVObsoleteV1HeaderHint(t *testing.T) {
	bad := "oldhost,oldport,oldsecurity,olduser,oldpassword,newhost,newport,newsecurity,newuser,newpassword\n" +
		"a,993,ssl,d,e,f,993,ssl,i,j\n"
	_, err := ValidateCSV(writeCSV(t, bad))
	if err == nil || !strings.Contains(err.Error(), "obsolete v1") {
		t.Fatalf("obsolete v1 header should get a rename hint: %v", err)
	}
}

func TestCSVWrongColumnCountRow(t *testing.T) {
	bad := goodHeader + "imap.example.com,993,ssl,ann@example.com,pw1,imap.example.org,993,ssl\n" // too few columns
	_, err := ValidateCSV(writeCSV(t, bad))
	if err == nil || !strings.Contains(err.Error(), "column(s), expected") {
		t.Fatalf("mis-shaped row not reported: %v", err)
	}
}

func TestCSVBlankRowsSkipped(t *testing.T) {
	body := goodHeader +
		"\n" +
		"imap.example.com,993,ssl,ann@example.com,pw1,imap.example.org,993,ssl,ann@example.org,pw2\n" +
		"   \n"
	specs, err := ValidateCSV(writeCSV(t, body))
	if err != nil {
		t.Fatalf("blank rows should be skipped, got: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("blank rows not skipped: got %d specs", len(specs))
	}
}

func TestCSVMissingFileReturnsError(t *testing.T) {
	if _, err := ValidateCSV(filepath.Join(t.TempDir(), "does-not-exist.csv")); err == nil {
		t.Fatal("opening a missing CSV should error")
	}
}

// ParseCSV is the public entry point and must be exactly ValidateCSV.
func TestParseCSVDelegatesToValidate(t *testing.T) {
	body := goodHeader + "imap.example.com,993,ssl,ann@example.com,pw1,imap.example.org,993,ssl,ann@example.org,pw2\n"
	p := writeCSV(t, body)
	a, err1 := ParseCSV(p)
	b, err2 := ValidateCSV(p)
	if err1 != nil || err2 != nil {
		t.Fatalf("unexpected errors: %v / %v", err1, err2)
	}
	if len(a) != len(b) {
		t.Fatalf("ParseCSV and ValidateCSV disagree: %d vs %d", len(a), len(b))
	}
}
