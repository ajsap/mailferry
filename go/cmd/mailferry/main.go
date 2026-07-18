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

// mailferry: the command-line entry point.
package main

import (
	"context"
	"crypto/x509"
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/engine"
	"github.com/ajsap/mailferry/v2/internal/fakeimap"
	"github.com/ajsap/mailferry/v2/internal/identity"
	"github.com/ajsap/mailferry/v2/internal/imapx"
	"github.com/ajsap/mailferry/v2/internal/progress"
	"github.com/ajsap/mailferry/v2/internal/report"
	"github.com/ajsap/mailferry/v2/internal/state"
	"github.com/ajsap/mailferry/v2/internal/tui"
	"github.com/ajsap/mailferry/v2/internal/util"
)

//go:embed CHANGELOG.md
var embeddedChangelog string

func termInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

func main() { os.Exit(run(os.Args[1:])) }

var commands = map[string]bool{"run": true, "resume": true, "check": true,
	"validate": true, "doctor": true, "benchmark": true, "init": true,
	"import-state": true, "capabilities": true, "verify": true, "compact": true,
	"changelog": true, "roadmap": true, "status": true, "failed": true,
	"retry-failed": true, "config": true, "version": true, "about": true}

// bootCfg is the file-configured baseline every command starts from.
// mailferry.toml: sensible defaults that just work; the file only exists so
// advanced users can tune behaviour. CLI flags always win. A missing or
// broken file can never stop MailFerry from starting.
var (
	bootCfg     *config.Run
	bootPath    string
	bootCreated bool
)

func bootstrapConfig(argv []string) []string {
	// extract --config PATH from anywhere in argv (Python-argparse parity)
	explicit := ""
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--config" && i+1 < len(argv) {
			explicit = argv[i+1]
			argv = append(argv[:i], argv[i+2:]...)
			break
		}
		if strings.HasPrefix(argv[i], "--config=") {
			explicit = strings.TrimPrefix(argv[i], "--config=")
			argv = append(argv[:i], argv[i+1:]...)
			break
		}
	}
	bootCfg = config.Defaults()
	warns, path, created := config.LoadTOML(bootCfg, explicit, true)
	bootPath, bootCreated = path, created
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, progress.C("note: "+w, "yellow"))
	}
	if created {
		fmt.Fprintln(os.Stderr, progress.C(fmt.Sprintf(
			"note: wrote a documented default configuration to %s "+
				"(optional — MailFerry runs fine without it)", path), "cyan"))
	}
	return argv
}

func run(argv []string) int {
	if len(argv) > 0 && (argv[0] == "--about" || argv[0] == "about") {
		fmt.Print(identity.AboutText())
		return 0
	}
	// wrapper muscle memory: `mailferry --init FILE`
	if len(argv) > 1 && argv[0] == "--init" {
		argv = append([]string{"init"}, argv[1:]...)
	}
	if len(argv) > 0 && !commands[argv[0]] && !strings.HasPrefix(argv[0], "-") {
		argv = append([]string{"run"}, argv...) // `mailferry mailboxes.csv`
	}
	argv = bootstrapConfig(argv)
	if len(argv) == 0 {
		usage()
		return 0
	}
	switch argv[0] {
	case "--version", "-V", "version":
		fmt.Print(identity.VersionText())
		return 0
	case "--help", "-h", "help":
		usage()
		return 0
	}
	cmd := argv[0]
	rest := argv[1:]
	switch cmd {
	case "init":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: mailferry init FILE")
			return 2
		}
		if err := os.WriteFile(rest[0], []byte(config.CSVTemplate), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			return 1
		}
		fmt.Println("Sample CSV written to", rest[0])
		return 0
	case "config":
		return cmdConfig(rest)
	case "doctor":
		return cmdDoctor()
	case "benchmark":
		return cmdBenchmark(rest)
	case "capabilities":
		return cmdCapabilities(rest)
	case "verify":
		return cmdVerify(rest)
	case "compact":
		return cmdCompact(rest)
	case "import-state":
		return cmdImportState(rest)
	case "changelog":
		return cmdChangelog(rest)
	case "roadmap":
		fmt.Printf("%s\n%s\n\n", identity.BannerLine(), identity.Slogan)
		fmt.Printf("Project roadmap (aspirational — see %s):\n\n", identity.Repository)
		fmt.Println(strings.Join(identity.RoadmapLines(), "\n"))
		return 0
	case "status":
		return cmdStatus(rest)
	case "failed":
		return cmdFailed(rest)
	case "retry-failed":
		return cmdRetryFailed(rest)
	case "run", "resume", "check", "validate":
		return cmdRun(cmd, rest)
	}
	usage()
	return 2
}

func usage() {
	fmt.Print(identity.VersionText())
	fmt.Print(`
MailFerry migrates, synchronises and backs up IMAP mailboxes natively —
no imapsync, no external tools, no runtime dependencies.

Usage:
  mailferry run CSV [flags]      migrate / sync mailboxes (default command)
  mailferry resume CSV           alias of run — resuming is just running again
  mailferry check CSV            preflight: connect, auth, list, estimate — no writes
  mailferry validate CSV         alias of check
  mailferry status [--db PATH]   inspect a State Database (read-only, safe anytime)
  mailferry failed               list / export the Failed Message Registry
  mailferry retry-failed         re-queue registry entries for the next run
  mailferry verify CSV           compare per-folder counts: source vs destination vs state
  mailferry capabilities HOST PORT   probe a server's capabilities
  mailferry doctor               local environment self-test (no servers contacted)
  mailferry benchmark            loopback throughput benchmark (in-process servers)
  mailferry compact [--db PATH]  prune per-message rows for completed folders
  mailferry import-state FILE    import the old wrapper's migration.state
  mailferry init FILE            write a CSV template
  mailferry config               show / create the mailferry.toml configuration
  mailferry changelog | roadmap  release history / project roadmap
  mailferry version | about

Frequent flags: --workers N --db PATH --logs-dir DIR --timeout S
  --retries N --retry-delay S --per-host-conns N --skip-completed
  --order csv|size --include GLOB --exclude GLOB --map FILE --sync-flags
  --compress auto|off --baseline --tls-no-verify --no-tui --trace --config PATH
Run 'mailferry run -h' for the full list.

Author:     ` + identity.Author + ` <` + identity.AuthorEmail + `>
Repository: ` + identity.Repository + `
Support:    ` + identity.SupportURL + `
License:    ` + identity.LicenseShort + ` — run 'mailferry about' for details.
`)
}

// multiFlag collects repeatable --include / --exclude values.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// obsolete wrapper flags are rejected loudly with a pointer to the native way.
var obsoleteFlags = map[string]string{
	"imapsync-path":        "MailFerry speaks IMAP natively — no imapsync needed",
	"extra-args":           "MailFerry has first-class options for everything — see --help",
	"split-size":           "MailFerry's adaptive batching replaced --split-size",
	"skip-duplicate-check": "the State Database makes duplicate checks O(1); see --no-dedup-scan for empty destinations",
	"darwinfix":            "no Perl process to patch",
	"state-file":           "state moved to the State Database: use --db (see import-state)",
	"no-state":             "renamed: use --ephemeral",
}

func cmdRun(cmd string, rest []string) int {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	var include, exclude multiFlag
	var (
		workers      = fs.Int("workers", 0, "Concurrent mailboxes (default 10)")
		logsDir      = fs.String("logs-dir", "", "Directory for logs (default ./logs)")
		dbPath       = fs.String("db", "", "State database path (default ./migration.db)")
		ephemeral    = fs.Bool("ephemeral", false, "Keep no persistent state")
		force        = fs.Bool("force", false, "Re-verify everything (full replan + rescan; never blind re-copy)")
		skipDone     = fs.Bool("skip-completed", false, "Skip mailboxes recorded SUCCESS")
		retries      = fs.Int("retries", -1, "Mailbox retry attempts (auth failures are never auto-retried)")
		noRetry      = fs.Bool("no-retry", false, "Same as --retries 0")
		retryDelay   = fs.Float64("retry-delay", 0, "Base delay between retries (s); doubles each time")
		order        = fs.String("order", "", "Mailbox admission order: csv | size (largest known first)")
		maxConns     = fs.Int("max-conns-per-mailbox", 0, "Parallel folder connection pairs per mailbox")
		perHost      = fs.Int("per-host-conns", 0, "Connection cap per server host")
		timeout      = fs.Float64("timeout", 0, "Inactivity watchdog per connection (s)")
		lockTimeout  = fs.Float64("lock-timeout", 0, "A lease with no heartbeat for this long is stale (s)")
		resetStale   = fs.Bool("reset-stale-locks", false, "Compatibility no-op: dead workers are reclaimed automatically (see --worker-timeout)")
		compress     = fs.String("compress", "", "COMPRESS=DEFLATE when the server offers it: auto | off")
		baseline     = fs.Bool("baseline", false, "RFC-3501-only conservative mode (no LITERAL+/COMPRESS)")
		tlsNoVerify  = fs.Bool("tls-no-verify", false, "Disable TLS certificate verification")
		mapFile      = fs.String("map", "", "Folder mapping file: lines of 'Source Name = Dest Name'")
		gmailAll     = fs.Bool("gmail-all-mail", false, "Include Gmail [Gmail]/All Mail and Important")
		subscribe    = fs.Bool("subscribe", false, "SUBSCRIBE created folders")
		syncFlags    = fs.Bool("sync-flags", false, "Re-apply changed flags to already-synced messages (backup mode)")
		rescanDest   = fs.Bool("rescan-dest", false, "Force a fresh destination fingerprint scan")
		noDedupScan  = fs.Bool("no-dedup-scan", false, "Skip destination adoption scanning (guaranteed-empty destinations only)")
		jsonLogs     = fs.Bool("json-logs", false, "Also write an NDJSON event log")
		jsonProgress = fs.Bool("json-progress", false, "Write NDJSON progress snapshots (1 Hz)")
		noTUI        = fs.Bool("no-tui", false, "Plain status lines instead of the dashboard")
		traceF       = fs.Bool("trace", false, "Protocol-level trace in per-mailbox logs (credentials redacted)")
		debugF       = fs.Bool("debug", false, "Verbose errors")
		checkF       = fs.Bool("check", false, "Preflight only: connect, authenticate, list, estimate — write nothing")
		dryRun       = fs.Bool("dry-run", false, "Alias of --check")
		cfgPath      = fs.String("config", "", "Path to mailferry.toml (already applied at startup)")
		workerTO     = fs.Float64("worker-timeout", 0, "Cluster worker offline threshold (s)")
		msgRetries   = fs.Int("message-attempts", 0, "Transfer passes per message")
		reconnectAtt = fs.Int("reconnect-attempts", 0, "Folder reconnect attempts")
		staleTO      = fs.Float64("stale-timeout", -1, "Auto-recover a mailbox with no progress for S seconds (0 disables)")
		recRetries   = fs.Int("recovery-retries", 0, "Connection-recovery attempts per stall before Recovery Mode")
		recInterval  = fs.Float64("recovery-interval", 0, "Wait between recovery attempts (s)")
		batchAtt     = fs.Int("batch-attempts", 0, "Attempts per level during failure isolation")
		noIsolate    = fs.Bool("no-isolate", false, "Disable progressive failed-message isolation")
		noSkipFailed = fs.Bool("no-skip-failed", false, "Do not skip Failed Message Registry entries on resume")
	)
	fs.Var(&include, "include", "Only sync folders matching GLOB (repeatable)")
	fs.Var(&exclude, "exclude", "Skip folders matching GLOB (repeatable)")
	obsoleteSet := map[string]*string{}
	for name := range obsoleteFlags {
		obsoleteSet[name] = fs.String(name, "", "(obsolete)")
	}
	fs.Parse(reorderArgs(fs, rest))
	_ = cfgPath // consumed by bootstrapConfig before dispatch
	for name, val := range obsoleteSet {
		if *val != "" || flagWasSet(fs, name) {
			fmt.Fprintf(os.Stderr, "--%s is obsolete in MailFerry: %s\n", name, obsoleteFlags[name])
			return 2
		}
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: mailferry "+cmd+" CSV [flags]")
		return 2
	}
	cfg := bootCfg
	// CLI overrides file
	cfg.CSVFile = fs.Arg(0)
	if *workers > 0 {
		cfg.Workers = *workers
	}
	if *logsDir != "" {
		cfg.LogsDir = *logsDir
	}
	if *dbPath != "" {
		cfg.DBPath = *dbPath
	}
	cfg.Ephemeral = *ephemeral
	cfg.Force = *force
	cfg.SkipCompleted = *skipDone
	if *noRetry {
		cfg.Retries = 0
	} else if *retries >= 0 {
		cfg.Retries = *retries
	}
	if *retryDelay > 0 {
		cfg.RetryDelay = *retryDelay
	}
	if *order != "" {
		if *order != "csv" && *order != "size" {
			fmt.Fprintln(os.Stderr, "ERROR: --order must be csv or size")
			return 2
		}
		cfg.Order = *order
	}
	if *maxConns > 0 {
		cfg.MaxConnsPerBox = *maxConns
	}
	if *perHost > 0 {
		cfg.PerHostConns = *perHost
	}
	if *timeout > 0 {
		cfg.Timeout = *timeout
		if cfg.Timeout < 20 {
			cfg.Timeout = 20
		}
	}
	if *lockTimeout > 0 {
		cfg.LockTimeout = *lockTimeout
		if cfg.LockTimeout < 30 {
			cfg.LockTimeout = 30
		}
	}
	cfg.ResetStaleLocks = *resetStale
	if *compress != "" {
		if *compress != "auto" && *compress != "off" {
			fmt.Fprintln(os.Stderr, "ERROR: --compress must be auto or off")
			return 2
		}
		cfg.Compress = *compress
	}
	cfg.Baseline = *baseline
	if *tlsNoVerify {
		cfg.TLSVerify = false
	}
	if len(include) > 0 {
		cfg.Include = include
	}
	if len(exclude) > 0 {
		cfg.Exclude = exclude
	}
	if *mapFile != "" {
		cfg.MapFile = *mapFile
	}
	cfg.NoTUI = *noTUI
	cfg.GmailAllMail = *gmailAll
	cfg.Subscribe = *subscribe
	cfg.SyncFlags = *syncFlags
	cfg.RescanDest = *rescanDest || *force
	cfg.NoDedupScan = *noDedupScan && !*force
	cfg.JSONLogs = *jsonLogs
	cfg.JSONProgress = *jsonProgress
	cfg.Trace = cfg.Trace || *traceF
	cfg.Debug = cfg.Debug || *debugF
	if *workerTO > 0 {
		cfg.WorkerTimeout = *workerTO
	}
	if *msgRetries > 0 {
		cfg.MsgRetries = *msgRetries
	}
	if *reconnectAtt > 0 {
		cfg.ReconnectAttempts = *reconnectAtt
	}
	if *staleTO >= 0 {
		cfg.StaleTimeout = *staleTO
	}
	if *recRetries > 0 {
		cfg.RecoveryRetries = *recRetries
	}
	if *recInterval > 0 {
		cfg.RecoveryInterval = *recInterval
	}
	if *batchAtt > 0 {
		cfg.BatchAttempts = *batchAtt
	}
	if *noIsolate {
		cfg.IsolateFailed = false
	}
	if *noSkipFailed {
		cfg.SkipKnownFailed = false
	}
	cfg.CheckOnly = cmd == "check" || cmd == "validate" || *checkF || *dryRun

	specs, err := config.ParseCSV(cfg.CSVFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	if cfg.CheckOnly {
		return runCheck(cfg, specs)
	}

	if err := os.MkdirAll(cfg.LogsDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	pruneOldLogs(cfg.LogsDir, cfg.LogKeepDays)
	jsonEvents := ""
	if cfg.JSONLogs {
		jsonEvents = filepath.Join(cfg.LogsDir, "events-"+cfg.RunID+".ndjson")
	}
	session := report.NewSession(filepath.Join(cfg.LogsDir, "session.log"), jsonEvents)
	if bootCreated {
		session.Log("configuration: wrote a documented default mailferry.toml to " + bootPath)
	}
	var stopProgress func()
	if cfg.JSONProgress {
		stopProgress = func() {}
	}
	stats := engine.NewStats()
	stats.CSVFile = filepath.Base(cfg.CSVFile)
	stats.DBPath = cfg.DBPath
	if cfg.Ephemeral {
		stats.DBPath = "(ephemeral)"
	}
	stats.LogsDir = cfg.LogsDir
	if cfg.JSONProgress {
		stopProgress = report.StartProgressNDJSON(
			filepath.Join(cfg.LogsDir, "progress-"+cfg.RunID+".ndjson"), stats)
		defer stopProgress()
	}
	if cfg.Workers > 20 {
		fmt.Println("Note: >20 concurrent mailboxes — some servers throttle or cap simultaneous " +
			"IMAP logins per account/IP. Lower --workers if you see auth/connection errors.")
	}
	fmt.Println(identity.BannerLine())
	fmt.Println(identity.Slogan)
	fmt.Println()
	fmt.Printf("Starting migration: %d mailbox(es), %d worker(s), State Database %s\n",
		len(specs), cfg.Workers, stats.DBPath)
	session.Log(fmt.Sprintf("=== %s — run %s start: csv=%s rows=%d workers=%d db=%s%s%s",
		identity.BannerLine(), cfg.RunID, stats.CSVFile, len(specs), cfg.Workers, stats.DBPath,
		map[bool]string{true: " force"}[cfg.Force], map[bool]string{true: " ephemeral"}[cfg.Ephemeral]))

	// Automatic terminal detection: interactive TTY -> Bubble Tea TUI;
	// non-interactive or --no-tui -> headless. Both drive the SAME engine.
	interactive := progress.IsTTY && termInteractive() && !cfg.NoTUI
	ctx, cancel := context.WithCancel(context.Background())
	bus := engine.NewBus()
	start := time.Now()
	interrupted := false

	var res engine.RunResult
	var runErr error
	if interactive {
		res, runErr, interrupted = runInteractive(ctx, cancel, cfg, specs, stats, bus, session)
	} else {
		res, runErr, interrupted = runHeadless(ctx, cancel, cfg, specs, stats, bus, session)
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", runErr)
		return 1
	}

	snap := stats.Snapshot()
	resultsPath := report.WriteResultsCSV(snap, cfg.LogsDir)
	runtimeS := time.Since(start).Seconds()
	session.Log(fmt.Sprintf("=== %s run %s end: runtime=%s ok=%d warnings=%d partial=%d failed=%d stale=%d cancelled=%d%s",
		identity.BannerLine(), cfg.RunID, util.FmtDHMS(runtimeS),
		res.Counts["SUCCESS"], res.Counts["WARNINGS"], res.Counts["PARTIAL"],
		res.Counts["FAILED"], res.Counts["STALE"], res.Counts["CANCELLED"],
		map[bool]string{true: " (interrupted)"}[interrupted]))
	report.PrintSummary(snap, resultsPath, cfg, runtimeS, interrupted, progress.C)
	report.PrintFailedSection(res.FailedRegistry, res.Outstanding, cfg.LogsDir, progress.C)
	if bootCreated {
		fmt.Println(progress.C("note: a documented default configuration was written to "+
			bootPath+" on this first run.", "cyan"))
	}
	if interrupted {
		fmt.Println(progress.C("Interrupted — per-UID state is committed; re-run the same "+
			"command to resume exactly where this stopped.", "yellow"))
		return 130
	}
	if res.Counts["FAILED"] > 0 || res.Counts["PARTIAL"] > 0 || res.Counts["STALE"] > 0 {
		return 1
	}
	return 0 // WARNINGS completes the run: failures are recorded, not fatal
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

const graceSeconds = 6 // bounded escalation after a graceful stop request

// escalate closes every connection if the engine hasn't unwound within the
// grace window, so shutdown never hangs on a stalled socket.
func escalate(bus *engine.Bus, session *report.Session, done <-chan struct{}) {
	time.AfterFunc(graceSeconds*time.Second, func() {
		select {
		case <-done:
			return
		default:
			if n := bus.AbortAllConnections(); n > 0 {
				session.Log(fmt.Sprintf("shutdown escalation: %d connection(s) force-closed "+
					"after %ds grace (state stays consistent)", n, graceSeconds))
			}
		}
	})
}

// runInteractive drives the Bubble Tea TUI over the engine. The engine runs
// in its own goroutine; the TUI is a pure consumer, so a render fault or
// lost terminal can never corrupt migration state.
func runInteractive(ctx context.Context, cancel context.CancelFunc,
	cfg *config.Run, specs []config.MailboxSpec, stats *engine.Stats, bus *engine.Bus,
	session *report.Session) (engine.RunResult, error, bool) {
	session.Log(fmt.Sprintf("=== %s — run %s start (TUI): rows=%d workers=%d db=%s",
		identity.BannerLine(), cfg.RunID, len(specs), cfg.Workers, cfg.DBPath))
	var res engine.RunResult
	var runErr error
	done := make(chan struct{})
	go func() {
		res, runErr = engine.RunMigrationBus(ctx, cfg, specs, stats, bus, session.Log,
			report.MailboxLoggerFactory(cfg.LogsDir))
		close(done)
	}()
	gracefulStop := func() {
		session.Log("interrupt received (Ctrl+C) — graceful stop: no new work, active " +
			"workers finish the current message, then connections close")
		cancel()
		escalate(bus, session, done)
	}
	hardStop := func() {
		session.Log("second interrupt — immediate abort (state stays consistent)")
		bus.AbortAllConnections()
		cancel()
		go func() { time.Sleep(400 * time.Millisecond); os.Exit(130) }()
	}
	model := tui.New(stats, bus, gracefulStop, hardStop,
		time.Duration(cfg.RefreshMS)*time.Millisecond, done)
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		// TUI failed: never abort the migration — fall back to waiting headless
		fmt.Fprintln(os.Stderr, "note: TUI unavailable (", err, ") — continuing headless")
		<-done
	} else {
		<-done
	}
	return res, runErr, ctx.Err() != nil
}

// runHeadless runs the same engine with clean structured console output.
func runHeadless(ctx context.Context, cancel context.CancelFunc, cfg *config.Run,
	specs []config.MailboxSpec, stats *engine.Stats, bus *engine.Bus,
	session *report.Session) (engine.RunResult, error, bool) {
	progress.IsTTY = false
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	interrupted := false
	done := make(chan struct{})
	go func() {
		<-sigCh
		interrupted = true
		stats.Interrupted = true
		session.Log("interrupt received (Ctrl+C / SIGTERM) — graceful stop: no new work, " +
			"active workers finish the current message, then connections close")
		fmt.Fprintln(os.Stderr, "\nStopping gracefully — press Ctrl+C again to force quit…")
		cancel()
		escalate(bus, session, done)
		<-sigCh
		session.Log("second interrupt — immediate abort (state stays consistent)")
		bus.AbortAllConnections()
		time.Sleep(300 * time.Millisecond)
		os.Exit(130)
	}()
	renderer := progress.NewRenderer(stats, session.Log,
		time.Duration(cfg.RefreshMS)*time.Millisecond)
	renderer.Start()
	res, runErr := engine.RunMigrationBus(ctx, cfg, specs, stats, bus, session.Log,
		report.MailboxLoggerFactory(cfg.LogsDir))
	close(done)
	renderer.Stop(true)
	signal.Stop(sigCh)
	return res, runErr, interrupted
}

// ---------------------------------------------------------------- check --

// runCheck: preflight — connect, authenticate, list, estimate. No writes.
func runCheck(cfg *config.Run, specs []config.MailboxSpec) int {
	fmt.Printf("%s\n%s\n\nMailFerry preflight check — %d mailbox(es); nothing will be written.\n\n",
		identity.BannerLine(), identity.Slogan, len(specs))
	type result struct {
		spec config.MailboxSpec
		res  engine.PreflightResult
	}
	sem := make(chan struct{}, 4)
	out := make(chan result, len(specs))
	for _, sp := range specs {
		sp := sp
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			out <- result{sp, engine.Preflight(cfg, sp)}
		}()
	}
	results := make([]result, 0, len(specs))
	for range specs {
		results = append(results, <-out)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].spec.Index < results[j].spec.Index })
	okn := 0
	for _, r := range results {
		if r.res.Err != nil {
			fmt.Printf("  FAIL %-38s -> %-38s %v\n", r.spec.Src.Label(), r.spec.Dst.Label(), r.res.Err)
			continue
		}
		okn++
		size := "?"
		if r.res.Bytes > 0 {
			size = util.FmtBytes(float64(r.res.Bytes))
		}
		fmt.Printf("  OK   %-38s -> %-38s folders=%-4d msgs=%-8d size=%-10s %s %s\n",
			r.spec.Src.Label(), r.spec.Dst.Label(), r.res.Folders, r.res.Msgs, size,
			r.res.SrcCaps, r.res.DstCaps)
	}
	fmt.Printf("\nCheck complete: %d/%d mailbox(es) ready.\n", okn, len(specs))
	if okn == len(specs) {
		return 0
	}
	return 1
}

// --------------------------------------------------------------- doctor --

func cmdDoctor() int {
	fmt.Printf("%s\n%s\n\nEnvironment self-test:\n", identity.BannerLine(), identity.Slogan)
	ok := true
	line := func(label string, good bool, detail string, advisory bool) {
		if !advisory {
			ok = ok && good
		}
		mark := "✓"
		if !good {
			mark = map[bool]string{true: "•", false: "✗"}[advisory]
		}
		fmt.Printf("  %s %-24s %s\n", mark, label, detail)
	}
	line("Runtime", true, runtime.Version()+" (static binary — no interpreter needed)", false)
	line("Platform", true, runtime.GOOS+"/"+runtime.GOARCH, false)
	tty := termInteractive()
	line("TTY / interactive", tty,
		map[bool]string{true: "yes", false: "no — TUI disabled here (advisory)"}[tty], true)
	lang := strings.ToLower(os.Getenv("LC_ALL") + os.Getenv("LC_CTYPE") + os.Getenv("LANG"))
	utf := strings.Contains(lang, "utf")
	line("UTF-8 locale", utf || runtime.GOOS == "windows",
		map[bool]string{true: "yes", false: os.Getenv("LANG") + " (set LANG=…UTF-8 for best rendering)"}[utf], true)
	tmp, err := os.MkdirTemp("", "mfdoctor")
	if err == nil {
		defer os.RemoveAll(tmp)
		db, derr := state.Open(filepath.Join(tmp, "t.db"), false, 300)
		if derr == nil {
			db.Close()
			line("State Database", true, "SQLite writable (pure Go — no CGO)", false)
		} else {
			line("State Database", false, "SQLite error: "+derr.Error(), false)
		}
	} else {
		line("State Database", false, "temp dir: "+err.Error(), false)
	}
	if pool, cerr := loadSystemRoots(); cerr == nil && pool > 0 {
		line("TLS CA store", true, fmt.Sprintf("%d trusted roots", pool), false)
	} else if cerr != nil {
		line("TLS CA store", false, cerr.Error(), false)
	} else {
		line("TLS CA store", true, "system verifier (platform API)", false)
	}
	cfgNote := bootPath
	if bootCreated {
		cfgNote += "   (created just now)"
	}
	line("Configuration", true, cfgNote, true)
	if ok {
		fmt.Println("\nAll good — ready to migrate.")
		return 0
	}
	fmt.Println("\nSome checks failed — see above.")
	return 1
}

// --------------------------------------------------------- capabilities --

func cmdCapabilities(rest []string) int {
	fs := flag.NewFlagSet("capabilities", flag.ExitOnError)
	security := fs.String("security", "ssl", "ssl | tls | none")
	user := fs.String("user", "", "Optional login for post-auth capabilities")
	password := fs.String("password", "", "Password for --user")
	tlsNoVerify := fs.Bool("tls-no-verify", false, "Disable TLS certificate verification")
	fs.Parse(reorderArgs(fs, rest))
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: mailferry capabilities HOST PORT [--security ssl|tls|none] [--user U --password P]")
		return 2
	}
	port := 0
	fmt.Sscanf(fs.Arg(1), "%d", &port)
	ep := imapx.Endpoint{Host: fs.Arg(0), Port: port, Security: *security,
		User: *user, Password: *password}
	cli := imapx.NewClient(ep, 30*time.Second, !*tlsNoVerify, nil, ep.Host, nil)
	if err := cli.Connect(); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	defer cli.Logout(5 * time.Second)
	fmt.Printf("Greeting : %s\n", cli.Greeting)
	caps := func() []string {
		var out []string
		for c := range cli.Caps {
			out = append(out, c)
		}
		sort.Strings(out)
		return out
	}
	fmt.Printf("Pre-auth : %s\n", strings.Join(caps(), " "))
	if *user != "" {
		if err := cli.Login(); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			return 1
		}
		fmt.Printf("Post-auth: %s\n", strings.Join(caps(), " "))
	}
	fmt.Println("Optimisation plan:")
	for _, row := range [][2]string{
		{"UIDPLUS", "exact UID mapping (APPENDUID)"},
		{"LITERAL+", "non-blocking uploads"},
		{"COMPRESS=DEFLATE", "wire compression"},
		{"CONDSTORE", "MODSEQ delta sync"},
		{"QRESYNC", "quick resync"},
		{"MULTIAPPEND", "batched appends (reserved)"},
		{"SPECIAL-USE", "role-based folder mapping"},
		{"STATUS=SIZE", "byte-accurate ETAs"},
		{"APPENDLIMIT", "oversize preflight"},
		{"NAMESPACE", "prefix detection"},
		{"ID", "client identification"},
	} {
		has := cli.Has(row[0])
		if !has {
			for c := range cli.Caps {
				if strings.HasPrefix(c, row[0]+"=") {
					has = true
					break
				}
			}
		}
		mark := "·"
		if has {
			mark = "✓"
		}
		fmt.Printf("  %s %-18s %s\n", mark, row[0], row[1])
	}
	return 0
}

// --------------------------------------------------------------- verify --

func cmdVerify(rest []string) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dbPath := fs.String("db", "./migration.db", "State database path")
	tlsNoVerify := fs.Bool("tls-no-verify", false, "Disable TLS certificate verification")
	fs.Parse(reorderArgs(fs, rest))
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: mailferry verify CSV [--db PATH]")
		return 2
	}
	specs, err := config.ParseCSV(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	db, err := state.OpenForTest(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	defer db.Close()
	bad := 0
	for _, spec := range specs {
		mid := db.MailboxIDByKey(spec.Key())
		if mid == 0 {
			fmt.Printf("%s: no state recorded yet\n", spec.Src.Label())
			continue
		}
		folders := db.FoldersOf(mid)
		if len(folders) == 0 {
			fmt.Printf("%s: no folder state recorded yet\n", spec.Src.Label())
			continue
		}
		to := 60 * time.Second
		src := imapx.NewClient(imapx.Endpoint(spec.Src), to, !*tlsNoVerify, nil, spec.Label(), nil)
		dst := imapx.NewClient(imapx.Endpoint(spec.Dst), to, !*tlsNoVerify, nil, spec.Label(), nil)
		open := func(c *imapx.Client) error {
			if err := c.Connect(); err != nil {
				return err
			}
			return c.Login()
		}
		if err := open(src); err != nil {
			fmt.Printf("%s: verify failed (%v)\n", spec.Src.Label(), err)
			bad++
			continue
		}
		if err := open(dst); err != nil {
			fmt.Printf("%s: verify failed (%v)\n", spec.Src.Label(), err)
			src.Logout(3 * time.Second)
			bad++
			continue
		}
		for _, fo := range folders {
			s, serr := src.Status(imapx.EncodeMUTF7(fo.SrcName))
			d, derr := dst.Status(imapx.EncodeMUTF7(fo.DstName))
			if serr != nil || derr != nil {
				fmt.Printf("  %s [%s]: STATUS failed\n", spec.Src.Label(), fo.SrcName)
				bad++
				continue
			}
			okc := "OK      "
			if d["MESSAGES"] < fo.MsgsDone {
				okc = "MISMATCH"
				bad++
			}
			fmt.Printf("  %s %s [%s]: src=%d dst=%d synced=%d\n",
				okc, spec.Src.Label(), fo.SrcName, s["MESSAGES"], d["MESSAGES"], fo.MsgsDone)
		}
		src.Logout(3 * time.Second)
		dst.Logout(3 * time.Second)
	}
	if bad > 0 {
		return 1
	}
	return 0
}

// ------------------------------------------------- compact/import-state --

func cmdCompact(rest []string) int {
	fs := flag.NewFlagSet("compact", flag.ExitOnError)
	dbPath := fs.String("db", "./migration.db", "State database path")
	fs.Parse(reorderArgs(fs, rest))
	db, err := state.OpenForTest(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	defer db.Close()
	n := db.Compact()
	fmt.Printf("Removed %d per-message rows for completed folders (aggregates kept).\n", n)
	return 0
}

func cmdImportState(rest []string) int {
	fs := flag.NewFlagSet("import-state", flag.ExitOnError)
	dbPath := fs.String("db", "./migration.db", "State database path")
	fs.Parse(reorderArgs(fs, rest))
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: mailferry import-state STATEFILE [--db PATH]")
		return 2
	}
	db, err := state.Open(*dbPath, false, 300)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	defer db.Close()
	n, err := db.ImportWrapperState(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	fmt.Printf("Imported %d completed mailbox record(s) from %s into %s.\n", n, fs.Arg(0), *dbPath)
	fmt.Println("Those mailboxes will get a cheap incremental pass (or be skipped with --skip-completed).")
	return 0
}

// ------------------------------------------------------------ changelog --

func cmdChangelog(rest []string) int {
	fs := flag.NewFlagSet("changelog", flag.ExitOnError)
	full := fs.Bool("full", false, "Show the entire changelog")
	fs.Parse(reorderArgs(fs, rest))
	if *full {
		fmt.Println(strings.TrimRight(embeddedChangelog, "\n"))
		return 0
	}
	version := identity.Version
	if fs.NArg() > 0 {
		version = strings.TrimPrefix(fs.Arg(0), "v")
	}
	fmt.Printf("%s\n\nChangelog for v%s (full history: %s/blob/main/CHANGELOG.md):\n\n",
		identity.BannerLine(), version, identity.Repository)
	section := func(name string) (string, bool) {
		re := regexp.MustCompile(`(?s)## \[` + regexp.QuoteMeta(name) + `\].*?\n(.*?)(\n## \[|\n\[|\z)`)
		if m := re.FindStringSubmatch(embeddedChangelog); m != nil {
			return strings.TrimSpace(m[1]), true
		}
		return "", false
	}
	if text, ok := section(version); ok {
		fmt.Println(text)
	} else if text, ok := section("Unreleased"); ok && version == identity.Version {
		// The running version has not been released yet (release candidate):
		// show the in-progress notes rather than nothing.
		fmt.Println("(v" + version + " is not released yet — showing the [Unreleased] notes)\n")
		fmt.Println(text)
	} else {
		fmt.Printf("No changelog section for v%s in this build — see the repository.\n", version)
	}
	return 0
}

// ------------------------------------------------------------ benchmark --

// cmdBenchmark: loopback throughput benchmark against in-process fake IMAP
// servers. An upper bound for the engine + protocol stack on this machine —
// real migrations are network-bound.
func cmdBenchmark(rest []string) int {
	fs := flag.NewFlagSet("benchmark", flag.ExitOnError)
	nMsgs := fs.Int("messages", 2000, "Messages in the synthetic mailbox")
	msgKB := fs.Int("size-kb", 32, "Approximate message size (KB)")
	workers := fs.Int("workers", 4, "Concurrent folder pipelines")
	fs.Parse(reorderArgs(fs, rest))
	fmt.Printf("%s\n%s\n\nLoopback benchmark: %d messages × ~%d KB, in-process servers.\n"+
		"This measures the engine + protocol stack — real migrations are network-bound.\n\n",
		identity.BannerLine(), identity.Slogan, *nMsgs, *msgKB)
	srcA := fakeimap.NewAccount("bench", "pw")
	inbox := srcA.Folder("INBOX")
	phrase := "The quick brown ferry crosses the harbour. "
	pad := strings.Repeat(phrase, (*msgKB*1024)/len(phrase)+2)
	for i := 1; i <= *nMsgs; i++ {
		body := fmt.Sprintf("Message-ID: <bench%d@example.test>\r\nFrom: bench@example.test\r\n"+
			"To: bench@example.org\r\nSubject: benchmark %d\r\nDate: Fri, 17 Jul 2026 10:00:00 +0000\r\n"+
			"\r\n%s\r\n", i, i, pad[:*msgKB*1024])
		inbox.Add([]byte(body), []string{`\Seen`}, "17-Jul-2026 10:00:00 +0000")
	}
	dstA := fakeimap.NewAccount("bench2", "pw")
	src, dst := fakeimap.NewServer(srcA), fakeimap.NewServer(dstA)
	if err := src.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	if err := dst.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	defer src.Stop()
	defer dst.Stop()
	tmp, err := os.MkdirTemp("", "mfbench")
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	defer os.RemoveAll(tmp)
	cfg := config.Defaults()
	cfg.CSVFile = "benchmark"
	cfg.DBPath = filepath.Join(tmp, "bench.db")
	cfg.LogsDir = tmp
	cfg.Workers = 1
	cfg.MaxConnsPerBox = *workers
	cfg.StaleTimeout = 0
	cfg.RunID = time.Now().Format("20060102-150405")
	specs := []config.MailboxSpec{{Index: 1,
		Src: config.Endpoint{Host: "127.0.0.1", Port: src.Port(), Security: "none",
			User: "bench", Password: "pw"},
		Dst: config.Endpoint{Host: "127.0.0.1", Port: dst.Port(), Security: "none",
			User: "bench2", Password: "pw"}}}
	stats := engine.NewStats()
	t0 := time.Now()
	res, err := engine.RunMigration(context.Background(), cfg, specs, stats,
		func(string) {}, func(config.MailboxSpec) func(string, ...any) {
			return func(string, ...any) {}
		})
	el := time.Since(t0).Seconds()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	agg := stats.Snapshot().Agg()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("  Messages     %d migrated in %.2fs\n", agg.MsgsDone, el)
	fmt.Printf("  Throughput   %.0f msgs/s · %s/s payload\n",
		float64(agg.MsgsDone)/el, util.FmtBytes(float64(agg.BytesDone)/el))
	fmt.Printf("  Wire         ↓%s ↑%s\n",
		util.FmtBytes(float64(agg.WireRX)), util.FmtBytes(float64(agg.WireTX)))
	fmt.Printf("  Memory       %s heap in use · %d goroutine peak est.\n",
		util.FmtBytes(float64(ms.HeapInuse)), runtime.NumGoroutine())
	fmt.Printf("  Result       %v\n", res.Counts)
	fmt.Println("\nPublish comparisons only from identical hardware and identical corpora.")
	return 0
}

// ---------------------------------------------------------------- config --

func cmdConfig(rest []string) int {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	pathOnly := fs.Bool("path", false, "Print only the active config path")
	fs.Parse(rest)
	if *pathOnly {
		fmt.Println(bootPath)
		return 0
	}
	fmt.Printf("%s\n%s\n\n", identity.BannerLine(), identity.Slogan)
	note := ""
	if bootCreated {
		note = "   (created just now)"
	} else if _, err := os.Stat(bootPath); err != nil {
		note = "   (missing — using built-in defaults)"
	}
	fmt.Printf("Configuration file : %s%s\n", bootPath, note)
	fmt.Printf("Default location   : %s\n", config.DefaultTOMLPath())
	fmt.Println("Search order       : --config PATH > ./mailferry.toml > default location")
	fmt.Println("\nEvery option is documented inside the file itself. CLI flags")
	fmt.Println("always override it; deleting it is always safe. New options in")
	fmt.Println("future versions are appended as commented defaults — your own")
	fmt.Println("settings are never rewritten.")
	return 0
}

// reorderArgs lets operators write flags anywhere (Python-argparse style):
// `mailferry run file.csv --workers 6` works exactly like
// `mailferry run --workers 6 file.csv`.
func reorderArgs(fs *flag.FlagSet, args []string) []string {
	boolFlags := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		if bv, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bv.IsBoolFlag() {
			boolFlags[f.Name] = true
		}
	})
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" && a != "--" {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if eq := strings.Index(name, "="); eq >= 0 {
				continue
			}
			if !boolFlags[name] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func pruneOldLogs(dir string, keepDays int) {
	if keepDays <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -keepDays)
	for _, pat := range []string{"*.log", "*.ndjson"} {
		files, _ := filepath.Glob(filepath.Join(dir, pat))
		for _, f := range files {
			if st, err := os.Stat(f); err == nil && st.ModTime().Before(cutoff) {
				os.Remove(f)
			}
		}
	}
}

// --------------------------------------------------------------- status --

func cmdStatus(rest []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dbPath := fs.String("db", "./migration.db", "State database path")
	workerTO := fs.Float64("worker-timeout", 60, "Offline threshold (s)")
	fs.Parse(reorderArgs(fs, rest))
	if _, err := os.Stat(*dbPath); err != nil {
		fmt.Fprintln(os.Stderr, "no State Database at", *dbPath)
		return 1
	}
	db, err := state.OpenForTest(*dbPath) // read-only snapshot; never competes with workers
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	defer db.Close()
	rep := db.Status(*workerTO)
	fmt.Println(identity.BannerLine())
	fmt.Println(identity.Slogan)
	fmt.Println()
	kv := func(k, v string) { fmt.Printf("%-22s: %s\n", k, v) }
	kv("Last run", rep.LastRunID)
	if rep.LastRunStart > 0 {
		started := time.Unix(int64(rep.LastRunStart), 0)
		st := "running"
		if rep.LastRunEnd > 0 {
			st = "finished — " + rep.LastResult
		}
		kv("Run state", st)
		kv("Started", started.Format("2006-01-02 15:04:05"))
	}
	kv("Running mailboxes", fmt.Sprintf("%d", rep.Counts["RUNNING"]))
	if len(rep.RunningLabels) > 0 {
		kv("  active", strings.Join(rep.RunningLabels, ", "))
	}
	kv("Queued", fmt.Sprintf("%d", rep.Counts["NEW"]+rep.Counts["QUEUED"]))
	kv("Completed", fmt.Sprintf("%d", rep.Counts["SUCCESS"]))
	kv("Completed with warnings", fmt.Sprintf("%d", rep.Counts["WARNINGS"]))
	kv("Partial", fmt.Sprintf("%d", rep.Counts["PARTIAL"]))
	kv("Failed", fmt.Sprintf("%d", rep.Counts["FAILED"]))
	if rep.Counts["STALE"] > 0 {
		kv("Stale", fmt.Sprintf("%d", rep.Counts["STALE"]))
	}
	if rep.Counts["REMOTE"] > 0 {
		kv("On other workers", fmt.Sprintf("%d", rep.Counts["REMOTE"]))
	}
	kv("Outstanding failed msgs", fmt.Sprintf("%d", rep.Outstanding))
	kv("Messages", fmt.Sprintf("%d of %d (%s)", rep.MsgsDone, rep.MsgsTotal,
		util.Pct(rep.MsgsDone, rep.MsgsTotal)))
	fmt.Println()
	fmt.Printf("Workers (%d):\n", len(rep.Workers))
	for _, wk := range rep.Workers {
		hb := "-"
		if wk.Heartbeat > 0 {
			hb = fmt.Sprintf("%ds ago", int(wk.HBAge))
		}
		fmt.Printf("  %-30s %-16s %-8s %d mailbox(es)  heartbeat %s\n",
			state.ShortWorker(wk.ID), wk.Host, wk.Status, wk.Active, hb)
	}
	if len(rep.Workers) == 0 {
		fmt.Println("  (no workers currently registered)")
	}
	return 0
}

// --------------------------------------------------------------- failed --

func cmdFailed(rest []string) int {
	fs := flag.NewFlagSet("failed", flag.ExitOnError)
	dbPath := fs.String("db", "./migration.db", "State database path")
	mailbox := fs.String("mailbox", "", "Only this mailbox (source user)")
	asJSON := fs.Bool("json", false, "Emit JSON")
	csvOut := fs.String("csv", "", "Export to a CSV file")
	all := fs.Bool("all", false, "Include RECOVERED / IGNORED")
	ignore := fs.Bool("ignore", false, "Mark the selection IGNORED (still skipped, no longer outstanding)")
	fs.Parse(reorderArgs(fs, rest))
	db, err := state.OpenForTest(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	defer db.Close()
	mid := db.MailboxIDByUser(*mailbox)
	if mid == -1 {
		fmt.Fprintln(os.Stderr, "mailbox not found:", *mailbox)
		return 1
	}
	if *ignore {
		n := db.SetFailedStatus("IGNORED", *mailbox, "", 0)
		fmt.Printf("marked %d registry entr%s IGNORED (still skipped; no longer counted as outstanding)\n",
			n, map[bool]string{true: "y", false: "ies"}[n == 1])
		return 0
	}
	rows := db.FailedRows(mid, !*all)
	if *asJSON {
		return report.FailedJSON(rows)
	}
	if *csvOut != "" {
		if err := report.WriteFailedCSVTo(rows, *csvOut); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			return 1
		}
		fmt.Printf("exported %d entr%s to %s\n", len(rows),
			map[bool]string{true: "y", false: "ies"}[len(rows) == 1], *csvOut)
		return 0
	}
	fmt.Println(identity.BannerLine())
	fmt.Println()
	if len(rows) == 0 {
		fmt.Println(progress.C("Failed Message Registry is clean — no outstanding failures.", "green"))
		return 0
	}
	fmt.Printf("Failed Message Registry — %d entr%s:\n\n", len(rows),
		map[bool]string{true: "y", false: "ies"}[len(rows) == 1])
	for _, r := range rows {
		fmt.Printf("  %-24s %-14s UID %-6d %-9s %-16s %s\n", clipS(r.Mailbox, 24),
			clipS(r.Folder, 14), r.SrcUID, util.FmtBytes(float64(r.Size)), r.FType, r.Status)
		fmt.Printf("      %s · %s · failed %dx · %s\n", orDashS(r.Subject),
			orDashS(r.Sender), r.FailCount, clipS(r.Reason, 80))
	}
	fmt.Println(progress.C("\nRetry: mailferry retry-failed [--mailbox USER] · "+
		"Export: --csv FILE / --json · Silence: --ignore", "cyan"))
	return 0
}

func cmdRetryFailed(rest []string) int {
	fs := flag.NewFlagSet("retry-failed", flag.ExitOnError)
	dbPath := fs.String("db", "./migration.db", "State database path")
	mailbox := fs.String("mailbox", "", "Only this mailbox (source user)")
	folder := fs.String("folder", "", "Only this folder")
	uid := fs.Int("uid", 0, "Only this source UID")
	fs.Parse(reorderArgs(fs, rest))
	db, err := state.OpenForTest(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	defer db.Close()
	n := db.SetFailedStatus("RETRY_PENDING", *mailbox, *folder, int64(*uid))
	if n == 0 {
		fmt.Println("no matching registry entries to retry")
		return 0
	}
	fmt.Printf("%d failed message(s) re-queued (RETRY_PENDING).\n", n)
	fmt.Println("Run the migration again — successes become RECOVERED; repeat " +
		"failures return to the registry.")
	return 0
}

func clipS(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func orDashS(s string) string {
	if s == "" {
		return "-"
	}
	if len(s) > 46 {
		return s[:46]
	}
	return s
}

// loadSystemRoots reports how many trusted CA roots the system store offers
// (0 with nil error means the platform verifier is used lazily — fine).
func loadSystemRoots() (int, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return 0, err
	}
	if pool == nil {
		return 0, nil
	}
	return len(pool.Subjects()), nil //nolint:staticcheck // count only, not used for verification
}
