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

// The Results view: the polished final screen an interactive migration
// lands on — the v2 native successor to the v1.2.0 completion report.
// Every figure comes from the authoritative engine snapshot and the
// engine's RunResult; nothing is recomputed for presentation. Hierarchy:
// PRIMARY (verdict + run/messages panels + attention panel), SECONDARY
// (per-mailbox results table), DETAIL (F-key views, Enter details,
// S reports popup, CLI reports).
package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ajsap/mailferry/v2/internal/engine"
	"github.com/ajsap/mailferry/v2/internal/util"
	"github.com/charmbracelet/lipgloss"
)

// ResultMsg delivers the authoritative end-of-run result to the TUI. It
// is sent by the run orchestrator the moment the engine returns, before
// the done signal, so the Results view renders complete on first frame.
type ResultMsg struct {
	Res        engine.RunResult
	RunID      string
	WorkerID   string
	DryRun     bool
	RangeLabel string
	Portable   bool
	Ephemeral  bool
	ResultsCSV string
	FailedCSV  string
	SessionLog string
	Runtime    float64 // seconds, authoritative wall clock from the runner
}

// comma renders 63074 as 63,074 (presentation only).
func comma(n int64) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return s
	}
	var b []byte
	for i, d := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			b = append(b, ',')
		}
		b = append(b, d)
	}
	return string(b)
}

// verdict classifies the finished run for the banner. Order of severity:
// failures beat warnings beat clean; special framings for dry-run and the
// idempotent nothing-new rerun.
func (m *Model) verdict() (text string, style lipgloss.Style) {
	agg := m.snap.Agg()
	switch {
	case agg.Counts["FAILED"] > 0 || agg.Counts["STALE"] > 0:
		return "✘  MIGRATION FAILED — SOME MAILBOXES NEED ATTENTION", cRed.Bold(true)
	case m.results != nil && m.results.DryRun:
		return "✔  DRY RUN COMPLETE — NOTHING WAS WRITTEN", cCyan.Bold(true)
	case agg.Counts["WARNINGS"] > 0 || agg.Counts["PARTIAL"] > 0:
		return "⚠  MIGRATION COMPLETED WITH WARNINGS", cYellow.Bold(true)
	case agg.Appended == 0 && agg.MsgsTotal > 0:
		return "✔  MIGRATION COMPLETE — NOTHING NEW TO COPY", cGreen.Bold(true)
	default:
		return "✔  MIGRATION COMPLETE", cGreen.Bold(true)
	}
}

// titledBox draws a bordered panel with the title in the top border.
func titledBox(title string, lines []string, w int) string {
	if w < 8 {
		w = 8
	}
	inner := w - 2
	head := "─ " + title + " "
	if lipgloss.Width(head) > inner {
		head = clip(head, inner)
	}
	var b strings.Builder
	b.WriteString(cDim.Render("╭") + cBold.Render(head) +
		cDim.Render(strings.Repeat("─", max0(inner-lipgloss.Width(head)))+"╮") + "\n")
	for _, ln := range lines {
		pad := max0(inner - lipgloss.Width(ln))
		b.WriteString(cDim.Render("│") + clip(ln, inner) + strings.Repeat(" ", pad) +
			cDim.Render("│") + "\n")
	}
	b.WriteString(cDim.Render("╰" + strings.Repeat("─", inner) + "╯"))
	return b.String()
}

// fmtDurShort renders a duration compactly for table cells: 42s · 12m
// · 1h 29m · 2d 4h. The panels keep the classic long form.
func fmtDurShort(sec float64) string {
	s := int64(sec + 0.5)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	case s < 86400:
		return fmt.Sprintf("%dh %02dm", s/3600, (s%3600)/60)
	default:
		return fmt.Sprintf("%dd %dh", s/86400, (s%86400)/3600)
	}
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// rkv renders one " Label      value" panel row.
func rkv(label, value string) string {
	return " " + cDim.Render(fmt.Sprintf("%-14s", label)) + " " + value
}

// statusDisplay maps engine statuses to the operator-facing wording used
// on the final screen (matching the classic v1.x vocabulary).
func statusDisplay(s string) string {
	switch s {
	case "SUCCESS":
		return "COMPLETED"
	case "WARNINGS":
		return "COMPLETED WITH WARNINGS"
	case "REMOTE":
		return "ON OTHER WORKER"
	default:
		return s
	}
}

// resultsOrder sorts problems first so attention lands where it belongs.
var resultsOrder = map[string]int{"FAILED": 0, "STALE": 1, "PARTIAL": 2,
	"WARNINGS": 3, "CANCELLED": 4, "REMOTE": 5, "SUCCESS": 6}

func (m *Model) resultsView(w, h int) string {
	snap := m.snap
	agg := snap.Agg()
	r := m.results
	var out []string

	// ---- PRIMARY: verdict banner + context line ------------------------
	text, vs := m.verdict()
	out = append(out, "", center(vs.Render(text), w))
	ctx := []string{}
	if r != nil {
		if r.DryRun {
			ctx = append(ctx, "dry run — zero mutations")
		}
		if r.RangeLabel != "" {
			ctx = append(ctx, "date range "+r.RangeLabel)
		}
		if r.Portable {
			ctx = append(ctx, "portable mode")
		}
		if r.Ephemeral {
			ctx = append(ctx, "ephemeral — nothing persisted")
		}
		ctx = append(ctx, "run "+r.RunID)
		if r.WorkerID != "" {
			ctx = append(ctx, "worker "+r.WorkerID)
		}
	}
	if len(ctx) > 0 {
		out = append(out, center(cDim.Render(strings.Join(ctx, " · ")), w))
	}
	out = append(out, "")

	// ---- PRIMARY: Run + Messages panels --------------------------------
	attempted := agg.Counts["SUCCESS"] + agg.Counts["WARNINGS"] + agg.Counts["PARTIAL"] +
		agg.Counts["FAILED"] + agg.Counts["STALE"]
	runtime := timeNow().Sub(snap.BatchStart).Seconds()
	if r != nil && r.Runtime > 0 {
		runtime = r.Runtime
	}
	runLines := []string{
		rkv("Runtime", util.FmtDHMS(runtime)),
		rkv("Mailboxes", fmt.Sprintf("%d attempted · %d in CSV", attempted, len(snap.Mailboxes))),
		rkv("Successful", cGreen.Render(fmt.Sprint(agg.Counts["SUCCESS"]))),
	}
	if n := agg.Counts["WARNINGS"]; n > 0 {
		runLines = append(runLines, rkv("With warnings", cYellow.Render(fmt.Sprint(n))))
	}
	if n := agg.Counts["PARTIAL"]; n > 0 {
		runLines = append(runLines, rkv("Partial", cYellow.Render(fmt.Sprint(n))))
	}
	failedStr := fmt.Sprint(agg.Counts["FAILED"])
	if agg.Counts["FAILED"] > 0 {
		failedStr = cRed.Render(failedStr)
	}
	runLines = append(runLines, rkv("Failed", failedStr))
	if n := agg.Counts["STALE"]; n > 0 {
		runLines = append(runLines, rkv("Stale", cRed.Render(fmt.Sprint(n))))
	}
	if n := agg.Counts["CANCELLED"]; n > 0 {
		runLines = append(runLines, rkv("Cancelled", cYellow.Render(fmt.Sprint(n))))
	}
	if n := agg.Counts["REMOTE"]; n > 0 {
		runLines = append(runLines, rkv("Other workers", cCyan.Render(fmt.Sprint(n))))
	}
	if attempted > 0 {
		rate := float64(agg.Counts["SUCCESS"]+agg.Counts["WARNINGS"]) * 100 / float64(attempted)
		runLines = append(runLines, rkv("Success rate", fmt.Sprintf("%.0f%% (of attempted)", rate)))
	}

	msgLines := []string{
		rkv("Synced", fmt.Sprintf("%s of %s (%s)", comma(agg.MsgsDone), comma(agg.MsgsTotal),
			util.Pct(agg.MsgsDone, agg.MsgsTotal))),
		rkv("Copied (new)", comma(agg.Appended)),
	}
	if agg.Planned > 0 {
		msgLines = append(msgLines, rkv("Would copy", comma(agg.Planned)+cDim.Render(" dry run — plan only")))
	}
	if agg.PriorDone > 0 {
		msgLines = append(msgLines, rkv("Prior runs", comma(agg.PriorDone)+cDim.Render(" confirmed earlier")))
	}
	if agg.Adopted > 0 {
		msgLines = append(msgLines, rkv("Adopted", comma(agg.Adopted)+cDim.Render(" dup-safe, not re-copied")))
	}
	if agg.SkippedMsgs > 0 {
		msgLines = append(msgLines, rkv("Skipped", cYellow.Render(comma(agg.SkippedMsgs))))
	}
	if agg.FailedMsgs > 0 {
		msgLines = append(msgLines, rkv("Failed msgs", cRed.Render(comma(agg.FailedMsgs))+cDim.Render(" → registry")))
	}
	msgLines = append(msgLines,
		rkv("Data", util.FmtBytes(float64(agg.BytesDone))),
		rkv("Wire", fmt.Sprintf("↓ %s  ↑ %s", util.FmtBytes(float64(agg.WireRX)),
			util.FmtBytes(float64(agg.WireTX)))))
	if runtime > 0 && agg.WireTX > 0 {
		msgLines = append(msgLines, rkv("Throughput", util.FmtBytes(float64(agg.WireTX)/runtime)+"/s upload"))
	}
	if agg.Reconnects > 0 || agg.Retries > 0 || snap.Stalls[0] > 0 {
		parts := []string{}
		if agg.Reconnects > 0 {
			parts = append(parts, fmt.Sprintf("%d reconnects", agg.Reconnects))
		}
		if agg.Retries > 0 {
			parts = append(parts, fmt.Sprintf("%d retries", agg.Retries))
		}
		if snap.Stalls[0] > 0 {
			parts = append(parts, fmt.Sprintf("%d stalls (%d recovered)", snap.Stalls[0], snap.Stalls[1]))
		}
		msgLines = append(msgLines, rkv("Resilience", cDim.Render(strings.Join(parts, " · "))))
	}

	if w >= 96 {
		lw := (w - 3) / 2
		left := titledBox("Run", runLines, lw)
		right := titledBox("Messages", msgLines, w-3-lw)
		joined := lipgloss.JoinHorizontal(lipgloss.Top, " ", left, " ", right)
		out = append(out, strings.Split(joined, "\n")...)
	} else {
		for _, box := range []string{titledBox("Run", runLines, w-2), titledBox("Messages", msgLines, w-2)} {
			for _, ln := range strings.Split(box, "\n") {
				out = append(out, " "+ln)
			}
		}
	}

	// ---- SECONDARY: per-mailbox results table --------------------------
	mbs := append([]engine.MBValues(nil), snap.Mailboxes...)
	sort.SliceStable(mbs, func(i, j int) bool {
		oi, oj := resultsOrder[mbs[i].Status], resultsOrder[mbs[j].Status]
		if oi != oj {
			return oi < oj
		}
		return mbs[i].Label < mbs[j].Label
	})
	attention := m.attentionLines(w - 6)
	reserve := 0
	if len(attention) > 0 {
		reserve = 1 + len(attention) + 2 // blank + content + borders
	}
	// exact budget: blank + top border + header + rows [+ more] + bottom
	maxRows := h - len(out) - reserve - 4
	if maxRows > len(mbs) {
		maxRows = len(mbs)
	}
	if maxRows < len(mbs) {
		maxRows-- // one line goes to the "+N more" note
	}
	if maxRows < 3 {
		maxRows = 3
	}
	const stW, msW, dtW, tmW = 23, 12, 9, 8
	labelW := (w - 6) - stW - msW - dtW - tmW - 4
	cell := func(s string, width int, right bool) string {
		s = clip(s, width)
		pad := strings.Repeat(" ", max0(width-lipgloss.Width(s)))
		if right {
			return pad + s
		}
		return s + pad
	}
	tbl := []string{cBold.Render(" " + cell("Mailbox", labelW, false) + " " +
		cell("Status", stW, false) + " " + cell("Messages", msW, true) + " " +
		cell("Data", dtW, true) + " " + cell("Time", tmW, true))}
	shown := 0
	for _, mb := range mbs {
		if shown >= maxRows {
			break
		}
		dur := "-"
		if !mb.Start.IsZero() && !mb.End.IsZero() {
			dur = fmtDurShort(mb.End.Sub(mb.Start).Seconds())
		}
		msgs := comma(mb.MsgsDone)
		if mb.FailedMsgs > 0 {
			msgs += fmt.Sprintf(" (−%d)", mb.FailedMsgs)
		}
		msgsCell := cell(msgs, msW, true)
		if mb.FailedMsgs > 0 {
			msgsCell = cYellow.Render(msgsCell)
		}
		tbl = append(tbl, " "+cell(mb.Label, labelW, false)+" "+
			statusStyle(mb.Status).Render(cell(statusDisplay(mb.Status), stW, false))+" "+
			msgsCell+" "+cell(util.FmtBytes(float64(mb.BytesDone)), dtW, true)+" "+
			cell(dur, tmW, true))
		shown++
	}
	if rest := len(mbs) - shown; rest > 0 {
		tbl = append(tbl, cDim.Render(fmt.Sprintf("   … +%d more — Enter opens the Mailboxes view", rest)))
	}
	out = append(out, "")
	box := titledBox(fmt.Sprintf("Mailboxes (%d)", len(mbs)), tbl, w-2)
	for _, ln := range strings.Split(box, "\n") {
		out = append(out, " "+ln)
	}

	// ---- PRIMARY: attention panel (only when something needs it) -------
	if len(attention) > 0 {
		out = append(out, "")
		box := titledBox("Needs attention", attention, w-2)
		for _, ln := range strings.Split(box, "\n") {
			out = append(out, " "+ln)
		}
	}
	return strings.Join(out, "\n")
}

// attentionLines builds the concise warnings/failures summary — counts,
// affected mailboxes/folders and the recovery actions. Full per-message
// detail deliberately lives in F6, `mailferry failed` and the reports.
func (m *Model) attentionLines(w int) []string {
	agg := m.snap.Agg()
	r := m.results
	var lines []string
	outstanding := int64(0)
	if r != nil {
		outstanding = r.Res.Outstanding
	}
	if outstanding == 0 && r != nil {
		outstanding = int64(len(r.Res.FailedRegistry))
	}
	if outstanding > 0 {
		plural := "s"
		if outstanding == 1 {
			plural = ""
		}
		lines = append(lines, " "+cYellow.Render(fmt.Sprintf(
			"%d message%s could not be migrated and remain in the Failed Message Registry.",
			outstanding, plural)))
		if r != nil && len(r.Res.FailedRegistry) > 0 {
			mbSet, fldSet := map[string]bool{}, map[string]bool{}
			var mbl, fll []string
			for _, row := range r.Res.FailedRegistry {
				if !mbSet[row.Mailbox] && row.Mailbox != "" {
					mbSet[row.Mailbox] = true
					mbl = append(mbl, row.Mailbox)
				}
				if !fldSet[row.Folder] && row.Folder != "" {
					fldSet[row.Folder] = true
					fll = append(fll, row.Folder)
				}
			}
			lines = append(lines, " "+cDim.Render(clip(fmt.Sprintf("Mailboxes: %s · Folders: %s",
				capList(mbl, 3), capList(fll, 4)), w-2)))
		}
		lines = append(lines, " Review: "+cBold.Render("F6")+" Failed Messages · Retry: "+
			cBold.Render("mailferry retry-failed"))
		if r != nil && r.FailedCSV != "" {
			lines = append(lines, " "+cDim.Render(clip("Report: "+r.FailedCSV, w-2)))
		}
	}
	for _, mb := range m.snap.Mailboxes {
		switch mb.Status {
		case "FAILED", "STALE":
			lines = append(lines, " "+cRed.Render(clip(fmt.Sprintf("%s — %s: %s", mb.Label,
				statusDisplay(mb.Status), util.Ellipsize(orEmpty(mb.Error), 90)), w-2)))
		}
	}
	if agg.Counts["FAILED"]+agg.Counts["STALE"]+agg.Counts["PARTIAL"] > 0 {
		lines = append(lines, " "+cCyan.Render(
			"Resume: re-run the same command — completed messages are never re-copied."))
	}
	return lines
}

func capList(items []string, n int) string {
	sort.Strings(items)
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" +%d more", len(items)-n)
}

// openReportsPopup (S on the Results view): every report and file this
// run produced, in one place.
func (m *Model) openReportsPopup() {
	r := m.results
	lines := []string{}
	add := func(k, v string) {
		if v != "" {
			lines = append(lines, cDim.Render(fmt.Sprintf("%-18s", k))+" "+v, "")
		}
	}
	if r != nil {
		add("Results CSV", r.ResultsCSV)
		add("Failed report", r.FailedCSV)
		add("Session log", r.SessionLog)
		add("Run ID", r.RunID)
	}
	add("Per-mailbox logs", m.snap.LogsDir)
	db := m.snap.DBPath
	if r != nil && r.Ephemeral {
		db = "ephemeral (--ephemeral) — nothing persisted"
	}
	add("State Database", db)
	m.popup = &popupState{Title: "Reports & files", Lines: lines}
}
