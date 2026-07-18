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

package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/ajsap/mailferry/v2/internal/engine"
	"github.com/ajsap/mailferry/v2/internal/identity"
	"github.com/ajsap/mailferry/v2/internal/util"
)

func running(snap engine.Snapshot) []engine.MBValues {
	var out []engine.MBValues
	for _, m := range snap.Mailboxes {
		if m.Status == "RUNNING" || m.Status == "RETRYING" {
			out = append(out, m)
		}
	}
	return out
}

// ------------------------------------------------------------- workers --

func (m *Model) workersView(w, h int) string {
	var b strings.Builder
	roster := m.bus.ClusterSnapshot()
	if q := strings.ToLower(m.viewSearch(vWorkers)); q != "" {
		var out []engine.WorkerInfo
		for _, wk := range roster {
			if strings.Contains(strings.ToLower(wk.ID), q) ||
				strings.Contains(strings.ToLower(wk.Host), q) {
				out = append(out, wk)
			}
		}
		roster = out
	}
	b.WriteString(cBold.Render(fmt.Sprintf(" CLUSTER — %d worker(s) on %s", len(roster), m.snap.DBPath)) + "\n\n")
	b.WriteString(cDim.Render(fmt.Sprintf("  %-28s %-16s %-8s %8s %10s %16s",
		"Worker ID", "Host", "Status", "Mailboxes", "Heartbeat", "Connected since")) + "\n")
	for _, wk := range roster {
		me := ""
		if wk.ID == m.bus.WorkerID {
			me = " ◂ this"
		}
		hb := "-"
		if !wk.Heartbeat.IsZero() {
			hb = fmt.Sprintf("%ds ago", int(wk.HBAge))
		}
		since := "-"
		if !wk.Started.IsZero() {
			since = wk.Started.Format("02 Jan 15:04")
		}
		b.WriteString(fmt.Sprintf("  %-28s %-16s %s %8d %10s %16s\n",
			clip(wk.ID+me, 28), clip(wk.Host, 16),
			statusStyle(wk.Status).Render(fmt.Sprintf("%-8s", wk.Status)),
			wk.Active, hb, since))
	}
	if len(roster) == 0 {
		b.WriteString("  (single instance — no other workers have joined this State Database)\n")
	}
	run := running(m.snap)
	b.WriteString("\n" + cBold.Render(fmt.Sprintf(" TRANSFERS — %d active of %d local slots",
		len(run), m.snap.Workers)) + "\n\n")
	for i, mb := range run {
		b.WriteString(fmt.Sprintf("  W%-2d %-26s %-14s %s %s/s\n", i+1, clip(mb.Label, 26),
			clip(orEmpty(mb.Folder), 14), clip(mb.Op, w-60),
			util.FmtBytes(m.mbSpeed(mb.Index))))
	}
	if len(run) == 0 {
		b.WriteString("  (no active transfers on this instance)\n")
	}
	return b.String()
}

// ----------------------------------------------------------- mailboxes --

func (m *Model) mailboxesView(w, h int) string {
	var b strings.Builder
	b.WriteString(cBold.Render(fmt.Sprintf(" MAILBOXES — %d total", len(m.snap.Mailboxes))) + "\n\n")
	rows := m.mailboxRows()
	if m.mailSel >= len(rows) {
		m.mailSel = len(rows) - 1
	}
	if m.mailSel < 0 {
		m.mailSel = 0
	}
	b.WriteString(cDim.Render(fmt.Sprintf("  %3s %-30s %-8s %-7s %-16s %6s %8s",
		"#", "Mailbox", "Status", "Fldr", "Msgs", "Fail", "Elapsed")) + "\n")
	vh := h - 4
	top := 0
	if m.mailSel >= vh {
		top = m.mailSel - vh + 1
	}
	for i := top; i < len(rows) && i < top+vh; i++ {
		mb := rows[i]
		cursor := "  "
		if i == m.mailSel {
			cursor = cCyan.Render("▸ ")
		}
		el := "-"
		if !mb.Start.IsZero() {
			end := mb.End
			if end.IsZero() {
				end = time.Now()
			}
			el = util.FmtDHMS(end.Sub(mb.Start).Seconds())
		}
		msgs := "-"
		if mb.MsgsTotal > 0 {
			msgs = fmt.Sprintf("%d/%d", mb.MsgsDone, mb.MsgsTotal)
		}
		fl := "-"
		if mb.FoldersTotal > 0 {
			fl = fmt.Sprintf("%d/%d", mb.FolderIndex, mb.FoldersTotal)
		}
		nfail := mb.Skipped + mb.FailedMsgs
		failCell := "-"
		if nfail > 0 {
			failCell = cRed.Render(fmt.Sprintf("%d", nfail))
		}
		b.WriteString(fmt.Sprintf("%s%3d %-30s %s %-7s %-16s %6s %8s\n",
			cursor, mb.Index, clip(mb.Label, 30),
			statusStyle(mb.Status).Render(fmt.Sprintf("%-8s", mb.Status)),
			fl, clip(msgs, 16), failCell, el))
		if i == m.mailSel {
			b.WriteString(m.mailboxDetail(mb, w))
		}
	}
	return b.String()
}

func (m *Model) mailboxDetail(mb engine.MBValues, w int) string {
	var b strings.Builder
	line := func(s string) { b.WriteString(cDim.Render("      "+clip(s, w-6)) + "\n") }
	line(fmt.Sprintf("Source      %s [%s] %s ↓%s", mb.Src.Host, strings.Join(mb.Src.Caps, ","),
		mb.Src.ConnState, util.FmtBytes(float64(mb.Src.RXBytes))))
	line(fmt.Sprintf("Destination %s [%s] %s ↑%s  pre-existing %d  new %d  adopted %d",
		mb.Dst.Host, strings.Join(mb.Dst.Caps, ","), mb.Dst.ConnState,
		util.FmtBytes(float64(mb.Dst.TXBytes)), mb.Dst.Existing, mb.Appended, mb.Adopted))
	if mb.Op != "" {
		line("Operation   " + mb.Op)
	}
	if mb.Error != "" {
		b.WriteString(cRed.Render("      "+clip(mb.Error, w-6)) + "\n")
	}
	return b.String()
}

// -------------------------------------------------------------- queue --

func (m *Model) queueView(w, h int) string {
	groups := []struct {
		name  string
		match func(string) bool
	}{
		{"RUNNING", func(s string) bool { return s == "RUNNING" || s == "RETRYING" }},
		{"REMOTE", func(s string) bool { return s == "REMOTE" }},
		{"PENDING", func(s string) bool { return s == "QUEUED" }},
		{"COMPLETED", func(s string) bool { return s == "SUCCESS" || s == "SKIPPED" }},
		{"WARNINGS", func(s string) bool { return s == "WARNINGS" }},
		{"PARTIAL", func(s string) bool { return s == "PARTIAL" }},
		{"STALE", func(s string) bool { return s == "STALE" }},
		{"FAILED", func(s string) bool { return s == "FAILED" }},
	}
	var b strings.Builder
	b.WriteString(cBold.Render(fmt.Sprintf(" QUEUE — %d mailboxes", len(m.snap.Mailboxes))) + "\n\n")
	for _, g := range groups {
		var names []string
		for _, mb := range m.snap.Mailboxes {
			if g.match(mb.Status) {
				names = append(names, fmt.Sprintf("%d %s", mb.Index, mb.Label))
			}
		}
		st := statusStyle(map[string]string{"PENDING": "QUEUED"}[g.name])
		if g.name != "PENDING" {
			st = statusStyle(g.name)
		}
		b.WriteString(st.Render(fmt.Sprintf(" %s (%d)", g.name, len(names))) + "\n")
		shown := names
		extra := ""
		if len(names) > 6 {
			shown = names[:6]
			extra = fmt.Sprintf("  … +%d", len(names)-6)
		}
		b.WriteString("   " + clip(strings.Join(shown, " · ")+extra, w-4) + "\n")
	}
	return b.String()
}

// ------------------------------------------------------------ history --

// historyRows applies the / search filter to the history feed.
func (m *Model) historyRows() []engine.HistoryEntry {
	hist := m.bus.HistorySnapshot()
	q := strings.ToLower(m.viewSearch(vHistory))
	if q == "" {
		return hist
	}
	var out []engine.HistoryEntry
	for _, e := range hist {
		if strings.Contains(strings.ToLower(e.Event), q) ||
			strings.Contains(strings.ToLower(e.Mailbox), q) ||
			strings.Contains(strings.ToLower(e.Details), q) {
			out = append(out, e)
		}
	}
	return out
}

// errorRows: newest first, / search filtered.
func (m *Model) errorRows() []engine.LogEntry {
	errs := m.bus.ErrorsSnapshot()
	rev := make([]engine.LogEntry, 0, len(errs))
	for i := len(errs) - 1; i >= 0; i-- {
		rev = append(rev, errs[i])
	}
	q := strings.ToLower(m.viewSearch(vErrors))
	if q == "" {
		return rev
	}
	var out []engine.LogEntry
	for _, e := range rev {
		if strings.Contains(strings.ToLower(e.Mailbox), q) ||
			strings.Contains(strings.ToLower(e.Message), q) {
			out = append(out, e)
		}
	}
	return out
}

func (m *Model) historyView(w, h int) string {
	hist := m.historyRows()
	var b strings.Builder
	foll := cGreen.Render("following")
	if !m.histFollow {
		foll = cYellow.Render("browsing (End/F to follow)")
	}
	head := fmt.Sprintf(" HISTORY / ACTIVITY — %d events", len(hist))
	if q := m.viewSearch(vHistory); q != "" {
		head += "   /" + q
	}
	b.WriteString(cBold.Render(head) + "   · " + foll + "\n\n")
	vh := h - 3
	if vh < 1 {
		vh = 1
	}
	if m.histFollow && len(hist) > 0 {
		m.histSel = len(hist) - 1
	}
	top := 0
	if m.histSel >= vh {
		top = m.histSel - vh + 1
	}
	b.WriteString(cDim.Render(fmt.Sprintf("  %-19s %-28s %-6s %s",
		"Time", "Event", "Status", "Mailbox / Details")) + "\n")
	for i := top; i < len(hist) && i < top+vh; i++ {
		e := hist[i]
		cursor := "  "
		if i == m.histSel && !m.histFollow {
			cursor = cCyan.Render("▸ ")
		}
		tail := e.Mailbox
		if e.Mailbox == "-" {
			tail = ""
		}
		if e.Details != "" {
			if tail != "" {
				tail += " · "
			}
			tail += e.Details
		}
		b.WriteString(fmt.Sprintf("%s%-19s %-28s %s %s\n", cursor,
			e.TS.Format("2006-01-02 15:04:05"), clip(e.Event, 28),
			histStyle(e.Status).Render(fmt.Sprintf("%-6s", e.Status)),
			clip(tail, w-60)))
	}
	if len(hist) == 0 {
		b.WriteString("  (no activity yet)\n")
	}
	return b.String()
}

// ------------------------------------------------------------- errors --

func (m *Model) errorsView(w, h int) string {
	errs := m.errorRows()
	var b strings.Builder
	head := fmt.Sprintf(" ERRORS & WARNINGS — %d recorded", len(errs))
	if q := m.viewSearch(vErrors); q != "" {
		head += "   /" + q
	}
	b.WriteString(cBold.Render(head) + "\n\n")
	if len(errs) == 0 {
		b.WriteString(cGreen.Render("  no errors recorded — all clear") + "\n")
		return b.String()
	}
	if m.errSel >= len(errs) {
		m.errSel = len(errs) - 1
	}
	vh := h - 3
	top := 0
	if m.errSel >= vh {
		top = m.errSel - vh + 1
	}
	for i := top; i < len(errs) && i < top+vh; i++ {
		e := errs[i]
		cursor := "  "
		if i == m.errSel {
			cursor = cCyan.Render("▸ ")
		}
		b.WriteString(cursor + sevStyle(e.Severity).Render(fmt.Sprintf("%s  %-5s %-24s %s",
			e.TS.Format("15:04:05"), e.Severity, clip(e.Mailbox, 24),
			clip(e.Message, w-44))) + "\n")
	}
	return b.String()
}

// -------------------------------------------------------- performance --

// sparkline renders values (0..max) as a compact ▁▂▃▄▅▆▇█ history strip.
func sparkline(values []float64, width int) string {
	if width < 4 {
		width = 4
	}
	blocks := []rune("▁▂▃▄▅▆▇█")
	if len(values) > width {
		values = values[len(values)-width:]
	}
	var max float64
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	if max <= 0 {
		return strings.Repeat("·", len(values))
	}
	var b strings.Builder
	for _, v := range values {
		i := int(v / max * float64(len(blocks)-1))
		if i < 0 {
			i = 0
		}
		if i >= len(blocks) {
			i = len(blocks) - 1
		}
		b.WriteRune(blocks[i])
	}
	return b.String()
}

func (m *Model) performanceView(w, h int) string {
	agg := m.snap.Agg()
	br, mr := m.displayRates()
	s := m.snap.Stalls
	sw := w - 30
	if sw > 40 {
		sw = 40
	}
	if sw < 8 {
		sw = 8
	}
	var b strings.Builder
	b.WriteString(cBold.Render(" PERFORMANCE") + "   " + cDim.Render("60-second histories") + "\n\n")
	b.WriteString(fmt.Sprintf("  Throughput  %10s   %s\n", util.FmtBytes(br)+"/s",
		cCyan.Render(sparkline(m.histRate, sw))))
	b.WriteString(fmt.Sprintf("  Messages    %10s   %s\n", fmt.Sprintf("%.0f/s", mr),
		cCyan.Render(sparkline(m.histMsgs, sw))))
	cpu := "N/A"
	if m.sys.HasCPU {
		cpu = fmt.Sprintf("%.0f%%", m.sys.CPU)
	}
	b.WriteString(fmt.Sprintf("  CPU         %10s   %s\n", cpu,
		cCyan.Render(sparkline(m.histCPU, sw))))
	if m.sys.HasLoad {
		b.WriteString(fmt.Sprintf("  Load        %.2f %.2f %.2f\n",
			m.sys.Load[0], m.sys.Load[1], m.sys.Load[2]))
	}
	mem := "Process RSS " + util.FmtBytes(float64(m.sys.RSS))
	if m.sys.MemTotal > 0 && m.sys.MemUsed > 0 {
		mem += fmt.Sprintf(" · system %s/%s", util.FmtBytes(float64(m.sys.MemUsed)),
			util.FmtBytes(float64(m.sys.MemTotal)))
	}
	b.WriteString("  Memory      " + mem + "\n")
	b.WriteString(fmt.Sprintf("  Network     wire ↓%s · ↑%s\n",
		util.FmtBytes(float64(agg.WireRX)), util.FmtBytes(float64(agg.WireTX))))
	b.WriteString(fmt.Sprintf("  Payload     %s of %s (%s)\n",
		util.FmtBytes(float64(agg.BytesDone)), util.FmtBytes(float64(agg.BytesTotal)),
		util.Pct(agg.BytesDone, agg.BytesTotal)))
	busy := agg.Counts["RUNNING"] + agg.Counts["RETRYING"]
	b.WriteString(fmt.Sprintf("  Workers     %d/%d busy · reconnects %d · retries %d\n",
		busy, m.snap.Workers, agg.Reconnects, agg.Retries))
	b.WriteString(fmt.Sprintf("  Efficiency  adopted %d duplicates prevented · skipped %d · failed %d\n",
		agg.Adopted, agg.SkippedMsgs, agg.FailedMsgs))
	b.WriteString(fmt.Sprintf("  Stalls      detected %d · recovered %d · failed %d\n",
		s[0], s[1], s[2]))
	return b.String()
}

// --------------------------------------------------------------- logs --

func (m *Model) filteredLogs() []engine.LogEntry {
	logs := m.bus.LogsSnapshot()
	q := strings.ToLower(m.viewSearch(vLogs))
	if q == "" {
		return logs
	}
	var out []engine.LogEntry
	for _, e := range logs {
		if strings.Contains(strings.ToLower(e.Mailbox), q) ||
			strings.Contains(strings.ToLower(e.Message), q) ||
			strings.Contains(strings.ToLower(e.Severity), q) {
			out = append(out, e)
		}
	}
	return out
}

func (m *Model) logViewHeight() int {
	h := m.height - 6
	if h < 1 {
		h = 1
	}
	return h
}

func (m *Model) logsView(w, h int) string {
	logs := m.filteredLogs()
	vh := h - 2
	if vh < 1 {
		vh = 1
	}
	if m.logFollow {
		m.logTop = len(logs) - vh
		if m.logTop < 0 {
			m.logTop = 0
		}
	}
	foll := cGreen.Render("FOLLOW: ON ▶")
	if !m.logFollow {
		foll = cYellow.Render("FOLLOW: OFF ⏸ (press F to resume)")
	}
	var b strings.Builder
	b.WriteString(cBold.Render(fmt.Sprintf(" LOGS — %d events · ", len(logs))) + foll + "\n")
	end := m.logTop + vh
	if end > len(logs) {
		end = len(logs)
	}
	for i := m.logTop; i < end; i++ {
		e := logs[i]
		b.WriteString(sevStyle(e.Severity).Render(fmt.Sprintf("  %s  %-5s %-22s %s",
			e.TS.Format("15:04:05"), e.Severity, clip(e.Mailbox, 22),
			clip(e.Message, w-42))) + "\n")
	}
	if len(logs) == 0 {
		b.WriteString("  (no log events yet)\n")
	} else if !m.logFollow {
		b.WriteString(cDim.Render(fmt.Sprintf("  ── viewing %d–%d of %d · F returns to the live tail ──",
			m.logTop+1, end, len(logs))) + "\n")
	}
	return b.String()
}

// ----------------------------------------------------------- settings --

func (m *Model) settingsView(w, h int) string {
	var b strings.Builder
	b.WriteString(cBold.Render(" SETTINGS") + "\n\n")
	paused := "running"
	if m.bus.Paused {
		paused = cYellow.Render("PAUSED")
	}
	b.WriteString("  Migration   " + paused + "  (p pauses / resumes from any view)\n")
	b.WriteString(fmt.Sprintf("  Refresh     %v (display only — engine unaffected)\n", m.refresh))
	b.WriteString(fmt.Sprintf("  Run         %s · State Database %s\n", m.snap.CSVFile, m.snap.DBPath))
	b.WriteString(fmt.Sprintf("  Logs        %s\n", m.snap.LogsDir))
	b.WriteString(fmt.Sprintf("  Cluster     worker %s · %d worker(s) on this State Database\n",
		m.bus.WorkerID, len(m.bus.ClusterSnapshot())))
	return b.String()
}

// -------------------------------------------------------------- help --

func (m *Model) helpView(w, h int) string {
	var b strings.Builder
	for _, l := range identity.AboutLines() {
		if strings.HasPrefix(l, "About ") {
			b.WriteString(cBold.Render(l) + "\n")
		} else {
			b.WriteString("  " + l + "\n")
		}
	}
	b.WriteString("\n" + cBold.Render("Keyboard") + "\n")
	rows := [][2]string{
		{"F1–F10 or 1–0", "switch view (digits work when SSH/tmux intercept F-keys)"},
		{"Tab / Shift+Tab", "cycle views"},
		{"↑↓ or k / j", "select · PgUp/PgDn · Home/End"},
		{"⏎", "open details (Mailboxes · History · Errors)"},
		{"/", "search within the view · ⏎ keep · Esc clear"},
		{"p", "pause / resume the migration"},
		{"r / R", "retry the selected / all failed mailboxes"},
		{"u", "reload the CSV (new rows are admitted live)"},
		{"s · f", "sort · filter (Mailboxes)"},
		{"Space", "freeze / resume the display refresh"},
		{"Ctrl+L", "repaint the screen"},
		{"F5 History", "End or F to follow the tail"},
		{"F8 Logs", "F to follow · scroll or mouse wheel to browse"},
		{"Esc / q", "back to the Dashboard (clears the search first)"},
		{"Ctrl+C", "graceful shutdown dialog · again to force"},
	}
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("  %-18s %s\n", r[0], cDim.Render(r[1])))
	}
	return b.String()
}

func orEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
