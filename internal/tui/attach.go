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

// A compact, READ-ONLY live monitor for `mailferry attach`. It is a small,
// self-contained Bubble Tea model — the main dashboard Model is untouched —
// that renders periodic snapshots of the shared State Database plus a tail of
// the session log. It reuses the existing styles and identity banner.
//
// Why polling, not IPC: the State Database is SQLite in WAL mode, where any
// number of readers can proceed concurrently with a single writer without
// blocking it. A 1 Hz status-grade poll is negligible contention, and it
// means attach needs NO fragile IPC channel to the live workers — it takes no
// lease, registers no worker, and writes nothing. Detaching can therefore
// never affect a running migration.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ajsap/mailferry/v2/internal/identity"
	"github.com/ajsap/mailferry/v2/internal/state"
	"github.com/ajsap/mailferry/v2/internal/util"
	tea "github.com/charmbracelet/bubbletea"
)

// AttachSnapshot is the immutable view-model one poll produces. Building it is
// a pure function of a read-only StatusReport plus the session-log tail, so it
// is trivially unit-testable without a terminal.
type AttachSnapshot struct {
	RunID    string
	DBPath   string
	Filter   string // optional run-id filter for the title
	Report   state.StatusReport
	LogLines []string // last N session-log lines (already trimmed)
	LogPath  string
	LogFound bool
	Taken    time.Time
}

// AttachPoller is the read-only data layer behind the monitor. It owns a DB
// handle opened for reading only and the session-log path; Poll() never
// writes. Factored out so tests can exercise it directly.
type AttachPoller struct {
	db       *state.DB
	dbPath   string
	logPath  string
	filter   string
	workerTO float64
	tailN    int
}

// NewAttachPoller wraps an already-open (read-only) State Database handle.
// The caller owns closing db. logPath may be "" (then no log tail is shown).
func NewAttachPoller(db *state.DB, dbPath, logPath, filter string, workerTO float64, tailN int) *AttachPoller {
	if tailN <= 0 {
		tailN = 12
	}
	if workerTO <= 0 {
		workerTO = 60
	}
	return &AttachPoller{db: db, dbPath: dbPath, logPath: logPath,
		filter: filter, workerTO: workerTO, tailN: tailN}
}

// Poll takes one read-only snapshot: DB status + a fresh session-log tail.
// It never mutates the database or the log.
func (p *AttachPoller) Poll() AttachSnapshot {
	rep := p.db.Status(p.workerTO)
	lines, found := tailFile(p.logPath, p.tailN)
	return AttachSnapshot{
		RunID:    rep.LastRunID,
		DBPath:   p.dbPath,
		Filter:   p.filter,
		Report:   rep,
		LogLines: lines,
		LogPath:  p.logPath,
		LogFound: found,
		Taken:    time.Now(),
	}
}

// tailFile returns the last n lines of a file (best effort, read-only). The
// bool reports whether the file exists.
func tailFile(path string, n int) ([]string, bool) {
	if path == "" {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	raw := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(raw) > n {
		raw = raw[len(raw)-n:]
	}
	return raw, true
}

// RenderAttach renders a snapshot to a plain (uncoloured-safe) string. It is
// the single source of truth for both the TUI View() and the smoke tests, so
// a test can assert on the exact banner/label text.
func RenderAttach(s AttachSnapshot) string {
	var b strings.Builder
	b.WriteString(cBanner.Render(identity.BannerLine()))
	b.WriteString("\n")
	title := "ATTACH — read-only monitor"
	if s.RunID != "" {
		title += " · run " + s.RunID
	}
	if s.Filter != "" {
		title += " · filter " + s.Filter
	}
	title += " · db " + s.DBPath
	b.WriteString(cDim.Render(title))
	b.WriteString("\n\n")

	rep := s.Report
	kv := func(k, v string) { b.WriteString(fmt.Sprintf("%-20s %s\n", k, v)) }
	runState := "idle"
	if rep.LastRunStart > 0 {
		if rep.LastRunEnd > 0 {
			runState = "finished — " + rep.LastResult
		} else {
			runState = "running"
		}
	}
	kv("Run state", runState)
	kv("Running", fmt.Sprintf("%d", rep.Counts["RUNNING"]))
	kv("Queued", fmt.Sprintf("%d", rep.Counts["NEW"]+rep.Counts["QUEUED"]))
	kv("Completed", fmt.Sprintf("%d (+%d warnings)", rep.Counts["SUCCESS"], rep.Counts["WARNINGS"]))
	kv("Failed / partial", fmt.Sprintf("%d / %d", rep.Counts["FAILED"], rep.Counts["PARTIAL"]))
	kv("Outstanding failed", fmt.Sprintf("%d", rep.Outstanding))
	kv("Messages", fmt.Sprintf("%d / %d (%s)", rep.MsgsDone, rep.MsgsTotal,
		util.Pct(rep.MsgsDone, rep.MsgsTotal)))

	// Mailbox table.
	b.WriteString("\n")
	b.WriteString(cBold.Render("Mailboxes"))
	b.WriteString("\n")
	if len(rep.Mailboxes) == 0 {
		b.WriteString(cDim.Render("  (no mailboxes recorded yet)\n"))
	} else {
		b.WriteString(cDim.Render(fmt.Sprintf("  %-26s %-10s %-14s %s\n",
			"Mailbox", "Status", "Msgs", "Fail")))
		shown := 0
		for _, mb := range rep.Mailboxes {
			if shown >= 20 {
				b.WriteString(cDim.Render(fmt.Sprintf("  … +%d more\n", len(rep.Mailboxes)-shown)))
				break
			}
			msgs := fmt.Sprintf("%d/%d", mb.Done, mb.Total)
			failCell := "-"
			if mb.Failed > 0 {
				failCell = cRed.Render(fmt.Sprintf("%d", mb.Failed))
			}
			b.WriteString(fmt.Sprintf("  %-26s %s %-14s %s\n",
				clipA(mb.Label, 26), statusStyle(mb.Status).Render(fmt.Sprintf("%-10s", mb.Status)),
				msgs, failCell))
			shown++
		}
	}

	// Worker roster.
	b.WriteString("\n")
	b.WriteString(cBold.Render(fmt.Sprintf("Workers (%d)", len(rep.Workers))))
	b.WriteString("\n")
	if len(rep.Workers) == 0 {
		b.WriteString(cDim.Render("  (no workers currently registered)\n"))
	} else {
		for _, wk := range rep.Workers {
			hb := "-"
			if wk.Heartbeat > 0 {
				hb = fmt.Sprintf("%ds ago", int(wk.HBAge))
			}
			run := wk.RunID
			if run == "" {
				run = "-"
			}
			b.WriteString(fmt.Sprintf("  %-26s %-8s run %-18s %d mbx  heartbeat %s\n",
				state.ShortWorker(wk.ID), wk.Status, run, wk.Active, hb))
		}
	}

	// Session-log tail.
	b.WriteString("\n")
	b.WriteString(cBold.Render("Session log"))
	b.WriteString("\n")
	if !s.LogFound {
		b.WriteString(cDim.Render(fmt.Sprintf("  (no session log found at %s)\n", s.LogPath)))
	} else if len(s.LogLines) == 0 {
		b.WriteString(cDim.Render("  (session log is empty)\n"))
	} else {
		for _, ln := range s.LogLines {
			b.WriteString("  " + clipA(ln, 120) + "\n")
		}
	}
	return b.String()
}

func clipA(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// ---------------------------------------------------------------- model --

// attachTickMsg drives the 1 Hz read-only poll.
type attachTickMsg time.Time

// AttachModel is the small Bubble Tea model for the monitor. It holds only a
// poller and the latest snapshot — no engine, no bus, no writes.
type AttachModel struct {
	poller *AttachPoller
	snap   AttachSnapshot
	quit   bool
}

// NewAttachModel builds the monitor model around a read-only poller.
func NewAttachModel(poller *AttachPoller) *AttachModel {
	return &AttachModel{poller: poller, snap: poller.Poll()}
}

func (m *AttachModel) Init() tea.Cmd { return m.tick() }

func (m *AttachModel) tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return attachTickMsg(t) })
}

func (m *AttachModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case attachTickMsg:
		m.snap = m.poller.Poll()
		return m, m.tick()
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			// Detach is a no-op for the migration — we are only a reader.
			m.quit = true
			return m, tea.Quit
		}
	}
	return m, nil
}

// View renders the current snapshot plus a one-line footer.
func (m *AttachModel) View() string {
	if m.quit {
		return ""
	}
	return RenderAttach(m.snap) +
		"\n" + cDim.Render("q / Esc / Ctrl+C detach (read-only — the migration is never affected)") + "\n"
}
