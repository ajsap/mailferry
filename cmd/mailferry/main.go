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

// mailferry: the command-line entry point.
package main

import (
	"context"
	"crypto/x509"
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

	mailferry "github.com/ajsap/mailferry/v2"
	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/engine"
	"github.com/ajsap/mailferry/v2/internal/fakeimap"
	"github.com/ajsap/mailferry/v2/internal/identity"
	"github.com/ajsap/mailferry/v2/internal/imapx"
	"github.com/ajsap/mailferry/v2/internal/paths"
	"github.com/ajsap/mailferry/v2/internal/progress"
	"github.com/ajsap/mailferry/v2/internal/report"
	"github.com/ajsap/mailferry/v2/internal/state"
	"github.com/ajsap/mailferry/v2/internal/termstate"
	"github.com/ajsap/mailferry/v2/internal/tui"
	"github.com/ajsap/mailferry/v2/internal/util"
)

func termInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

func main() {
	code := run(os.Args[1:])
	termstate.Snapshot("pre-exit")
	os.Exit(code)
}

var commands = map[string]bool{"run": true, "resume": true, "check": true,
	"validate": true, "doctor": true, "benchmark": true, "init": true,
	"import-state": true, "capabilities": true, "verify": true, "compact": true,
	"changelog": true, "roadmap": true, "status": true, "failed": true,
	"retry-failed": true, "config": true, "version": true, "about": true,
	"dedup": true, "attach": true, "term-diag": true}

// bootCfg is the file-configured baseline every command starts from.
// mailferry.toml: sensible defaults that just work; the file only exists so
// advanced users can tune behaviour. CLI flags always win. A missing or
// broken file can never stop MailFerry from starting.
//
// The bootstrap is STRICTLY READ-ONLY: informational commands (--help,
// version, about, changelog, roadmap, config paths, …) must never create
// configuration, directories or any application state. Configuration is
// generated only on the first OPERATIONAL run (run/resume) or by the
// explicit `mailferry config` command — see ensureConfig.
var (
	bootCfg      *config.Run
	bootPath     string
	bootExplicit string
	bootCreated  bool // set by ensureConfig, never by the bootstrap
)

// ensureConfig performs the explicit/operational configuration ensure:
// creates the documented default mailferry.toml if absent (never
// overwrites), appends newly-known options as commented defaults, and
// restricts permissions. Reports and records creation.
func ensureConfig(session func(string)) {
	fresh := config.Defaults()
	warns, path, created := config.LoadTOML(fresh, bootExplicit, true)
	bootPath, bootCreated = path, created
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, progress.C("note: "+w, "yellow"))
	}
	if created {
		fmt.Fprintln(os.Stderr, progress.C("Created configuration:\n  "+path, "cyan"))
		if session != nil {
			session("configuration: wrote a documented default mailferry.toml to " + path)
		}
	}
}

func bootstrapConfig(argv []string) []string {
	// extract the global --portable flag first so every command sees it and
	// the config location below resolves against the portable root.
	argv = extractPortable(argv)
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
	// Portable precedence: an explicit --config always wins. Otherwise the
	// portable root's mailferry.toml is authoritative — it resolves through the
	// normal FindTOML path (paths.Default() is portable-aware), so it is
	// auto-generated on the first operational run exactly like the native
	// default, rather than being treated as a fixed --config (which is never
	// auto-created).
	bootCfg = config.Defaults()
	bootExplicit = explicit
	warns, path, _ := config.LoadTOML(bootCfg, explicit, false) // read-only
	bootPath = path
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, progress.C("note: "+w, "yellow"))
	}
	return argv
}

func run(argv []string) int {
	// BEFORE the first byte of output: never print into a broken terminal.
	// A previous full-screen program (any program) that died in raw mode
	// leaves the tty without newline translation — every "\n" would then
	// render as a stair-step, starting with our own banner, and a plain
	// capture/restore would preserve that damage forever. Repair the
	// cooked-mode flags first; every later capture then snapshots the
	// SANE state, so no exit path can hand the poison back to the shell.
	termstate.Snapshot("entry")
	if repairs := termstate.Sanitize(); len(repairs) > 0 {
		termstate.Snapshot("post-sanitise", repairs...)
		fmt.Fprintf(os.Stderr, "note: repaired inherited terminal state (%s) — "+
			"a previous full-screen program exited uncleanly\n", strings.Join(repairs, " "))
	} else {
		termstate.Snapshot("post-sanitise")
	}
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
	case "dedup":
		return cmdDedup(rest)
	case "attach":
		return cmdAttach(rest)
	case "term-diag":
		return cmdTermDiag(rest)
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
  mailferry attach [RUN-ID]      live read-only monitor of a running migration
  mailferry failed               list / export the Failed Message Registry
  mailferry retry-failed         re-queue registry entries for the next run
  mailferry dedup CSV            find/quarantine destination duplicates (safe; --execute)
  mailferry verify CSV           compare per-folder counts: source vs destination vs state
  mailferry capabilities HOST PORT   probe a server's capabilities
  mailferry doctor               local environment self-test (no servers contacted)
  mailferry term-diag            terminal output self-test (no servers, no data);
                                 MAILFERRY_TERM_DIAG=FILE records termios stages
  mailferry benchmark            loopback throughput benchmark (in-process servers)
  mailferry compact [--db PATH]  prune per-message rows for completed folders
  mailferry import-state FILE    import the old wrapper's migration.state
  mailferry init FILE            write a CSV template
  mailferry config               show / create the mailferry.toml configuration
  mailferry config paths         display all canonical paths (creates nothing)
  mailferry changelog | roadmap  release history / project roadmap
  mailferry version | about

Frequent flags: --workers N --db PATH (native per-user mailferry.db
  by default) --logs-dir DIR --timeout S
  --retries N --retry-delay S --per-host-conns N --skip-completed
  --order csv|size --include GLOB --exclude GLOB --map FILE --sync-flags
  --compress auto|off --baseline --tls-no-verify --no-tui --trace --config PATH
  --portable (run entirely from the executable's own directory)
Run 'mailferry run -h' for the full list.

Author:     ` + identity.Author + ` <` + identity.AuthorEmail + `>
Repository: ` + identity.Repository + `
Support:    ` + identity.SupportURL + `
License:    ` + identity.LicenseShort + ` — run 'mailferry about' for details.
`)
}

// resolveStateDB determines the effective State Database path without
// creating anything: CLI/TOML override > native per-OS default. When the
// native default is selected and a legacy development database
// (./migration.db) exists while the native one does not, the caller gets
// legacyHint to surface — MailFerry never silently picks between two
// candidate authoritative databases.
func resolveStateDB(override string) (db string, legacyHint string) {
	if override != "" {
		return override, ""
	}
	nat := paths.Default()
	if _, err := os.Stat(nat.StateDB); err == nil {
		return nat.StateDB, ""
	}
	if _, err := os.Stat(nat.LegacyStateDB); err == nil {
		return nat.StateDB, fmt.Sprintf(
			"old development State Database detected: %s\n"+
				"canonical location: %s\n"+
				"MailFerry will not choose between them automatically. Either\n"+
				"  keep using it:   --db %s\n"+
				"  or adopt the canonical location by moving it:\n"+
				"  mv %s \"%s\"",
			nat.LegacyStateDB, nat.StateDB, nat.LegacyStateDB,
			nat.LegacyStateDB, nat.StateDB)
	}
	return nat.StateDB, ""
}

// requireExistingDB resolves --db for read-only State Database commands
// (status/failed/retry-failed/verify/compact) and refuses to CREATE a
// database as a side effect: absent DB is an error, never an empty file.
func requireExistingDB(override string) (string, bool) {
	db, hint := resolveStateDB(override)
	if _, err := os.Stat(db); err != nil {
		fmt.Fprintln(os.Stderr, "no State Database at", db)
		if hint != "" {
			fmt.Fprintln(os.Stderr, hint)
		} else {
			fmt.Fprintln(os.Stderr, "(nothing has been migrated on this machine yet, "+
				"or pass --db PATH)")
		}
		return db, false
	}
	return db, true
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
		logsDir      = fs.String("logs-dir", "", "Directory for logs (default: the native per-OS location)")
		dbPath       = fs.String("db", "", "State database path (default: the native per-user mailferry.db)")
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
		dryRun       = fs.Bool("dry-run", false, "Plan/scan a real run but write nothing (server + State DB read-only)")
		fromF        = fs.String("from", "", "Only migrate messages on/after this ISO 8601 instant (INTERNALDATE)")
		toF          = fs.String("to", "", "Only migrate messages on/before this ISO 8601 instant (INTERNALDATE)")
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
	cfg.CheckOnly = cmd == "check" || cmd == "validate" || *checkF
	// --dry-run is a full, read-only run: the engine plans, scans and reports
	// exactly as a real run would, but every server mutation is blocked at the
	// choke point (see imapx.Client.ReadOnly). It is NOT the same as --check
	// (the lightweight connect/list preflight above). A dry run is also forced
	// EPHEMERAL so the projection never mutates the persistent State Database —
	// together these make a dry run provably side-effect-free on both sides.
	cfg.DryRun = *dryRun
	if cfg.DryRun {
		cfg.Ephemeral = true
	}

	// Resolve the ISO 8601 --from/--to window ONCE, here, to fixed instants,
	// before any file is opened or any connection is attempted, so a bad range
	// fails instantly. time.Local is the zone applied to timezone-less inputs.
	rng, rerr := config.ResolveRange(*fromF, *toF, time.Local)
	if rerr != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", rerr)
		return 2
	}
	cfg.Range = rng

	specs, err := config.ParseCSV(cfg.CSVFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	if cfg.CheckOnly {
		return runCheck(cfg, specs) // preflight: nothing written locally
	}

	// Portable precedence: portable beats a TOML database.path / logging
	// directory, but an explicit --db / --logs-dir still wins. Clearing the
	// TOML-derived value lets the resolvers below fall through to the portable
	// root (paths.Default() is portable-aware). Then guard writability with a
	// clear, actionable error for a read-only portable location.
	if paths.PortableActive() {
		if !flagWasSet(fs, "db") {
			cfg.DBPath = ""
		}
		if !flagWasSet(fs, "logs-dir") {
			cfg.LogsDir = ""
		}
		guardDirs := []string{paths.Default().LogsDir}
		if !cfg.Ephemeral && cfg.DBPath == "" {
			guardDirs = append(guardDirs, filepath.Dir(paths.Default().StateDB))
		}
		if err := portableWritableGuard(guardDirs...); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			return 1
		}
	}

	// First OPERATIONAL run: this is the point where configuration and
	// application state may be created — never during informational use.
	ensureConfig(nil)

	if !cfg.Ephemeral {
		db, legacyHint := resolveStateDB(cfg.DBPath)
		if legacyHint != "" {
			fmt.Fprintln(os.Stderr, legacyHint)
			return 1
		}
		cfg.DBPath = db
		if err := paths.EnsureParent(cfg.DBPath); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			return 1
		}
	}
	if cfg.LogsDir == "" {
		cfg.LogsDir = paths.Default().LogsDir
	}
	if err := paths.EnsureDir(cfg.LogsDir); err != nil {
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
	defer paths.RestrictDB(cfg.DBPath)
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
	termstate.Snapshot("pre-banner")
	fmt.Println(identity.BannerLine())
	fmt.Println(identity.Slogan)
	fmt.Println()
	fmt.Printf("Starting migration: %d mailbox(es), %d worker(s), State Database %s\n",
		len(specs), cfg.Workers, stats.DBPath)
	fmt.Printf("Run %s · worker %s — multiple MailFerry processes may share this "+
		"State Database safely\n", cfg.RunID, state.ShortWorker(state.LeaseOwnerID()))
	termstate.Snapshot("post-banner")
	if cfg.DryRun {
		fmt.Println(progress.C("DRY RUN — planning and scanning only; nothing will be "+
			"written to either server or the State Database.", "yellow"))
	}
	if cfg.Range.Active {
		fmt.Printf("Date range: %s\n", cfg.Range.Label())
	}
	session.Log(fmt.Sprintf("=== %s — run %s start: csv=%s rows=%d workers=%d db=%s%s%s%s%s",
		identity.BannerLine(), cfg.RunID, stats.CSVFile, len(specs), cfg.Workers, stats.DBPath,
		map[bool]string{true: " force"}[cfg.Force], map[bool]string{true: " ephemeral"}[cfg.Ephemeral],
		map[bool]string{true: " dry-run"}[cfg.DryRun],
		map[bool]string{true: " range=" + cfg.Range.Label()}[cfg.Range.Active]))

	// Automatic terminal detection: interactive TTY -> Bubble Tea TUI;
	// non-interactive or --no-tui -> headless. Both drive the SAME engine.
	interactive := progress.IsTTY && termInteractive() && !cfg.NoTUI
	ctx, cancel := context.WithCancel(context.Background())
	bus := engine.NewBus()
	start := time.Now()
	interrupted := false

	var res engine.RunResult
	var runErr error
	resultsShown := false
	if interactive {
		res, runErr, interrupted, resultsShown = runInteractive(ctx, cancel, cfg, specs, stats, bus, session)
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
	termstate.Snapshot("pre-report")
	if resultsShown && !interrupted {
		// The operator reviewed the Results screen inside the TUI and quit
		// deliberately — print a short confirmation, never a second report.
		fmt.Printf("Run %s complete: %d successful · %d with warnings · %d failed\n",
			cfg.RunID, res.Counts["SUCCESS"], res.Counts["WARNINGS"], res.Counts["FAILED"])
		fmt.Println("Results: " + resultsPath + " · logs: " + cfg.LogsDir)
		if res.Outstanding > 0 {
			fmt.Println(progress.C(fmt.Sprintf(
				"Outstanding failed messages: %d — mailferry failed · mailferry retry-failed",
				res.Outstanding), "yellow"))
		}
	} else {
		report.PrintSummary(snap, resultsPath, cfg, runtimeS, interrupted, progress.C)
		if n := res.Counts["REMOTE"]; n > 0 {
			fmt.Println(progress.C(fmt.Sprintf("Handled by other workers : %d mailbox(es) were "+
				"actively owned by other MailFerry processes (details above and in the "+
				"session log) — they were mirrored, not duplicated.", n), "cyan"))
		}
		report.PrintFailedSection(res.FailedRegistry, res.Outstanding, cfg.LogsDir, progress.C)
	}
	if bootCreated {
		fmt.Println(progress.C("note: a documented default configuration was written to "+
			bootPath+" on this first operational run.", "cyan"))
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

// captureTerminal snapshots the exact terminal state before a TUI takes
// over and returns a restorer that reinstates it verbatim. Bubble Tea
// restores on every normal path (verified by the exit-path matrix in
// tools/ptyprobe-derived tests); this converts that expectation into a
// GUARANTEE for every current and future return path — including TUI
// startup failures and third-party edge cases — without reverting or
// weakening the hard-stop fix. The restorer also re-enables cursor +
// primary screen and disables mouse reporting defensively; those
// sequences are idempotent no-ops on an already-clean terminal.
//
// Because termstate.Sanitize runs before any output, the state captured
// here is always a SANE cooked state — restoring it can never hand a
// poisoned terminal back to the shell (the v2.0.1 defect class).
func captureTerminal() func() {
	fd := int(os.Stdin.Fd())
	st, err := term.GetState(fd)
	if err != nil {
		return func() {
			fmt.Fprint(os.Stdout, "\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1049l\x1b[?25h\x1b[0m")
		}
	}
	return func() {
		_ = term.Restore(fd, st)
		// leave alternate screen, show cursor, reset attributes and
		// mouse reporting — harmless when already clean
		fmt.Fprint(os.Stdout, "[?1002l[?1003l[?1006l[?1049l[?25h[0m")
	}
}

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
	session *report.Session) (engine.RunResult, error, bool, bool) {
	session.Log(fmt.Sprintf("=== %s — run %s start (TUI): rows=%d workers=%d db=%s",
		identity.BannerLine(), cfg.RunID, len(specs), cfg.Workers, cfg.DBPath))
	var res engine.RunResult
	var runErr error
	done := make(chan struct{})
	resCh := make(chan tui.ResultMsg, 1)
	go func() {
		defer func() { // a crashed engine must never leave the terminal raw
			if r := recover(); r != nil {
				runErr = fmt.Errorf("engine panic: %v", r)
				session.Log(fmt.Sprintf("FATAL engine panic: %v", r))
			}
			if runErr == nil {
				// Authoritative results payload for the TUI Results view,
				// delivered BEFORE the done signal so the final screen
				// renders complete on its first frame. Reports are written
				// now so every path on screen already exists.
				snapNow := stats.Snapshot()
				resultsCSV := report.WriteResultsCSV(snapNow, cfg.LogsDir)
				failedCSV := ""
				if len(res.FailedRegistry) > 0 {
					failedCSV = filepath.Join(cfg.LogsDir, "failed_messages.csv")
					_ = report.WriteFailedCSVTo(res.FailedRegistry, failedCSV)
				}
				rangeLabel := ""
				if cfg.Range.Active {
					rangeLabel = cfg.Range.Label()
				}
				resCh <- tui.ResultMsg{
					Res: res, RunID: cfg.RunID,
					WorkerID: state.ShortWorker(state.LeaseOwnerID()),
					DryRun:   cfg.DryRun, RangeLabel: rangeLabel,
					Portable: paths.PortableActive(), Ephemeral: cfg.Ephemeral,
					ResultsCSV: resultsCSV, FailedCSV: failedCSV,
					SessionLog: filepath.Join(cfg.LogsDir, "session.log"),
					Runtime:    time.Since(snapNow.BatchStart).Seconds(),
				}
			}
			close(resCh)
			close(done)
		}()
		res, runErr = engine.RunMigrationBus(ctx, cfg, specs, stats, bus, session.Log,
			report.MailboxLoggerFactory(cfg.LogsDir))
	}()
	hardRequested := false
	gracefulStop := func() {
		session.Log("interrupt received (Ctrl+C) — graceful stop: no new work, active " +
			"workers finish the current message, then connections close")
		cancel()
		escalate(bus, session, done)
	}
	hardStop := func() {
		// NEVER os.Exit while Bubble Tea owns the terminal: abort the
		// engine, let the TUI quit and restore the terminal, then the
		// bounded wait below finishes the process.
		hardRequested = true
		session.Log("second interrupt — immediate abort (state stays consistent)")
		bus.AbortAllConnections()
		cancel()
	}
	model := tui.New(stats, bus, gracefulStop, hardStop,
		time.Duration(cfg.RefreshMS)*time.Millisecond, done)
	restoreTerm := captureTerminal()
	termstate.Snapshot("pre-tui")
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
	go func() { // forward the results payload into the running program
		if r, ok := <-resCh; ok {
			p.Send(r)
		}
	}()
	// SIGHUP (SSH drop, terminal window closed) previously killed the
	// process mid-raw-mode — the classic way a session ends up with the
	// stair-step terminal. Treat it as a graceful stop: abort the engine,
	// unwind the TUI cleanly, restore the terminal, exit as interrupted.
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	go func() {
		if _, ok := <-hupCh; ok {
			session.Log("SIGHUP received (terminal hangup) — graceful stop; " +
				"terminal state restored, state committed per message")
			cancel()
			p.Send(tea.QuitMsg{})
		}
	}()
	finalModel, runUIErr := p.Run()
	signal.Stop(hupCh)
	close(hupCh)
	restoreTerm() // guarantee: cooked mode + primary screen on EVERY path
	termstate.Snapshot("post-tui")
	resultsShown := false
	if mm, ok := finalModel.(*tui.Model); ok {
		resultsShown = mm.ResultsShown()
	}
	if runUIErr != nil {
		// TUI failed: never abort the migration — fall back to waiting headless
		fmt.Fprintln(os.Stderr, "note: TUI unavailable (", runUIErr, ") — continuing headless")
	}
	// terminal is restored here; bound the engine wait after a hard stop
	if hardRequested {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			fmt.Fprintln(os.Stderr, "hard stop: engine still unwinding — exiting; "+
				"state is committed per message and the next run resumes cleanly")
			os.Exit(130)
		}
	} else {
		<-done
	}
	return res, runErr, ctx.Err() != nil, resultsShown
}

// runHeadless runs the same engine with clean structured console output.
func runHeadless(ctx context.Context, cancel context.CancelFunc, cfg *config.Run,
	specs []config.MailboxSpec, stats *engine.Stats, bus *engine.Bus,
	session *report.Session) (engine.RunResult, error, bool) {
	progress.IsTTY = false
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
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
	// Surface notable coordination events on stdout — a headless process
	// must never appear to do nothing without saying why.
	notable := map[string]bool{"Mailbox already active": true, "Worker takeover": true,
		"Stale lock auto-reset": true, "Job released — resuming": true,
		"Completed by another worker": true, "Completed with warnings": true}
	histStop := make(chan struct{})
	go func() {
		sub := bus.Subscribe()
		seen := 0
		for {
			select {
			case <-histStop:
				return
			case <-sub:
			case <-time.After(2 * time.Second):
			}
			hist := bus.HistorySnapshot()
			for ; seen < len(hist); seen++ {
				e := hist[seen]
				if notable[e.Event] {
					fmt.Printf("%s: %s — %s\n", e.Event, e.Mailbox, e.Details)
				}
			}
		}
	}()
	res, runErr := engine.RunMigrationBus(ctx, cfg, specs, stats, bus, session.Log,
		report.MailboxLoggerFactory(cfg.LogsDir))
	close(done)
	close(histStop)
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
	dbPath := fs.String("db", "", "State database path (default: the native per-user mailferry.db)")
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
	resolved, okDB := requireExistingDB(*dbPath)
	if !okDB {
		return 1
	}
	db, err := state.OpenForTest(resolved)
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
	dbPath := fs.String("db", "", "State database path (default: the native per-user mailferry.db)")
	fs.Parse(reorderArgs(fs, rest))
	resolved, okDB := requireExistingDB(*dbPath)
	if !okDB {
		return 1
	}
	db, err := state.OpenForTest(resolved)
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
	dbPath := fs.String("db", "", "State database path (default: the native per-user mailferry.db)")
	fs.Parse(reorderArgs(fs, rest))
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: mailferry import-state STATEFILE [--db PATH]")
		return 2
	}
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
	db, err := state.Open(resolved, false, 300)
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
	fmt.Printf("Imported %d completed mailbox record(s) from %s into %s.\n", n, fs.Arg(0), resolved)
	fmt.Println("Those mailboxes will get a cheap incremental pass (or be skipped with --skip-completed).")
	return 0
}

// ------------------------------------------------------------ changelog --

func cmdChangelog(rest []string) int {
	fs := flag.NewFlagSet("changelog", flag.ExitOnError)
	full := fs.Bool("full", false, "Show the entire changelog")
	fs.Parse(reorderArgs(fs, rest))
	if *full {
		fmt.Println(strings.TrimRight(mailferry.Changelog, "\n"))
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
		if m := re.FindStringSubmatch(mailferry.Changelog); m != nil {
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
	// `config paths` — display every canonical location WITHOUT creating
	// anything (displaying a path must not require it to exist).
	if len(rest) > 0 && rest[0] == "paths" {
		p := paths.Default()
		fmt.Printf("%s\n%s\n\n", identity.BannerLine(), identity.Slogan)
		mark := func(fp string) string {
			if _, err := os.Stat(fp); err == nil {
				return ""
			}
			return "   (not created yet)"
		}
		cfgPath, _ := config.FindTOML(bootExplicit)
		dbPath, _ := resolveStateDB("")
		if paths.PortableActive() {
			fmt.Println(progress.C("(portable mode — everything lives beside the executable: "+
				paths.PortableRoot()+")", "cyan"))
			fmt.Println()
		}
		fmt.Printf("Configuration : %s%s\n", cfgPath, mark(cfgPath))
		fmt.Printf("State Database: %s%s\n", dbPath, mark(dbPath))
		fmt.Printf("Logs          : %s%s\n", p.LogsDir, mark(p.LogsDir))
		fmt.Printf("Cache         : %s%s\n", p.CacheDir, mark(p.CacheDir))
		if paths.PortableActive() {
			fmt.Println("\nPrecedence: CLI flags (--config/--db/--logs-dir) > portable " +
				"(--portable) >")
			fmt.Println("mailferry.toml > native OS default. Everything is created lazily, only")
			fmt.Println("when an operation needs it — informational commands never create files.")
		} else {
			fmt.Println("\nPrecedence: CLI flags (--config/--db/--logs-dir) > mailferry.toml >")
			fmt.Println("native OS default. Everything is created lazily, only when an")
			fmt.Println("operation needs it — informational commands never create files.")
		}
		return 0
	}
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	pathOnly := fs.Bool("path", false, "Print only the active config path")
	fs.Parse(rest)
	if *pathOnly {
		fmt.Println(bootPath)
		return 0
	}
	// Explicit configuration request: creating the default file here is
	// intentional (the ONE informational-family command allowed to).
	ensureConfig(nil)
	fmt.Printf("%s\n%s\n\n", identity.BannerLine(), identity.Slogan)
	note := ""
	if bootCreated {
		note = "   (created just now)"
	} else if _, err := os.Stat(bootPath); err != nil {
		note = "   (missing — could not be created; using built-in defaults)"
	}
	fmt.Printf("Configuration file : %s%s\n", bootPath, note)
	fmt.Printf("Default location   : %s\n", config.DefaultTOMLPath())
	fmt.Println("Search order       : --config PATH > ./mailferry.toml > native OS location")
	fmt.Println("\nEvery option is documented inside the file itself. CLI flags")
	fmt.Println("always override it; deleting it is always safe. New options in")
	fmt.Println("future versions are appended as commented defaults — your own")
	fmt.Println("settings are never rewritten. `mailferry config paths` shows all")
	fmt.Println("canonical locations without creating them.")
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
	dbPath := fs.String("db", "", "State database path (default: the native per-user mailferry.db)")
	workerTO := fs.Float64("worker-timeout", 60, "Offline threshold (s)")
	fs.Parse(reorderArgs(fs, rest))
	resolved, ok := requireExistingDB(*dbPath)
	if !ok {
		return 1
	}
	db, err := state.OpenForTest(resolved) // read-only snapshot; never competes with workers
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
	dbPath := fs.String("db", "", "State database path (default: the native per-user mailferry.db)")
	mailbox := fs.String("mailbox", "", "Only this mailbox (source user)")
	asJSON := fs.Bool("json", false, "Emit JSON")
	csvOut := fs.String("csv", "", "Export to a CSV file")
	all := fs.Bool("all", false, "Include RECOVERED / IGNORED")
	ignore := fs.Bool("ignore", false, "Mark the selection IGNORED (still skipped, no longer outstanding)")
	fs.Parse(reorderArgs(fs, rest))
	resolved, okDB := requireExistingDB(*dbPath)
	if !okDB {
		return 1
	}
	db, err := state.OpenForTest(resolved)
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
		fmt.Printf("      %s · %s · failed %dx · %s\n",
			util.DecodeEllipsize(orDashS(r.Subject), 64),
			util.DecodeEllipsize(orDashS(r.Sender), 40), r.FailCount, clipS(r.Reason, 80))
	}
	fmt.Println(progress.C("\nRetry: mailferry retry-failed [--mailbox USER] · "+
		"Export: --csv FILE / --json · Silence: --ignore", "cyan"))
	return 0
}

func cmdRetryFailed(rest []string) int {
	fs := flag.NewFlagSet("retry-failed", flag.ExitOnError)
	dbPath := fs.String("db", "", "State database path (default: the native per-user mailferry.db)")
	mailbox := fs.String("mailbox", "", "Only this mailbox (source user)")
	folder := fs.String("folder", "", "Only this folder")
	uid := fs.Int("uid", 0, "Only this source UID")
	fs.Parse(reorderArgs(fs, rest))
	resolved, okDB := requireExistingDB(*dbPath)
	if !okDB {
		return 1
	}
	db, err := state.OpenForTest(resolved)
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
