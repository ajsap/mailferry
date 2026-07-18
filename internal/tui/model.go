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

package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ajsap/mailferry/v2/internal/engine"
	"github.com/ajsap/mailferry/v2/internal/sysmon"
	"github.com/ajsap/mailferry/v2/internal/util"
	tea "github.com/charmbracelet/bubbletea"
)

// View identifiers mirror the familiar F1–F10 model.
const (
	vDashboard = iota
	vWorkers
	vMailboxes
	vQueue
	vHistory
	vErrors
	vPerformance
	vLogs
	vSettings
	vHelp
	numViews
)

var viewNames = [numViews]string{
	"Dashboard", "Workers", "Mailboxes", "Queue", "History", "Errors",
	"Performance", "Logs", "Settings", "Help",
}

// tickMsg drives periodic re-render (display cadence only — the engine is
// never gated on it).
type tickMsg time.Time

// busMsg wakes the model when the engine publishes an event.
type busMsg struct{}

// doneMsg is sent when the migration finishes so the program can exit.
type doneMsg struct{}

// phaseMsg advances the graceful-shutdown dialog through its phases.
type phaseMsg struct{}

// shutdownPhases — same ordered phases as the classic TUI dialog.
var shutdownPhases = []string{
	"Stopping the scheduler",
	"Waiting for active workers",
	"Saving migration state",
	"Flushing logs",
	"Closing IMAP connections",
	"Releasing resources",
}

// popupState is a modal detail box (Enter opens, Esc/Enter/q closes).
type popupState struct {
	Title string
	Lines []string
}

// Model is the Bubble Tea model. It owns only presentation state; all
// migration state lives behind the engine (Stats snapshot + Bus).
type Model struct {
	stats   *engine.Stats
	bus     *engine.Bus
	cancel  context.CancelFunc // graceful stop (first Ctrl+C)
	hardKC  context.CancelFunc // force stop (second Ctrl+C)
	refresh time.Duration
	sub     <-chan struct{}
	doneCh  <-chan struct{}

	width, height int
	active        int
	snap          engine.Snapshot

	// per-view scroll / selection state
	mailSel    int
	histSel    int
	histFollow bool
	logTop     int
	logFollow  bool
	errSel     int
	helpTop    int
	mailSort   int // 0 index · 1 status · 2 progress
	mailFilter int // 0 all · 1 problems · 2 active

	// global display state
	frozen     bool
	searching  bool
	search     string
	searchView int
	popup      *popupState
	flash      string

	// system telemetry (informational only)
	mon      *sysmon.Mon
	sys      sysmon.Snap
	lastPerf time.Time
	histRate []float64 // 60s ring buffers for the Performance sparklines
	histMsgs []float64
	histCPU  []float64

	// shutdown dialog
	shuttingDown bool
	shutdownAt   time.Time
	forced       bool
	shutPhase    int
	shutHold     bool

	// rolling rates (wire-based, like the classic dashboard)
	grate     *engine.RateTracker
	gwire     *engine.RateTracker
	mbWire    map[int]*engine.RateTracker
	mbPay     map[int]*engine.RateTracker
	lastRates [2]float64

	quitting bool
}

func New(stats *engine.Stats, bus *engine.Bus, cancel, hardKC context.CancelFunc,
	refresh time.Duration, doneCh <-chan struct{}) *Model {
	if refresh <= 0 {
		refresh = 250 * time.Millisecond
	}
	mon := sysmon.New()
	mon.Start()
	return &Model{
		stats: stats, bus: bus, cancel: cancel, hardKC: hardKC, refresh: refresh,
		sub: bus.Subscribe(), doneCh: doneCh,
		active: vDashboard, histFollow: true, logFollow: true, searchView: -1,
		snap: stats.Snapshot(), mon: mon,
		grate: engine.NewRateTracker(), gwire: engine.NewRateTracker(),
		mbWire: map[int]*engine.RateTracker{}, mbPay: map[int]*engine.RateTracker{},
	}
}

func (m *Model) stopSysmon() {
	if m.mon != nil {
		m.mon.Stop()
	}
}

// phaseCmd schedules the next shutdown-dialog phase tick.
func (m *Model) phaseCmd() tea.Cmd {
	return tea.Tick(280*time.Millisecond, func(time.Time) tea.Msg { return phaseMsg{} })
}

// samplePerf maintains the 60-second Performance histories at ~1 Hz.
func (m *Model) samplePerf() {
	now := time.Now()
	if now.Sub(m.lastPerf) < time.Second {
		return
	}
	m.lastPerf = now
	if m.mon != nil {
		m.sys = m.mon.Snapshot()
	}
	push := func(dst []float64, v float64) []float64 {
		dst = append(dst, v)
		if len(dst) > 60 {
			dst = dst[len(dst)-60:]
		}
		return dst
	}
	br, mr := m.displayRates()
	m.histRate = push(m.histRate, br)
	m.histMsgs = push(m.histMsgs, mr)
	cpu := 0.0
	if m.sys.HasCPU {
		cpu = m.sys.CPU
	}
	m.histCPU = push(m.histCPU, cpu)
}

// ------------------------------------------------------- detail popups --

func (m *Model) openHistoryPopup(e engine.HistoryEntry) {
	lines := []string{
		"Time     " + e.TS.Format("2006-01-02 15:04:05"),
		"Event    " + e.Event,
		"Status   " + e.Status,
	}
	if e.Mailbox != "" && e.Mailbox != "-" {
		lines = append(lines, "Mailbox  "+e.Mailbox)
	}
	if e.Details != "" {
		lines = append(lines, "")
		lines = append(lines, wrapLines("Details  ", e.Details, 56)...)
	}
	m.popup = &popupState{Title: "History Event", Lines: lines}
}

func (m *Model) openErrorPopup(e engine.LogEntry) {
	lines := []string{
		"Time      " + e.TS.Format("2006-01-02 15:04:05"),
		"Severity  " + e.Severity,
		"Mailbox   " + orEmpty(e.Mailbox),
		"",
	}
	lines = append(lines, wrapLines("", e.Message, 56)...)
	m.popup = &popupState{Title: "Error Detail", Lines: lines}
}

func (m *Model) openMailboxPopup(mb engine.MBValues) {
	el := "-"
	if !mb.Start.IsZero() {
		end := mb.End
		if end.IsZero() {
			end = time.Now()
		}
		el = util.FmtDHMS(end.Sub(mb.Start).Seconds())
	}
	lines := []string{
		"Status      " + mb.Status,
		fmt.Sprintf("Source      %s @ %s (%s)", mb.Label, mb.Src.Host, mb.Src.ConnState),
		fmt.Sprintf("Destination %s @ %s (%s)", orEmpty(mb.Label2), mb.Dst.Host, mb.Dst.ConnState),
		fmt.Sprintf("Folders     %d/%d", mb.FolderIndex, mb.FoldersTotal),
		fmt.Sprintf("Messages    %d/%d · %s of %s", mb.MsgsDone, mb.MsgsTotal,
			util.FmtBytes(float64(mb.BytesDone)), util.FmtBytes(float64(mb.BytesTotal))),
		fmt.Sprintf("Adopted     %d · appended %d · pre-existing %d",
			mb.Adopted, mb.Appended, mb.Dst.Existing),
		fmt.Sprintf("Failed      %d · skipped %d · reconnects %d",
			mb.FailedMsgs, mb.Skipped, mb.Src.Reconnects+mb.Dst.Reconnects),
		"Elapsed     " + el,
	}
	if mb.Op != "" {
		lines = append(lines, "Operation   "+mb.Op)
	}
	if mb.Error != "" {
		lines = append(lines, "")
		lines = append(lines, wrapLines("Error  ", mb.Error, 56)...)
	}
	m.popup = &popupState{Title: "Mailbox — " + mb.Label, Lines: lines}
}

// wrapLines wraps text at width, prefixing the first line with head.
func wrapLines(head, text string, width int) []string {
	var out []string
	line := head
	for _, word := range strings.Fields(text) {
		if len(line)+len(word)+1 > width && line != "" && line != head {
			out = append(out, line)
			line = strings.Repeat(" ", len(head))
		}
		if line == "" || strings.TrimSpace(line) == "" && line != head {
			line += word
		} else {
			line += " " + word
		}
	}
	if strings.TrimSpace(line) != "" {
		out = append(out, line)
	}
	if len(out) == 0 {
		out = []string{head + text}
	}
	return out
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.tickCmd(), m.waitBus(), m.waitDone())
}

func (m *Model) tickCmd() tea.Cmd {
	return tea.Tick(m.refresh, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) waitBus() tea.Cmd {
	return func() tea.Msg {
		<-m.sub
		return busMsg{}
	}
}

func (m *Model) waitDone() tea.Cmd {
	return func() tea.Msg {
		<-m.doneCh
		return doneMsg{}
	}
}

func (m *Model) sampleRates() {
	now := m.snap.TS
	agg := m.snap.Agg()
	m.grate.Update(now, agg.BytesDone, agg.MsgsDone)
	wire := agg.WireRX
	if agg.WireTX > wire {
		wire = agg.WireTX
	}
	m.gwire.Update(now, wire, 0)
	for _, mb := range m.snap.Mailboxes {
		if m.mbWire[mb.Index] == nil {
			m.mbWire[mb.Index] = engine.NewRateTracker()
			m.mbPay[mb.Index] = engine.NewRateTracker()
		}
		w := mb.Src.RXBytes + mb.Src.TXBytes
		if d := mb.Dst.RXBytes + mb.Dst.TXBytes; d > w {
			w = d
		}
		m.mbWire[mb.Index].Update(now, w, 0)
		m.mbPay[mb.Index].Update(now, mb.BytesDone, mb.MsgsDone)
	}
	wb, _ := m.gwire.Rates()
	pb, pm := m.grate.Rates()
	if pb > wb {
		wb = pb
	}
	m.lastRates = [2]float64{wb, pm}
}

func (m *Model) displayRates() (float64, float64) { return m.lastRates[0], m.lastRates[1] }

func (m *Model) mbSpeed(idx int) float64 {
	var wr, pr float64
	if t := m.mbWire[idx]; t != nil {
		wr, _ = t.Rates()
	}
	if t := m.mbPay[idx]; t != nil {
		pr, _ = t.Rates()
	}
	if pr > wr {
		return pr
	}
	return wr
}

func (m *Model) eta() (float64, bool) {
	agg := m.snap.Agg()
	remB := agg.BytesTotal - agg.BytesDone
	remM := agg.MsgsTotal - agg.MsgsDone
	if e, ok := m.grate.ETA(remB, remM); ok {
		return e, true
	}
	if remB > 0 {
		if wr, _ := m.gwire.Rates(); wr > 1024 {
			return float64(remB) / wr, true
		}
	}
	return 0, false
}
