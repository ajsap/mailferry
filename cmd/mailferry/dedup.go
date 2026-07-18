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

package main

// `mailferry dedup CSV` — explicit destination deduplication. SAFE BY
// DESIGN: it touches ONLY each row's Destination Server, the Source is never
// contacted, analysis is the default (a dry run that mutates nothing), and
// execution is reversible (quarantine + \Deleted, never a permanent delete).

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/engine"
	"github.com/ajsap/mailferry/v2/internal/identity"
	"github.com/ajsap/mailferry/v2/internal/paths"
	"github.com/ajsap/mailferry/v2/internal/progress"
	"github.com/ajsap/mailferry/v2/internal/state"
	"github.com/ajsap/mailferry/v2/internal/util"
)

func cmdDedup(rest []string) int {
	fs := flag.NewFlagSet("dedup", flag.ExitOnError)
	dbPath := fs.String("db", "", "State database path (default: the native per-user mailferry.db)")
	logsDir := fs.String("logs-dir", "", "Directory for the CSV report (default: the native per-OS location)")
	execute := fs.Bool("execute", false, "Quarantine duplicates (reversible move/copy+\\Deleted); default is analysis only")
	dryRun := fs.Bool("dry-run", false, "Analysis only — the default; accepted explicitly and never errors")
	tlsNoVerify := fs.Bool("tls-no-verify", false, "Disable TLS certificate verification")
	timeout := fs.Float64("timeout", 60, "Per-connection inactivity watchdog (s)")
	cfgPath := fs.String("config", "", "Path to mailferry.toml (already applied at startup)")
	fs.Parse(reorderArgs(fs, rest))
	_ = cfgPath // consumed by bootstrapConfig before dispatch
	_ = *dryRun // default behaviour; kept for muscle memory, never an error
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: mailferry dedup CSV [--execute] [--db PATH] [--logs-dir DIR]")
		return 2
	}
	specs, err := config.ParseCSV(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}

	fmt.Println(identity.BannerLine())
	fmt.Println(identity.Slogan)
	fmt.Println()
	mode := "ANALYSIS (dry run — nothing will be modified)"
	if *execute {
		mode = "EXECUTE (reversible quarantine — originals flagged \\Deleted, never expunged)"
	}
	fmt.Printf("Destination deduplication — %d mailbox(es) · %s\n", len(specs), mode)
	fmt.Println("Only each row's Destination Server is contacted; the Source is never touched.")
	fmt.Println()

	// The State Database is used for resume/skip bookkeeping and the live
	// lease check. Analysis can run without one (never creates it); execution
	// resolves it and creates the parent lazily like other operational commands.
	var db *state.DB
	dbLabel := "(none — analysis only)"
	if *execute {
		resolved, legacyHint := resolveStateDB(*dbPath)
		if legacyHint != "" {
			fmt.Fprintln(os.Stderr, legacyHint)
			return 1
		}
		if err := paths.EnsureParent(resolved); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			return 1
		}
		defer paths.RestrictDB(resolved)
		db, err = state.Open(resolved, false, 300)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			return 1
		}
		defer db.Close()
		dbLabel = resolved
	} else if *dbPath != "" {
		// Analysis with an explicit --db: consult it STRICTLY read-only (no
		// schema write) for the busy check when it exists; never create one.
		if _, statErr := os.Stat(*dbPath); statErr == nil {
			if d, oerr := state.OpenReadOnly(*dbPath); oerr == nil {
				db = d
				defer db.Close()
				dbLabel = *dbPath + " (read-only)"
			}
		}
	} else {
		// Analysis with the native DB: consult it STRICTLY read-only for the
		// busy check when it already exists, but never create it.
		if resolved, hint := resolveStateDB(""); hint == "" {
			if _, statErr := os.Stat(resolved); statErr == nil {
				if d, oerr := state.OpenReadOnly(resolved); oerr == nil {
					db = d
					defer db.Close()
					dbLabel = resolved + " (read-only)"
				}
			}
		}
	}
	fmt.Printf("State Database: %s\n\n", dbLabel)

	// Report CSV location: --logs-dir > native default. Created lazily.
	reportDir := *logsDir
	if reportDir == "" {
		reportDir = paths.Default().LogsDir
	}
	if err := paths.EnsureDir(reportDir); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	reportPath := filepath.Join(reportDir, "dedup_report.csv")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nStopping — progress is committed per message; re-run to resume.")
		cancel()
	}()

	var reports []engine.DedupReport
	var totalGroups, totalDups, totalQuar, totalDone int
	var totalBytes int64
	interrupted := false
	for _, spec := range specs {
		if ctx.Err() != nil {
			interrupted = true
			break
		}
		// Never disturb an active migration: if a live worker holds a fresh
		// lease on this mailbox, skip it and carry on.
		if db != nil {
			if busy, owner := db.IsMailboxLeased(spec.Key()); busy {
				fmt.Printf("  %-38s Mailbox busy: skipped (leased by %s)\n",
					spec.Dst.Label(), state.ShortWorker(owner))
				reports = append(reports, engine.DedupReport{Mailbox: spec.Dst.Label(),
					Skipped: true, SkipReason: "busy: leased by " + state.ShortWorker(owner)})
				continue
			}
		}
		mid := int64(0)
		if db != nil {
			mid = db.MailboxIDByKey(spec.Key())
			if mid == 0 && *execute {
				// Record the mailbox so dedup_state has a stable id to key on,
				// without disturbing migration status.
				id, _, uerr := db.UpsertMailbox(spec.Key(), spec.Src.Host, spec.Src.User,
					spec.Dst.Host, spec.Dst.User)
				if uerr == nil {
					mid = id
				}
			}
		}
		opt := engine.DedupOptions{Execute: *execute, TLSVerify: !*tlsNoVerify, Timeout: *timeout}
		rep, derr := engine.Dedup(ctx, spec, opt, db, mid, func(string) {})
		if derr != nil {
			if ctx.Err() != nil {
				interrupted = true
				break
			}
			fmt.Printf("  %-38s ERROR: %v\n", spec.Dst.Label(), derr)
			continue
		}
		reports = append(reports, rep)
		totalGroups += rep.TotalGroups
		totalDups += rep.TotalDups
		totalBytes += rep.TotalBytes
		totalQuar += rep.Quarantined
		totalDone += rep.AlreadyDone
		printMailboxReport(rep, *execute)
	}

	if err := writeDedupCSV(reportPath, reports); err != nil {
		fmt.Fprintln(os.Stderr, "note: could not write CSV report:", err)
	}

	// Grouping rationale + summary.
	fmt.Println()
	fmt.Println(progress.C("Grouping rationale: two messages are duplicates ONLY when the "+
		"normalised\nMessage-ID (non-empty) AND the exact size AND the fingerprint "+
		"(Date/From/To/\nSubject when the MID is absent) all match, within the same "+
		"folder. Anything\nweaker is retained — uncertain matches always favour keeping mail.", "cyan"))
	fmt.Println()
	fmt.Printf("%-26s: %d\n", "Duplicate groups", totalGroups)
	fmt.Printf("%-26s: %d\n", "Duplicate messages", totalDups)
	fmt.Printf("%-26s: %s\n", "Reclaimable (est.)", util.FmtBytes(float64(totalBytes)))
	if *execute {
		fmt.Printf("%-26s: %d\n", "Quarantined this run", totalQuar)
		if totalDone > 0 {
			fmt.Printf("%-26s: %d\n", "Already done (resumed)", totalDone)
		}
		fmt.Println()
		fmt.Println(progress.C("Duplicates were relocated to \""+engine.QuarantineRoot+
			"/<folder>\" on the destination.", "green"))
		fmt.Println(progress.C("Where the server lacked MOVE, originals were COPIED to quarantine "+
			"and flagged\n\\Deleted, not expunged — reversible; compact with your mail client "+
			"when satisfied.", "green"))
		fmt.Println(progress.C("Permanent deletion is deliberately NOT implemented in MailFerry v"+
			identity.Version+".", "yellow"))
	} else {
		fmt.Println()
		fmt.Println(progress.C("Analysis only — NOTHING was modified. Re-run with --execute to "+
			"quarantine\nduplicates reversibly (originals are flagged \\Deleted, never expunged).", "green"))
	}
	fmt.Printf("\nCSV report: %s\n", reportPath)
	if interrupted {
		fmt.Println(progress.C("Interrupted — committed progress is safe; re-run to resume "+
			"(done rows are skipped).", "yellow"))
		return 130
	}
	return 0
}

// printMailboxReport prints the per-folder breakdown for one mailbox.
func printMailboxReport(rep engine.DedupReport, execute bool) {
	if rep.Skipped {
		return
	}
	head := fmt.Sprintf("%s — %d folder(s) with duplicates, %d group(s), %d duplicate message(s), %s reclaimable",
		rep.Mailbox, len(rep.Folders), rep.TotalGroups, rep.TotalDups, util.FmtBytes(float64(rep.TotalBytes)))
	if len(rep.Folders) == 0 {
		fmt.Printf("  %s — no duplicates found (%d message(s) scanned)\n", rep.Mailbox, rep.TotalMsgs)
		return
	}
	fmt.Println("  " + head)
	for _, fr := range rep.Folders {
		fmt.Printf("    [%s] %d group(s), %d duplicate(s), %s\n",
			fr.Folder, len(fr.Groups), fr.DupCount, util.FmtBytes(float64(fr.DupBytes)))
		for _, g := range fr.Groups {
			var dupUIDs []string
			for _, d := range g.Dups {
				dupUIDs = append(dupUIDs, fmt.Sprintf("%d", d.UID))
			}
			fmt.Printf("      keep UID %d · quarantine UID(s) %s\n", g.KeeperUID,
				joinComma(dupUIDs))
		}
	}
	if execute {
		fmt.Printf("    quarantined %d · already done %d\n", rep.Quarantined, rep.AlreadyDone)
	}
}

func joinComma(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

// writeDedupCSV writes the dedup report: one row per duplicate message.
// Columns: mailbox,folder,keeper_uid,duplicate_uid,message_id,size,subject.
func writeDedupCSV(path string, reports []engine.DedupReport) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"mailbox", "folder", "keeper_uid", "duplicate_uid",
		"message_id", "size", "subject"})
	for _, rep := range reports {
		for _, fr := range rep.Folders {
			for _, g := range fr.Groups {
				for _, d := range g.Dups {
					w.Write([]string{rep.Mailbox, fr.Folder,
						fmt.Sprint(g.KeeperUID), fmt.Sprint(d.UID),
						d.MessageID, fmt.Sprint(d.Size), d.Subject})
				}
			}
		}
	}
	return nil
}
