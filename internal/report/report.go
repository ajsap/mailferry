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

// Package report: session log, per-mailbox logs, results.csv, summary.
package report

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"encoding/json"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/engine"
	"github.com/ajsap/mailferry/v2/internal/identity"
	"github.com/ajsap/mailferry/v2/internal/state"
	"github.com/ajsap/mailferry/v2/internal/util"
)

// Session: timestamped, goroutine-safe master log. An optional NDJSON
// event log (--json-logs) receives {"ts","event"} rows in parallel.
type Session struct {
	mu       sync.Mutex
	path     string
	jsonPath string
}

func NewSession(path string, jsonPath ...string) *Session {
	s := &Session{path: path}
	if len(jsonPath) > 0 {
		s.jsonPath = jsonPath[0]
	}
	return s
}

func (s *Session) Log(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := util.NowISO()
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		fmt.Fprintf(f, "%s %s\n", ts, msg)
		f.Close()
	}
	if s.jsonPath != "" {
		if jf, err := os.OpenFile(s.jsonPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			enc, _ := json.Marshal(map[string]string{"ts": ts, "event": msg})
			jf.Write(append(enc, '\n'))
			jf.Close()
		}
	}
}

// StartProgressNDJSON writes one JSON snapshot per second (--json-progress)
// until the returned stop function is called. Purely observational.
func StartProgressNDJSON(path string, stats *engine.Stats) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				snap := stats.Snapshot()
				agg := snap.Agg()
				row := map[string]any{
					"ts": util.NowISO(), "counts": agg.Counts,
					"msgs_done": agg.MsgsDone, "msgs_total": agg.MsgsTotal,
					"bytes_done": agg.BytesDone, "bytes_total": agg.BytesTotal,
					"wire_rx": agg.WireRX, "wire_tx": agg.WireTX,
					"failed_msgs": agg.FailedMsgs, "reconnects": agg.Reconnects,
				}
				if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
					enc, _ := json.Marshal(row)
					f.Write(append(enc, '\n'))
					f.Close()
				}
			}
		}
	}()
	return func() { close(done) }
}

// MailboxLoggerFactory: per-mailbox operational logs, never raises.
func MailboxLoggerFactory(logsDir string) engine.LoggerFactory {
	return func(spec config.MailboxSpec) func(string, ...any) {
		path := filepath.Join(logsDir,
			fmt.Sprintf("%03d_%s.log", spec.Index, util.SafeName(spec.Src.User)))
		var mu sync.Mutex
		write := func(line string) {
			mu.Lock()
			defer mu.Unlock()
			f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return
			}
			defer f.Close()
			fmt.Fprintf(f, "%s %s\n", util.NowISO(), line)
		}
		write("# " + identity.BannerLine())
		write("# " + identity.Slogan)
		write(fmt.Sprintf("# Migration log — %s -> %s", spec.Src.Label(), spec.Dst.Label()))
		return func(format string, a ...any) { write(fmt.Sprintf(format, a...)) }
	}
}

var exitOf = map[string]string{"SUCCESS": "0", "WARNINGS": "0", "PARTIAL": "1",
	"FAILED": "1", "STALE": "1"}

// WriteResultsCSV writes logs/results.csv (same columns as v1.x).
func WriteResultsCSV(snap engine.Snapshot, logsDir string) string {
	path := filepath.Join(logsDir, "results.csv")
	f, err := os.Create(path)
	if err != nil {
		return path
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"index", "olduser", "newuser", "status", "exit_code",
		"elapsed_seconds", "log_file", "notes", "error_type", "attempts",
		"msgs_new", "msgs_adopted", "msgs_skipped", "msgs_failed", "bytes_done",
		"folders", "reconnects", "retries"})
	for _, m := range snap.Mailboxes {
		elapsed := int64(0)
		if !m.Start.IsZero() {
			end := m.End
			if end.IsZero() {
				end = time.Now()
			}
			elapsed = int64(end.Sub(m.Start).Seconds())
		}
		notes := ""
		switch m.Status {
		case "CANCELLED":
			notes = "interrupted before completion — will be retried on the next run"
		case "SKIPPED":
			notes = "already completed in a previous run — skipped (--skip-completed)"
		case "PARTIAL":
			notes = "completed with skipped messages or failed folders — re-run to retry the gaps"
		case "WARNINGS":
			notes = fmt.Sprintf("completed with warnings — %d message(s) could not be migrated",
				m.Skipped+m.FailedMsgs)
		case "FAILED":
			notes = trunc(m.Error, 300)
		}
		folders := ""
		if m.FoldersTotal > 0 {
			folders = fmt.Sprintf("%d/%d", m.FolderIndex, m.FoldersTotal)
		}
		w.Write([]string{
			fmt.Sprint(m.Index), m.Label, m.Label2, m.Status, exitOf[m.Status],
			fmt.Sprint(elapsed), m.LogPath, notes, trunc(m.Error, 120),
			fmt.Sprint(m.Attempt), fmt.Sprint(m.Appended), fmt.Sprint(m.Adopted),
			fmt.Sprint(m.Skipped), fmt.Sprint(m.FailedMsgs), fmt.Sprint(m.BytesDone),
			folders, fmt.Sprint(m.Src.Reconnects + m.Dst.Reconnects), fmt.Sprint(m.Retries)})
	}
	return path
}

// PrintSummary: the end-of-run report.
func PrintSummary(snap engine.Snapshot, resultsPath string, cfg *config.Run,
	runtime float64, interrupted bool, colf func(string, string) string) {
	agg := snap.Agg()
	c := colf
	fmt.Println()
	title := identity.BannerLine() + " — run complete"
	if interrupted {
		title = identity.BannerLine() + " — run interrupted, partial results"
	}
	fmt.Println(c(title, "bold"))
	kv := func(k, v string) { fmt.Printf("%-21s: %s\n", k, v) }
	kv("Total runtime", util.FmtDHMS(runtime))
	kv("Mailboxes in CSV", fmt.Sprint(len(snap.Mailboxes)))
	attempted := agg.Counts["SUCCESS"] + agg.Counts["WARNINGS"] + agg.Counts["PARTIAL"] +
		agg.Counts["FAILED"] + agg.Counts["STALE"]
	kv("Attempted this run", fmt.Sprint(attempted))
	kv("Successful", c(fmt.Sprint(agg.Counts["SUCCESS"]), "green"))
	if n := agg.Counts["WARNINGS"]; n > 0 {
		kv("With warnings", c(fmt.Sprintf("%d — a few messages could not be migrated", n), "yellow"))
	}
	if n := agg.Counts["PARTIAL"]; n > 0 {
		kv("Partial", c(fmt.Sprint(n), "yellow"))
	}
	kv("Failed", fmt.Sprint(agg.Counts["FAILED"]))
	if n := agg.Counts["CANCELLED"]; n > 0 {
		kv("Cancelled", c(fmt.Sprint(n), "yellow"))
	}
	if attempted > 0 {
		kv("Success rate", fmt.Sprintf("%.0f%% (of attempted)",
			float64(agg.Counts["SUCCESS"]+agg.Counts["WARNINGS"])*100/float64(attempted)))
	}
	kv("Messages synced", fmt.Sprintf("%d of %d (%s)", agg.MsgsDone, agg.MsgsTotal,
		util.Pct(agg.MsgsDone, agg.MsgsTotal)))
	kv("  copied (new)", fmt.Sprint(agg.Appended))
	if agg.Adopted > 0 {
		kv("  adopted (dup-safe)", fmt.Sprintf("%d — already on destination, not re-copied", agg.Adopted))
	}
	if agg.SkippedMsgs > 0 {
		kv("  skipped msgs", c(fmt.Sprintf("%d (see per-mailbox logs)", agg.SkippedMsgs), "yellow"))
	}
	kv("Data synced", util.FmtBytes(float64(agg.BytesDone)))
	kv("Wire traffic", fmt.Sprintf("down %s / up %s",
		util.FmtBytes(float64(agg.WireRX)), util.FmtBytes(float64(agg.WireTX))))
	if runtime > 0 && agg.WireTX > 0 {
		kv("Avg throughput", util.FmtBytes(float64(agg.WireTX)/runtime)+"/s (upload wire)")
	}
	if agg.Reconnects > 0 {
		kv("Reconnects", fmt.Sprint(agg.Reconnects))
	}
	kv("Per-mailbox logs", cfg.LogsDir)
	kv("Session log", filepath.Join(cfg.LogsDir, "session.log"))
	kv("Results CSV", resultsPath)
	kv("State Database", cfg.DBPath)

	var failed, partial []engine.MBValues
	for _, m := range snap.Mailboxes {
		switch m.Status {
		case "FAILED":
			failed = append(failed, m)
		case "PARTIAL":
			partial = append(partial, m)
		}
	}
	if len(failed) > 0 {
		fmt.Println(c("Failed mailboxes:", "red"))
		for _, m := range failed {
			fmt.Printf("  - %s — %s\n", m.Label, trunc(m.Error, 140))
		}
	}
	if len(partial) > 0 {
		fmt.Println(c("Partial mailboxes (gaps will be retried next run):", "yellow"))
		for _, m := range partial {
			fmt.Printf("  - %s — skipped %d, error: %s\n", m.Label, m.Skipped, trunc(m.Error, 120))
		}
	}
	if len(failed)+len(partial) > 0 && !cfg.Ephemeral {
		fmt.Println(c("Resume: re-run the same command — completed messages are never re-copied.", "cyan"))
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

var _ = strings.TrimSpace

// PrintFailedSection renders the dedicated Failed Messages block and writes
// logs/failed_messages.csv when there are outstanding failures.
func PrintFailedSection(rows []state.FailedRow, outstanding int64, logsDir string,
	colf func(string, string) string) {
	if len(rows) == 0 {
		return
	}
	path := filepath.Join(logsDir, "failed_messages.csv")
	_ = WriteFailedCSVTo(rows, path)
	fmt.Println()
	fmt.Println(colf(fmt.Sprintf("Failed Messages (%d outstanding):", outstanding), "red"))
	limit := len(rows)
	if limit > 20 {
		limit = 20
	}
	for _, r := range rows[:limit] {
		fmt.Printf("  - %s · %s · UID %d · %s · %s\n", orNA(r.Mailbox), r.Folder,
			r.SrcUID, util.FmtBytes(float64(r.Size)), r.FType)
		fmt.Printf("      %s · %s\n", clipR(orNA(r.Subject), 48), clipR(r.Reason, 90))
	}
	if len(rows) > 20 {
		fmt.Printf("  … +%d more\n", len(rows)-20)
	}
	fmt.Println(colf("  Full report: "+path+"   (JSON: mailferry failed --json)", "cyan"))
	fmt.Println(colf("  Retry any time: mailferry retry-failed — circumstances change "+
		"(quota, upgrades, filters).", "cyan"))
}

func orNA(s string) string {
	if s == "" {
		return "?"
	}
	return s
}
func clipR(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// WriteFailedCSVTo exports the Failed Message Registry to a CSV file.
func WriteFailedCSVTo(rows []state.FailedRow, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"mailbox", "folder", "uid", "message_id", "subject", "sender",
		"date", "size", "failure_type", "reason", "fail_count", "first_failure",
		"last_failure", "status"})
	for _, r := range rows {
		w.Write([]string{r.Mailbox, r.Folder, fmt.Sprint(r.SrcUID), r.MessageID,
			r.Subject, r.Sender, r.Date, fmt.Sprint(r.Size), r.FType, r.Reason,
			fmt.Sprint(r.FailCount), tsISO(r.FirstTS), tsISO(r.LastTS), r.Status})
	}
	return nil
}

// FailedJSON prints the registry as JSON to stdout.
func FailedJSON(rows []state.FailedRow) int {
	type outRow struct {
		Mailbox, Folder, MessageID, Subject, Sender, Date, FType, Reason, Status string
		UID                                                                      uint32
		Size, FailCount                                                          int64
	}
	var out []outRow
	for _, r := range rows {
		out = append(out, outRow{r.Mailbox, r.Folder, r.MessageID, r.Subject, r.Sender,
			r.Date, r.FType, r.Reason, r.Status, r.SrcUID, r.Size, r.FailCount})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return 0
}

func tsISO(ts float64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(int64(ts), 0).Format("2006-01-02T15:04:05")
}
