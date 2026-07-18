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

// Package config: run configuration, wrapper-compatible CSV parsing and
// the optional mailferry.toml (same keys as the Python engine).
package config

import (
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Endpoint struct {
	Host     string
	Port     int
	Security string // ssl | tls | none
	User     string
	Password string
}

func (e Endpoint) Label() string { return e.User + "@" + e.Host }

type MailboxSpec struct {
	Index int
	Src   Endpoint
	Dst   Endpoint
}

func (m MailboxSpec) Label() string { return m.Src.User + "@" + m.Src.Host }
func (m MailboxSpec) Key() string {
	return strings.Join([]string{m.Src.Host, m.Src.User, m.Dst.Host, m.Dst.User}, "\x1f")
}

// Run holds every tunable. Field defaults mirror the Python engine.
type Run struct {
	CSVFile           string
	Workers           int
	LogsDir           string
	DBPath            string
	Ephemeral         bool
	Force             bool
	SkipCompleted     bool
	Retries           int
	RetryDelay        float64
	Order             string
	MaxConnsPerBox    int
	PerHostConns      int
	Timeout           float64 // inactivity watchdog seconds
	StaleTimeout      float64
	RecoveryRetries   int
	RecoveryInterval  float64
	LockTimeout       float64
	WorkerTimeout     float64
	BatchAttempts     int
	ReconnectAttempts int
	IsolateFailed     bool
	SkipKnownFailed   bool
	LogKeepDays       int
	DBHeartbeat       float64
	Include           []string
	Exclude           []string
	MapFile           string
	GmailAllMail      bool
	Subscribe         bool
	SyncFlags         bool
	RescanDest        bool
	NoDedupScan       bool
	NoTUI             bool
	Trace             bool
	Debug             bool
	CheckOnly         bool
	Compress          string // auto | off (COMPRESS=DEFLATE when offered)
	Baseline          bool   // RFC-3501-only conservative mode
	ResetStaleLocks   bool   // headless verified takeover of ambiguous leases
	JSONLogs          bool   // NDJSON event log alongside session.log
	JSONProgress      bool   // NDJSON progress snapshots (1 Hz)
	MsgRetries        int
	BatchBytes        int64
	FetchWindow       int
	AppendWindow      int
	TLSVerify         bool
	RefreshMS         int
	RunID             string

	// DryRun is the general-purpose zero-mutation mode (feature: --dry-run):
	// the engine plans, scans and analyses adoption exactly as a real run
	// would, then reports what it WOULD migrate without writing a single byte
	// to either server or a single mutation to the State Database. Distinct
	// from CheckOnly (--check), which is the lightweight connect/list preflight.
	DryRun bool

	// Range is the resolved ISO 8601 INTERNALDATE selection window (feature:
	// --from/--to). When Range.Active, only messages whose IMAP INTERNALDATE
	// falls in [From,To] inclusive are migrated. Resolved to fixed instants at
	// run creation (see ResolveRange) and persisted so a resume of the same run
	// applies the identical window.
	Range DateRange
}

// NewRunID: unique per invocation — timestamp plus random suffix, so
// concurrent processes launched in the same second never collide.
func NewRunID() string {
	b := make([]byte, 2)
	rand.Read(b)
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// Defaults returns a Run with the Python engine's defaults.
func Defaults() *Run {
	return &Run{
		Workers: 10, LogsDir: "", DBPath: "", // "" = native per-OS default, resolved lazily
		Retries: 2, RetryDelay: 30, Order: "csv",
		MaxConnsPerBox: 3, PerHostConns: 8, Timeout: 120,
		StaleTimeout: 300, RecoveryRetries: 3, RecoveryInterval: 30,
		LockTimeout: 300, WorkerTimeout: 60,
		BatchAttempts: 3, ReconnectAttempts: 5,
		IsolateFailed: true, SkipKnownFailed: true,
		LogKeepDays: 30, DBHeartbeat: 15,
		MsgRetries: 3, BatchBytes: 8 << 20, FetchWindow: 8, AppendWindow: 8,
		TLSVerify: true, RefreshMS: 250, Compress: "auto",
		RunID: NewRunID(),
	}
}

// csvHeaders is the canonical v2 header, in order. Every column is required
// and none may repeat, be misspelled or be unknown.
var csvHeaders = []string{"srchost", "srcport", "srcsecurity", "srcuser", "srcpassword",
	"dsthost", "dstport", "dstsecurity", "dstuser", "dstpassword"}

// ParseCSV reads the wrapper-compatible mailbox CSV. The WHOLE file is
// validated in one pass (see ValidateCSV): every problem is collected and
// reported together, and no migration is ever started from a file with even
// a single error — migrating real email demands the whole plan be sound
// before the first connection.
func ParseCSV(path string) ([]MailboxSpec, error) {
	return ValidateCSV(path)
}

// ValidateCSV validates the entire mailbox CSV in a single pass and returns
// the parsed specs only when the file is completely clean. On any problem it
// returns ONE aggregated error listing every issue with its row number, so
// operators fix everything at once instead of one-error-at-a-time. Passwords
// are never echoed — offending values are referred to by column name only
// (except the port value, which is not a secret and is quoted to help).
func ValidateCSV(path string) ([]MailboxSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // count is validated per-row below, not by the reader
	rows, err := r.ReadAll()
	if err != nil {
		// A reader error carries the offending line via *csv.ParseError; surface
		// it as a single aggregated failure with the row number.
		return nil, aggregateErrors(path, []string{csvReaderError(err)})
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("CSV is empty")
	}

	// ---- header validation (missing / duplicate / unknown / obsolete) ----
	head, headerErrs := validateHeader(rows[0])
	if len(headerErrs) > 0 {
		// The header defines the shape of every row; without a sound header the
		// per-row checks are meaningless, so report header problems on their own.
		return nil, aggregateErrors(path, headerErrs)
	}

	get := func(row []string, key string) string {
		i := head[key]
		if i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}
	// sec normalises the security token. The canonical inputs are none|ssl|
	// starttls; the legacy alias "tls" is accepted silently and means STARTTLS
	// (kept for wrapper compatibility). Internally STARTTLS is stored as "tls",
	// exactly as before, so nothing downstream changes.
	sec := func(raw string) (string, bool) {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "ssl":
			return "ssl", true
		case "none":
			return "none", true
		case "starttls", "tls":
			return "tls", true
		}
		return "", false
	}
	// port parses the connection port; empty is allowed (a sensible default is
	// chosen from the security), but a present-yet-invalid value is an error.
	port := func(raw, sec string) (int, bool) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			if sec == "ssl" {
				return 993, true
			}
			return 143, true
		}
		p, err := strconv.Atoi(raw)
		if err != nil || p < 1 || p > 65535 {
			return 0, false
		}
		return p, true
	}

	var out []MailboxSpec
	var errs []string
	for n, row := range rows[1:] {
		lineNo := n + 2 // 1-based, +1 for the header row
		if len(row) == 0 || (len(row) == 1 && strings.TrimSpace(row[0]) == "") {
			continue // empty rows are skipped silently, as before
		}
		if len(row) != len(csvHeaders) {
			errs = append(errs, fmt.Sprintf("Row %d: has %d column(s), expected %d "+
				"(%s)", lineNo, len(row), len(csvHeaders), strings.Join(csvHeaders, ",")))
			continue // a mis-shaped row cannot be trusted for value checks
		}
		rowErrs := 0
		// required non-empty values (passwords included; referred to by name)
		for _, col := range csvHeaders {
			if col == "srcport" || col == "dstport" || col == "srcsecurity" || col == "dstsecurity" {
				continue // ports/security have their own dedicated checks below
			}
			if get(row, col) == "" {
				errs = append(errs, fmt.Sprintf("Row %d: %s is empty (a value is required)", lineNo, col))
				rowErrs++
			}
		}
		ssec, sok := sec(get(row, "srcsecurity"))
		if !sok {
			errs = append(errs, fmt.Sprintf("Row %d: srcsecurity %q is invalid "+
				"(expected: none, ssl, or starttls)", lineNo, get(row, "srcsecurity")))
			rowErrs++
		}
		dsec, dok := sec(get(row, "dstsecurity"))
		if !dok {
			errs = append(errs, fmt.Sprintf("Row %d: dstsecurity %q is invalid "+
				"(expected: none, ssl, or starttls)", lineNo, get(row, "dstsecurity")))
			rowErrs++
		}
		sport, spok := port(get(row, "srcport"), ssec)
		if !spok {
			errs = append(errs, fmt.Sprintf("Row %d: srcport %q is invalid "+
				"(expected: integer 1–65535)", lineNo, get(row, "srcport")))
			rowErrs++
		}
		dport, dpok := port(get(row, "dstport"), dsec)
		if !dpok {
			errs = append(errs, fmt.Sprintf("Row %d: dstport %q is invalid "+
				"(expected: integer 1–65535)", lineNo, get(row, "dstport")))
			rowErrs++
		}
		if rowErrs > 0 {
			continue // do not admit a row that carries any error
		}
		out = append(out, MailboxSpec{
			Index: len(out) + 1,
			Src: Endpoint{Host: get(row, "srchost"), Port: sport,
				Security: ssec, User: get(row, "srcuser"), Password: get(row, "srcpassword")},
			Dst: Endpoint{Host: get(row, "dsthost"), Port: dport,
				Security: dsec, User: get(row, "dstuser"), Password: get(row, "dstpassword")},
		})
	}
	if len(errs) > 0 {
		return nil, aggregateErrors(path, errs)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("CSV has a header but no mailbox rows")
	}
	return out, nil
}

// validateHeader checks the header row exactly: every canonical column must
// appear once, with no duplicates and no unknown columns. The obsolete v1
// old*/new* header keeps its dedicated rename hint.
func validateHeader(row []string) (map[string]int, []string) {
	seen := map[string]int{}  // column -> times seen
	first := map[string]int{} // column -> first index (used for the lookup map)
	var order []string        // columns in file order (for unknown reporting)
	for i, h := range row {
		name := strings.ToLower(strings.TrimSpace(h))
		seen[name]++
		if _, ok := first[name]; !ok {
			first[name] = i
		}
		order = append(order, name)
	}
	if _, obsolete := seen["oldhost"]; obsolete {
		return nil, []string{"obsolete v1 CSV header detected (old*/new* columns).\n" +
			"  The canonical v2 header is:\n" +
			"    " + strings.Join(csvHeaders, ",") + "\n" +
			"  Rename the columns: old* -> src*, new* -> dst* (values are unchanged)"}
	}
	known := map[string]bool{}
	for _, c := range csvHeaders {
		known[c] = true
	}
	var errs []string
	for _, c := range csvHeaders {
		switch seen[c] {
		case 0:
			errs = append(errs, fmt.Sprintf("Header: required column %q is missing "+
				"(canonical v2 header: %s)", c, strings.Join(csvHeaders, ",")))
		case 1:
			// exactly once — good
		default:
			errs = append(errs, fmt.Sprintf("Header: column %q appears %d times "+
				"(each column must appear exactly once)", c, seen[c]))
		}
	}
	reported := map[string]bool{}
	for _, name := range order {
		if name == "" || known[name] || reported[name] {
			continue
		}
		reported[name] = true
		errs = append(errs, fmt.Sprintf("Header: unknown column %q "+
			"(canonical v2 header: %s)", name, strings.Join(csvHeaders, ",")))
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return first, nil
}

// aggregateErrors renders the collected problems as ONE multi-line error in
// the documented format. cmd/mailferry prints this verbatim and exits 1.
func aggregateErrors(path string, errs []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "CSV validation failed: %s\n", path)
	fmt.Fprintf(&b, "%d error(s) found:\n", len(errs))
	for _, e := range errs {
		fmt.Fprintf(&b, "%s\n", e)
	}
	b.WriteString("Migration was not started.")
	return fmt.Errorf("%s", b.String())
}

// csvReaderError turns an encoding/csv reader failure (typically malformed
// quoting) into an operator-facing line naming the row where possible.
func csvReaderError(err error) string {
	var pe *csv.ParseError
	if errors.As(err, &pe) {
		return fmt.Sprintf("Row %d: malformed CSV — %v (check the quoting)", pe.Line, pe.Err)
	}
	return fmt.Sprintf("malformed CSV — %v (check the quoting)", err)
}

// FolderMap loads an explicit "Source = Destination" mapping file.
func (r *Run) FolderMap() map[string]string {
	out := map[string]string{}
	if r.MapFile == "" {
		return out
	}
	data, err := os.ReadFile(r.MapFile)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}
	return out
}

// CSVTemplate is written by `mailferry init`.
const CSVTemplate = `srchost,srcport,srcsecurity,srcuser,srcpassword,dsthost,dstport,dstsecurity,dstuser,dstpassword
imap.example.com,993,ssl,jeslyn@example.com,SourcePassword,imap.example.org,993,ssl,jeslyn@example.org,DestinationPassword
`
