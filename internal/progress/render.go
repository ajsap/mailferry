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

// Package progress: the classic MailFerry dashboard — same banner, same
// layout, same colours as v1.x — with alternate-screen differential
// repaint. Non-TTY output falls back to timestamped status lines.
package progress

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ajsap/mailferry/v2/internal/engine"
	"github.com/ajsap/mailferry/v2/internal/identity"
	"github.com/ajsap/mailferry/v2/internal/util"
	"golang.org/x/term"
)

var IsTTY = term.IsTerminal(int(os.Stdout.Fd()))

var colors = map[string]string{
	"reset": "\033[0m", "dim": "\033[2m", "bold": "\033[1m", "red": "\033[31m",
	"green": "\033[32m", "yellow": "\033[33m", "cyan": "\033[36m", "white": "\033[37m",
}

var statusColor = map[string]string{
	"QUEUED": "dim", "RUNNING": "yellow", "RETRYING": "yellow",
	"SUCCESS": "green", "PARTIAL": "yellow", "FAILED": "red",
	"CANCELLED": "yellow", "SKIPPED": "cyan", "WARNINGS": "yellow",
	"STALE": "red", "REMOTE": "cyan",
}

func C(text, color string) string {
	if !IsTTY {
		return text
	}
	return colors[color] + text + colors["reset"]
}

func visibleLen(s string) int {
	n, i := 0, 0
	for i < len(s) {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			i = j + 1
			continue
		}
		_, sz := decodeRune(s[i:])
		n++
		i += sz
	}
	return n
}

func decodeRune(s string) (rune, int) {
	r := []rune(s[:min(len(s), 4)])
	if len(r) == 0 {
		return 0, 1
	}
	return r[0], len(string(r[0]))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Renderer draws the classic dashboard on a cadence until stopped.
type Renderer struct {
	Stats   *engine.Stats
	Session func(string)
	Refresh time.Duration
	grate   *engine.RateTracker
	gwire   *engine.RateTracker
	mbWire  map[int]*engine.RateTracker
	mbRate  map[int]*engine.RateTracker
	stop    chan struct{}
	done    chan struct{}
	prev    []string
	active  bool
	mu      sync.Mutex
}

func NewRenderer(stats *engine.Stats, session func(string), refresh time.Duration) *Renderer {
	return &Renderer{Stats: stats, Session: session, Refresh: refresh,
		grate: engine.NewRateTracker(), gwire: engine.NewRateTracker(),
		mbWire: map[int]*engine.RateTracker{}, mbRate: map[int]*engine.RateTracker{},
		stop: make(chan struct{}), done: make(chan struct{})}
}

func (r *Renderer) Start() { go r.loop() }

func (r *Renderer) Stop(final bool) {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	<-r.done
	if r.active {
		fmt.Print("\033[?25h\033[?1049l") // cursor on, main screen
		r.active = false
		if final {
			for _, l := range r.render(r.Stats.Snapshot(), 0) {
				fmt.Println(l)
			}
		}
	}
}

func (r *Renderer) loop() {
	defer close(r.done)
	interval := r.Refresh
	if !IsTTY {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			snap := r.Stats.Snapshot()
			r.tick(snap)
			if IsTTY {
				r.paint(r.render(snap, termRows()))
			} else {
				fmt.Println(r.statusLine(snap))
			}
		}
	}
}

func (r *Renderer) tick(snap engine.Snapshot) {
	agg := snap.Agg()
	now := snap.TS
	r.grate.Update(now, agg.BytesDone, agg.MsgsDone)
	wire := agg.WireRX
	if agg.WireTX > wire {
		wire = agg.WireTX
	}
	r.gwire.Update(now, wire, 0)
	for _, m := range snap.Mailboxes {
		if r.mbWire[m.Index] == nil {
			r.mbWire[m.Index] = engine.NewRateTracker()
			r.mbRate[m.Index] = engine.NewRateTracker()
		}
		w := m.Src.RXBytes + m.Src.TXBytes
		if d := m.Dst.RXBytes + m.Dst.TXBytes; d > w {
			w = d
		}
		r.mbWire[m.Index].Update(now, w, 0)
		r.mbRate[m.Index].Update(now, m.BytesDone, m.MsgsDone)
	}
}

func (r *Renderer) mbSpeed(idx int) float64 {
	var wr, pr float64
	if t := r.mbWire[idx]; t != nil {
		wr, _ = t.Rates()
	}
	if t := r.mbRate[idx]; t != nil {
		pr, _ = t.Rates()
	}
	if pr > wr {
		return pr
	}
	return wr
}

func termRows() int {
	if !IsTTY {
		return 0
	}
	_, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || rows <= 0 {
		return 30
	}
	return rows
}

func termCols() int {
	cols, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || cols <= 0 {
		return 100
	}
	return cols
}

// paint: alternate screen + per-row differential repaint.
func (r *Renderer) paint(lines []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		fmt.Print("\033[?1049h\033[?25l\033[2J")
		r.active = true
		r.prev = nil
	}
	var b strings.Builder
	for i, l := range lines {
		if i < len(r.prev) && r.prev[i] == l {
			continue
		}
		b.WriteString(fmt.Sprintf("\033[%d;1H\033[2K%s", i+1, l))
	}
	for i := len(lines); i < len(r.prev); i++ {
		b.WriteString(fmt.Sprintf("\033[%d;1H\033[2K", i+1))
	}
	fmt.Print(b.String())
	r.prev = lines
}

func (r *Renderer) statusLine(snap engine.Snapshot) string {
	agg := snap.Agg()
	br, _ := r.displayRates()
	running := agg.Counts["RUNNING"] + agg.Counts["RETRYING"]
	done := agg.Counts["SUCCESS"] + agg.Counts["PARTIAL"] + agg.Counts["FAILED"] +
		agg.Counts["CANCELLED"] + agg.Counts["WARNINGS"]
	eta := "-"
	if e, ok := r.eta(agg); ok {
		eta = util.FmtDHMS(e)
	}
	return fmt.Sprintf("[%s] running=%d queued=%d done=%d/%d ok=%d warnings=%d failed=%d "+
		"msgs=%d/%d(%s) data=%s/%s rate=%s/s new=%d adopted=%d rec=%d eta=%s runtime=%s",
		util.NowISO(), running, agg.Counts["QUEUED"], done, len(snap.Mailboxes),
		agg.Counts["SUCCESS"], agg.Counts["WARNINGS"], agg.Counts["FAILED"],
		agg.MsgsDone, agg.MsgsTotal, util.Pct(agg.MsgsDone, agg.MsgsTotal),
		util.FmtBytes(float64(agg.BytesDone)), util.FmtBytes(float64(agg.BytesTotal)),
		util.FmtBytes(br), agg.Appended, agg.Adopted, agg.Reconnects, eta,
		util.FmtDHMS(time.Since(snap.BatchStart).Seconds()))
}

func (r *Renderer) displayRates() (float64, float64) {
	wb, _ := r.gwire.Rates()
	pb, pm := r.grate.Rates()
	if pb > wb {
		wb = pb
	}
	return wb, pm
}

func (r *Renderer) eta(agg engine.Agg) (float64, bool) {
	remB := agg.BytesTotal - agg.BytesDone
	remM := agg.MsgsTotal - agg.MsgsDone
	if e, ok := r.grate.ETA(remB, remM); ok {
		return e, true
	}
	if remB > 0 {
		if wr, _ := r.gwire.Rates(); wr > 1024 {
			return float64(remB) / wr, true
		}
	}
	return 0, false
}

// render draws the full classic dashboard.
func (r *Renderer) render(snap engine.Snapshot, rows int) []string {
	W := termCols() - 2
	if W > 120 {
		W = 120
	}
	if W < 70 {
		W = 70
	}
	agg := snap.Agg()
	var out []string
	line := strings.Repeat("═", W)
	out = append(out, "╔"+line+"╗")
	out = append(out, "║"+centerPad(identity.BannerLine(), W)+"║")
	out = append(out, "║"+centerPad(identity.Slogan, W)+"║")
	out = append(out, "╚"+line+"╝")

	left := []string{
		fmt.Sprintf("%-14s: %s", "CSV File", snap.CSVFile),
		fmt.Sprintf("%-14s: %s", "State Database", snap.DBPath),
		fmt.Sprintf("%-14s: %s", "Logs", snap.LogsDir),
		fmt.Sprintf("%-14s: %s", "Mode", snap.Mode),
		fmt.Sprintf("%-14s: %d/%d", "Workers",
			agg.Counts["RUNNING"]+agg.Counts["RETRYING"], snap.Workers),
		fmt.Sprintf("%-14s: %s", "Runtime", util.FmtDHMS(time.Since(snap.BatchStart).Seconds())),
	}
	right := []string{
		fmt.Sprintf("%-12s: %s / %s", "Wire RX/TX",
			util.FmtBytes(float64(agg.WireRX)), util.FmtBytes(float64(agg.WireTX))),
	}
	br, mr := r.displayRates()
	right = append(right, fmt.Sprintf("%-12s: %s/s (%.1f msg/s)", "Rate",
		util.FmtBytes(br), mr))
	if e, ok := r.eta(agg); ok {
		right = append(right, fmt.Sprintf("%-12s: %s", "Batch ETA", util.FmtDHMS(e)))
	} else {
		right = append(right, fmt.Sprintf("%-12s: -", "Batch ETA"))
	}
	rw := 0
	for _, s := range right {
		if len(s) > rw {
			rw = len(s)
		}
	}
	lw := W + 2 - rw - 3
	n := len(left)
	if len(right) > n {
		n = len(right)
	}
	for i := 0; i < n; i++ {
		l, rg := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			rg = right[i]
		}
		if lw >= 32 {
			out = append(out, strings.TrimRight(fmt.Sprintf("%-*s   %s", lw, util.Truncate(l, lw), rg), " "))
		} else {
			out = append(out, l)
		}
	}
	out = append(out, strings.Repeat("─", W+2))

	// mailbox table
	const idxW, stW, flW, msW, pctW, spW, elW = 3, 8, 7, 17, 5, 11, 16
	fixed := idxW + 2 + stW + 1 + flW + 1 + msW + 1 + pctW + 1 + spW + 1 + elW
	mw := W - fixed
	if mw < 14 {
		mw = 14
	}
	out = append(out, fmt.Sprintf("%*s  %-*s %-*s %-*s %-*s %*s %*s %*s",
		idxW, "#", mw, "Mailbox", stW, "Status", flW, "Fldr", msW, "Msgs",
		pctW, "Pct", spW, "Speed", elW, "Elapsed"))
	out = append(out, strings.Repeat("─", W+2))

	// height budget: shrink detail level to fit
	level := 2
	if rows > 0 {
		budget := rows - len(out) - 6
		if budget < len(snap.Mailboxes)*3 {
			level = 1
		}
		if budget < len(snap.Mailboxes)*2 {
			level = 0
		}
	}

	for _, m := range snap.Mailboxes {
		doneAll := m.Status == "SUCCESS" || m.Status == "PARTIAL" || m.Status == "FAILED" ||
			m.Status == "CANCELLED" || m.Status == "WARNINGS" || m.Status == "STALE"
		active := m.Status == "RUNNING" || m.Status == "RETRYING" || m.Status == "REMOTE"
		elapsed := "-"
		if !m.Start.IsZero() {
			end := m.End
			if end.IsZero() {
				end = time.Now()
			}
			elapsed = util.FmtDHMS(end.Sub(m.Start).Seconds())
		}
		pct := util.Pct(m.BytesDone, m.BytesTotal)
		if m.BytesTotal == 0 {
			pct = util.Pct(m.MsgsDone, m.MsgsTotal)
		}
		fl := "-"
		if m.FoldersTotal > 0 {
			fl = fmt.Sprintf("%d/%d", m.FolderIndex, m.FoldersTotal)
		}
		msgs := "-"
		if m.MsgsTotal > 0 {
			msgs = fmt.Sprintf("%d/%d", m.MsgsDone, m.MsgsTotal)
		}
		speed := "-"
		if active {
			speed = util.FmtBytes(r.mbSpeed(m.Index)) + "/s"
		}
		stColored := C(fmt.Sprintf("%-*s", stW, m.Status), statusColor[m.Status])
		out = append(out, fmt.Sprintf("%*d  %-*s %s %-*s %-*s %*s %*s %*s",
			idxW, m.Index, mw, util.Truncate(m.Label, mw), stColored,
			flW, fl, msW, util.Truncate(msgs, msW), pctW, pct, spW, speed, elW, elapsed))
		if m.Status == "RUNNING" && level >= 1 {
			bw := 30
			total, val := m.BytesTotal, m.BytesDone
			if total == 0 {
				total, val = m.MsgsTotal, m.MsgsDone
			}
			frac := 0.0
			if total > 0 {
				frac = float64(val) / float64(total)
				if frac > 1 {
					frac = 1
				}
			}
			fill := int(float64(bw) * frac)
			bar := strings.Repeat("█", fill) + strings.Repeat("░", bw-fill)
			out = append(out, fmt.Sprintf("      %-20s %s %4s  %s",
				util.Truncate(m.Folder, 20), bar, util.Pct(val, total),
				C(util.Truncate(m.Op, W-60), "cyan")))
			if level >= 2 {
				s, d := m.Src, m.Dst
				out = append(out, "      "+C(util.Truncate(fmt.Sprintf(
					"%-12s%s [%s] %s  ↓%s%s", "Source", util.Truncate(s.Host, 24),
					strings.Join(s.Caps, ","), s.ConnState, util.FmtBytes(float64(s.RXBytes)),
					reconnSuffix(s.Reconnects)), W-6), "dim"))
				out = append(out, "      "+C(util.Truncate(fmt.Sprintf(
					"%-12s%s [%s] %s  ↑%s  pre-existing %d  new %d  adopted %d",
					"Destination", util.Truncate(d.Host, 24), strings.Join(d.Caps, ","),
					d.ConnState, util.FmtBytes(float64(d.TXBytes)), d.Existing,
					m.Appended, m.Adopted), W-6), "dim"))
			}
		} else if doneAll && m.Error != "" && level >= 1 {
			col := "red"
			if m.Status == "PARTIAL" || m.Status == "WARNINGS" {
				col = "yellow"
			}
			out = append(out, "      "+C(util.Truncate(m.Error, W-6), col))
		}
	}

	out = append(out, strings.Repeat("─", W+2))
	doneN := agg.Counts["SUCCESS"] + agg.Counts["PARTIAL"] + agg.Counts["FAILED"] +
		agg.Counts["CANCELLED"] + agg.Counts["WARNINGS"] + agg.Counts["STALE"]
	frag := fmt.Sprintf("Done %d/%d   %s   %s   %s", doneN, len(snap.Mailboxes),
		C(fmt.Sprintf("%d ok", agg.Counts["SUCCESS"]), "green"),
		C(fmt.Sprintf("%d partial", agg.Counts["PARTIAL"]), "yellow"),
		C(fmt.Sprintf("%d failed", agg.Counts["FAILED"]), "red"))
	if agg.Counts["WARNINGS"] > 0 {
		frag += "   " + C(fmt.Sprintf("%d with warnings", agg.Counts["WARNINGS"]), "yellow")
	}
	out = append(out, frag)
	out = append(out, fmt.Sprintf("Msgs %d/%d (%s)   Data %s/%s (%s)   Rate %s/s (%.1f msg/s)   New %d   Adopted %d   Reconn %d",
		agg.MsgsDone, agg.MsgsTotal, util.Pct(agg.MsgsDone, agg.MsgsTotal),
		util.FmtBytes(float64(agg.BytesDone)), util.FmtBytes(float64(agg.BytesTotal)),
		util.Pct(agg.BytesDone, agg.BytesTotal), util.FmtBytes(br), mr,
		agg.Appended, agg.Adopted, agg.Reconnects))
	eta := "-"
	if e, ok := r.eta(agg); ok {
		eta = util.FmtDHMS(e)
	}
	out = append(out, "Batch ETA: "+eta+"   "+C("Ctrl+C stops gracefully — state is always consistent", "dim"))
	return out
}

func reconnSuffix(n int64) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("  reconnects %d", n)
}

func centerPad(s string, w int) string {
	v := visibleLen(s)
	if v >= w {
		return s
	}
	left := (w - v) / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", w-v-left)
}
