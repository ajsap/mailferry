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

package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ajsap/mailferry/v2/internal/engine"
	"github.com/ajsap/mailferry/v2/internal/identity"
	"github.com/ajsap/mailferry/v2/internal/util"
	"github.com/charmbracelet/lipgloss"
)

func timeNow() time.Time { return time.Now() }

const minCols, minRows = 72, 18

func (m *Model) View() string {
	if m.quitting {
		return ""
	}
	w, h := m.width, m.height
	if w == 0 {
		w, h = 100, 30
	}
	if m.shuttingDown {
		return m.shutdownView(w, h)
	}
	if m.popup != nil {
		return m.popupBox(w, h)
	}
	if w < minCols || h < minRows {
		return m.tooSmall(w, h)
	}
	var b strings.Builder
	b.WriteString(m.banner(w))
	b.WriteString("\n")
	b.WriteString(m.navBar(w))
	b.WriteString("\n")
	b.WriteString(strings.Repeat("─", w))
	b.WriteString("\n")
	// body fills the middle; footer is one line
	bodyH := h - 5
	body := m.safeBody(w, bodyH)
	lines := strings.Split(body, "\n")
	for i := 0; i < bodyH; i++ {
		if i < len(lines) {
			b.WriteString(clip(lines[i], w))
		}
		b.WriteString("\n")
	}
	b.WriteString(m.footer(w))
	return b.String()
}

// safeBody guards rendering: a view bug degrades to an error notice and the
// migration continues — navigation must never freeze (parity with classic).
func (m *Model) safeBody(w, h int) (out string) {
	defer func() {
		if r := recover(); r != nil {
			m.bus.Log("ERROR", "tui", fmt.Sprintf("view %q failed to render: %v",
				viewNames[m.active], r))
			out = "\n" + cRed.Render(fmt.Sprintf("  The %s view failed to render.", viewNames[m.active])) +
				"\n\n  The migration continues unaffected.\n" +
				cDim.Render("  Press 1 for the Dashboard, or 8 to check the Logs.")
		}
	}()
	return m.body(w, h)
}

func (m *Model) banner(w int) string {
	title := center(identity.BannerLine(), w)
	slogan := center(identity.Slogan, w)
	return cBanner.Render(title) + "\n" + cDim.Render(slogan)
}

func (m *Model) navBar(w int) string {
	var parts []string
	for i := 0; i < numViews; i++ {
		label := fmt.Sprintf("F%d %s", i+1, viewNames[i])
		if i+1 == 10 {
			label = "F10 " + viewNames[i]
		}
		if i == m.active {
			parts = append(parts, cBold.Render("▐"+label+"▌"))
		} else {
			parts = append(parts, cDim.Render(label))
		}
	}
	bar := " " + strings.Join(parts, " ")
	badge := ""
	if m.bus.Paused {
		badge = cYellow.Render(" ▮▮PAUSED")
	} else if m.frozen {
		badge = cCyan.Render(" ❄FROZEN")
	}
	return clip(bar, w-lipgloss.Width(badge)) + badge
}

func (m *Model) footer(w int) string {
	if m.searching {
		return cCyan.Render(clip(fmt.Sprintf(" search: /%s▏  (⏎ keep · Esc clear)", m.search), w))
	}
	hint := viewFooters[m.active]
	if m.flash != "" {
		hint = m.flash + "   ·   " + hint
		m.flash = ""
	}
	return cDim.Render(clip(" "+hint, w))
}

var viewFooters = [numViews]string{
	"F2–F10 Views   1–0 aliases   p Pause   Space Freeze   R Retry failed   u Reload CSV   ^C Quit",
	"↑↓/k j Select   / Search   F1 Dashboard   ^C Quit",
	"↑↓/k j Select   ⏎ Details   r Retry   s Sort   f Filter   / Search   ^C Quit",
	"F1 Dashboard   ^C Quit",
	"↑↓/k j Navigate   ⏎ Details   / Search   End/F Follow   Esc/q Back   ^C Quit",
	"↑↓/k j Select   ⏎ Details   / Search   ^C Quit",
	"Space Freeze   ^L Redraw   ^C Quit",
	"↑↓/k j · PgUp/PgDn · Home/End Scroll   F Follow   / Search   Mouse wheel   ^C Quit",
	"p Pause/Resume   ^C Quit",
	"↑↓ Scroll   Esc/q Back   ^C Quit",
}

func (m *Model) body(w, h int) string {
	switch m.active {
	case vDashboard:
		return m.dashboardView(w, h)
	case vWorkers:
		return m.workersView(w, h)
	case vMailboxes:
		return m.mailboxesView(w, h)
	case vQueue:
		return m.queueView(w, h)
	case vHistory:
		return m.historyView(w, h)
	case vErrors:
		return m.errorsView(w, h)
	case vPerformance:
		return m.performanceView(w, h)
	case vLogs:
		return m.logsView(w, h)
	case vSettings:
		return m.settingsView(w, h)
	case vHelp:
		return m.helpView(w, h)
	}
	return ""
}

// ------------------------------------------------------------ dashboard --

func (m *Model) dashboardView(w, h int) string {
	agg := m.snap.Agg()
	br, mr := m.displayRates()
	var b strings.Builder
	kv := func(k, v string) string { return fmt.Sprintf("%-14s: %s", k, v) }
	left := []string{
		kv("CSV File", m.snap.CSVFile),
		kv("State Database", m.snap.DBPath),
		kv("Mode", m.snap.Mode),
		kv("Workers", fmt.Sprintf("%d/%d", agg.Counts["RUNNING"]+agg.Counts["RETRYING"], m.snap.Workers)),
		kv("Runtime", util.FmtDHMS(time.Since(m.snap.BatchStart).Seconds())),
	}
	eta := "-"
	if e, ok := m.eta(); ok {
		eta = util.FmtDHMS(e)
	}
	right := []string{
		fmt.Sprintf("%-12s: %s / %s", "Wire RX/TX", util.FmtBytes(float64(agg.WireRX)), util.FmtBytes(float64(agg.WireTX))),
		fmt.Sprintf("%-12s: %s/s (%.1f msg/s)", "Rate", util.FmtBytes(br), mr),
		fmt.Sprintf("%-12s: %s", "Batch ETA", eta),
		fmt.Sprintf("%-12s: %s", "Data", util.FmtBytes(float64(agg.BytesDone))+"/"+util.FmtBytes(float64(agg.BytesTotal))),
		fmt.Sprintf("%-12s: %d", "Cluster", len(m.bus.ClusterSnapshot())),
	}
	lw := w - 40
	if lw < 30 {
		lw = 30
	}
	for i := 0; i < 5; i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		b.WriteString(fmt.Sprintf("%-*s   %s\n", lw, clip(l, lw), r))
	}
	b.WriteString(strings.Repeat("─", w) + "\n")
	b.WriteString(m.mailboxTable(w, h-7, true))
	b.WriteString(m.aggFooter(w))
	return b.String()
}

func (m *Model) mailboxTable(w, h int, detail bool) string {
	const idxW, stW, flW, msW, spW, elW = 3, 8, 7, 15, 10, 14
	var b strings.Builder
	mwidth := w - (idxW + 2 + stW + 1 + flW + 1 + msW + 1 + 5 + 1 + spW + 1 + elW)
	if mwidth < 14 {
		mwidth = 14
	}
	b.WriteString(cDim.Render(fmt.Sprintf("%*s  %-*s %-*s %-*s %-*s %5s %*s %*s",
		idxW, "#", mwidth, "Mailbox", stW, "Status", flW, "Fldr", msW, "Msgs",
		"Fail", spW, "Speed", elW, "Elapsed")) + "\n")
	shown := 0
	for _, mb := range m.snap.Mailboxes {
		if shown >= h {
			break
		}
		active := mb.Status == "RUNNING" || mb.Status == "RETRYING" || mb.Status == "REMOTE"
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
		speed := "-"
		if active {
			speed = util.FmtBytes(m.mbSpeed(mb.Index)) + "/s"
		}
		nfail := mb.Skipped + mb.FailedMsgs
		failCell := "-"
		if nfail > 0 {
			failCell = cRed.Render(fmt.Sprintf("%d", nfail))
		}
		st := statusStyle(mb.Status).Render(fmt.Sprintf("%-*s", stW, mb.Status))
		b.WriteString(fmt.Sprintf("%*d  %-*s %s %-*s %-*s %5s %*s %*s\n",
			idxW, mb.Index, mwidth, clip(mb.Label, mwidth), st,
			flW, fl, msW, clip(msgs, msW), failCell, spW, speed, elW, el))
		shown++
		if detail && mb.Status == "RUNNING" && shown < h {
			bar := progressBar(mb.BytesDone, mb.BytesTotal, mb.MsgsDone, mb.MsgsTotal, 24)
			b.WriteString("      " + bar + "  " + cCyan.Render(clip(mb.Op, w-40)) + "\n")
			shown++
		}
		if mb.Status == "WARNINGS" && shown < h {
			b.WriteString("      " + cYellow.Render(clip(fmt.Sprintf(
				"completed with warnings — %d in the Failed Message Registry "+
					"(mailferry failed / retry-failed)", nfail), w-8)) + "\n")
			shown++
		}
	}
	return b.String()
}

func (m *Model) aggFooter(w int) string {
	agg := m.snap.Agg()
	br, mr := m.displayRates()
	c := agg.Counts
	done := c["SUCCESS"] + c["PARTIAL"] + c["FAILED"] + c["CANCELLED"] + c["WARNINGS"] + c["STALE"]
	frag := fmt.Sprintf("Done %d/%d   %s   %s   %s", done, len(m.snap.Mailboxes),
		cGreen.Render(fmt.Sprintf("%d ok", c["SUCCESS"])),
		cYellow.Render(fmt.Sprintf("%d partial", c["PARTIAL"])),
		cRed.Render(fmt.Sprintf("%d failed", c["FAILED"])))
	if c["WARNINGS"] > 0 {
		frag += "   " + cYellow.Render(fmt.Sprintf("%d with warnings", c["WARNINGS"]))
	}
	if c["REMOTE"] > 0 {
		frag += "   " + cCyan.Render(fmt.Sprintf("%d on other workers", c["REMOTE"]))
	}
	line2 := fmt.Sprintf("Msgs %d/%d (%s)   Data %s   Rate %s/s (%.1f msg/s)   New %d   Adopted %d",
		agg.MsgsDone, agg.MsgsTotal, util.Pct(agg.MsgsDone, agg.MsgsTotal),
		util.FmtBytes(float64(agg.BytesDone)), util.FmtBytes(br), mr, agg.Appended, agg.Adopted)
	if agg.FailedMsgs > 0 {
		line2 += "   " + cRed.Render(fmt.Sprintf("MsgFail %d", agg.FailedMsgs))
	}
	st := m.snap.Stalls
	line3 := ""
	if st[0] > 0 {
		line3 = cYellow.Render(fmt.Sprintf("Stalls %d (recovered %d)", st[0], st[1]))
	}
	out := strings.Repeat("─", w) + "\n" + clip(frag, w) + "\n" + clip(line2, w)
	if line3 != "" {
		out += "\n" + line3
	}
	return out
}

func progressBar(bd, bt, md, mt int64, width int) string {
	total, val := bt, bd
	if total == 0 {
		total, val = mt, md
	}
	frac := 0.0
	if total > 0 {
		frac = float64(val) / float64(total)
		if frac > 1 {
			frac = 1
		}
	}
	fill := int(float64(width) * frac)
	return strings.Repeat("█", fill) + strings.Repeat("░", width-fill) +
		fmt.Sprintf(" %4s", util.Pct(val, total))
}

// -------------------------------------------------------------- helpers --

func center(s string, w int) string {
	vis := lipgloss.Width(s)
	if vis >= w {
		return s
	}
	left := (w - vis) / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", w-vis-left)
}

func clip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	// trim on visible width, preserving styling best-effort
	out := ""
	cur := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if cur+rw > w {
			break
		}
		out += string(r)
		cur += rw
	}
	return out
}

func (m *Model) tooSmall(w, h int) string {
	agg := m.snap.Agg()
	msg := []string{
		identity.BannerLine(),
		"",
		cYellow.Render("Terminal too small to draw the dashboard."),
		fmt.Sprintf("Please enlarge to at least %dx%d (currently %dx%d).", minCols, minRows, w, h),
		"",
		"The migration is still running in the background —",
		fmt.Sprintf("%d msgs / %s synced so far.", agg.MsgsDone, util.FmtBytes(float64(agg.BytesDone))),
		"",
		cDim.Render("Ctrl+C stops gracefully."),
	}
	var b strings.Builder
	pad := (h - len(msg)) / 2
	for i := 0; i < pad; i++ {
		b.WriteString("\n")
	}
	for _, l := range msg {
		b.WriteString(center(l, w) + "\n")
	}
	return b.String()
}

func (m *Model) shutdownView(w, h int) string {
	title := "Gracefully Stopping " + identity.Product
	if m.forced {
		title = "Force-stopping " + identity.Product
	}
	elapsed := int(time.Since(m.shutdownAt).Seconds())
	inner := 52
	var lines []string
	lines = append(lines, cBold.Render(center(title, inner)))
	lines = append(lines, cDim.Render(center(identity.Slogan, inner)))
	lines = append(lines, "")
	allDone := m.shutPhase >= len(shutdownPhases)
	for i, p := range shutdownPhases {
		switch {
		case i < m.shutPhase || allDone:
			lines = append(lines, "  "+cGreen.Render("✓")+"  "+p)
		case i == m.shutPhase:
			lines = append(lines, "  "+cYellow.Render("▶")+"  "+cBold.Render(p+"..."))
		default:
			lines = append(lines, "  "+cDim.Render("·  "+p))
		}
	}
	lines = append(lines, "")
	if allDone {
		lines = append(lines, cGreen.Render(center("Shutdown complete — state saved.", inner)))
		lines = append(lines, cDim.Render(center("Re-run the same command to resume.", inner)))
	} else {
		lines = append(lines, cYellow.Render(center("Please wait. Do not close this terminal.", inner)))
		lines = append(lines, cDim.Render(center("Ctrl+C again forces an immediate exit.", inner)))
	}
	lines = append(lines, cDim.Render(center(fmt.Sprintf("elapsed %s", util.FmtDHMS(float64(elapsed))), inner)))

	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).
		Width(inner).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, box)
}

// mailboxRows applies the Mailboxes view's filter (f), sort (s) and search.
func (m *Model) mailboxRows() []engine.MBValues {
	rows := m.snap.Mailboxes
	if q := strings.ToLower(m.viewSearch(vMailboxes)); q != "" {
		var out []engine.MBValues
		for _, r := range rows {
			if strings.Contains(strings.ToLower(r.Label), q) ||
				strings.Contains(strings.ToLower(r.Status), q) {
				out = append(out, r)
			}
		}
		rows = out
	}
	switch m.mailFilter {
	case 1: // problems
		var out []engine.MBValues
		for _, r := range rows {
			switch r.Status {
			case "FAILED", "PARTIAL", "STALE", "WARNINGS", "RETRYING":
				out = append(out, r)
			}
		}
		rows = out
	case 2: // active
		var out []engine.MBValues
		for _, r := range rows {
			switch r.Status {
			case "RUNNING", "RETRYING", "REMOTE":
				out = append(out, r)
			}
		}
		rows = out
	}
	if m.mailSort > 0 {
		rows = append([]engine.MBValues(nil), rows...)
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i], rows[j]
			switch m.mailSort {
			case 1:
				if a.Status != b.Status {
					return a.Status < b.Status
				}
			case 2:
				ap, bp := pctOf(a), pctOf(b)
				if ap != bp {
					return ap > bp
				}
			}
			return a.Index < b.Index
		})
	}
	return rows
}

func pctOf(m engine.MBValues) float64 {
	if m.MsgsTotal <= 0 {
		return 0
	}
	return float64(m.MsgsDone) / float64(m.MsgsTotal)
}

// popupBox renders the modal detail box over the current frame.
func (m *Model) popupBox(w, h int) string {
	inner := 60
	if inner > w-6 {
		inner = w - 6
	}
	var lines []string
	lines = append(lines, cBold.Render(center(m.popup.Title, inner)))
	lines = append(lines, "")
	for _, l := range m.popup.Lines {
		lines = append(lines, " "+clip(l, inner-2))
	}
	lines = append(lines, "")
	lines = append(lines, cDim.Render(center("Esc / ⏎ close", inner)))
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).
		Width(inner).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, box)
}
