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

package config

// mailferry.toml — optional, auto-generated, never fatal. Same keys and
// philosophy as the Python engine: sensible defaults always work; the file
// exists only so advanced users can tune behaviour. CLI flags override it.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ajsap/mailferry/v2/internal/identity"
)

const TOMLTemplate = `# MailFerry Configuration
# High-Performance Native IMAP Migration Engine
# Author: Andy Saputra <andy@saputra.org>
# https://github.com/ajsap/mailferry
#
# Generated automatically on first launch. Every value below is the
# built-in default — MailFerry behaves identically whether or not this
# file exists. Edit what you need; delete anything you don't. Command-line
# flags always override this file. Invalid values fall back to the
# default with a warning; unknown keys are ignored with a warning.

[migration]
# Mailboxes migrated concurrently (CSV rows in flight at once).
parallel_mailboxes = 10
# Extra folder pipelines *inside* one large mailbox (connection pairs).
parallel_folders = 3
# Simultaneous connections allowed per server host (be kind to servers).
per_host_connections = 8
# Every APPEND is confirmed via APPENDUID / destination probe before a
# message is marked done. Always on — recorded here for transparency.
verify_after_copy = true

[retry]
# Attempts per batch level during failure isolation (see [recovery]).
batch_attempts = 3
# Transfer passes per message before it is recorded as failed.
message_attempts = 3
# Reconnect attempts per folder for ordinary connection trouble.
reconnect_attempts = 5
# Whole-mailbox retry attempts after a hard failure.
mailbox_attempts = 2
# Base delay in seconds between mailbox retries (doubles each attempt).
retry_delay = 30
# Backoff strategy: "exponential" (only supported value).
reconnect_backoff = "exponential"

[recovery]
# Progressively isolate messages that repeatedly break the transfer
# (batch -> halves -> single) instead of retrying the same batch forever.
isolate_failed_messages = true
# Skip messages already recorded in the Failed Message Registry on
# future runs ("mailferry retry-failed" re-queues them explicitly).
skip_known_failed_messages = true
# Mark a mailbox stalled after this long without measurable progress
# and recover it automatically (0 disables the supervisor).
stale_timeout_seconds = 300
# Connection-recovery attempts per stall before Recovery Mode.
recovery_retries = 3
# Wait between connection-recovery attempts, in seconds.
recovery_interval_seconds = 30

[logging]
# "info" (default), "debug" (full tracebacks) or "trace" (wire protocol,
# credentials always redacted).
level = "info"
# Delete per-run logs older than this many days at startup (0 = keep all).
keep_days = 30

[dashboard]
# Dashboard refresh interval in milliseconds (display only).
refresh_ms = 250
# Show the live wire-throughput Speed column.
show_transfer_speed = true

[database]
# Worker/lease heartbeat interval in seconds.
heartbeat_seconds = 15
# A cluster worker silent for this long is offline; its mailboxes are
# reclaimed automatically by the remaining workers.
worker_timeout_seconds = 60
# Hard cap for any unexplained lease left in the State Database.
lock_timeout_seconds = 300
`

// DefaultTOMLPath honours MAILFERRY_CONFIG_DIR (used by tests).
func DefaultTOMLPath() string {
	if d := os.Getenv("MAILFERRY_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "mailferry.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "mailferry.toml"
	}
	return filepath.Join(home, ".config", "mailferry", "mailferry.toml")
}

// FindTOML: --config PATH > ./mailferry.toml > default location.
func FindTOML(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if _, err := os.Stat("mailferry.toml"); err == nil {
		return "mailferry.toml"
	}
	return DefaultTOMLPath()
}

// parseTOML handles the subset MailFerry generates: [sections],
// key = value with strings/ints/floats/bools. Junk lines raise.
func parseTOML(text string) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	section := ""
	for n, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if _, ok := out[section]; !ok {
				out[section] = map[string]any{}
			}
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			return nil, fmt.Errorf("line %d: expected 'key = value' or '[section]'", n+1)
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		if i := strings.Index(v, "#"); i >= 0 && !strings.HasPrefix(v, `"`) && !strings.HasPrefix(v, "'") {
			v = strings.TrimSpace(v[:i])
		}
		var val any
		switch {
		case strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) && len(v) >= 2:
			val = v[1 : len(v)-1]
		case strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") && len(v) >= 2:
			val = v[1 : len(v)-1]
		case v == "true", v == "false":
			val = v == "true"
		default:
			if iv, err := strconv.ParseInt(v, 10, 64); err == nil {
				val = iv
			} else if fv, err := strconv.ParseFloat(v, 64); err == nil {
				val = fv
			} else {
				val = v
			}
		}
		if section == "" {
			return nil, fmt.Errorf("line %d: key outside a [section]", n+1)
		}
		out[section][k] = val
	}
	return out, nil
}

// templateEntry is one documented option from TOMLTemplate — the template
// doubles as the catalogue of known options with their comments.
type templateEntry struct {
	Section  string
	Key      string
	Comments []string
	Line     string // "key = default"
}

func templateCatalog() []templateEntry {
	var out []templateEntry
	section := ""
	var pending []string
	for _, raw := range strings.Split(TOMLTemplate, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			pending = nil
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = strings.TrimSpace(line[1 : len(line)-1])
			pending = nil
		case strings.HasPrefix(line, "#"):
			if section != "" {
				pending = append(pending, line)
			}
		case strings.Contains(line, "=") && section != "":
			key := strings.TrimSpace(line[:strings.Index(line, "=")])
			out = append(out, templateEntry{section, key, pending, line})
			pending = nil
		}
	}
	return out
}

// upgradeAppend documents options this MailFerry version knows about that an
// older configuration file does not mention. It ONLY ever appends a fully
// commented block (never touches existing lines, never re-opens [sections]),
// so user customisations are preserved and the file stays valid for strict
// TOML parsers. Returns how many options were documented.
func upgradeAppend(path, raw string, tree map[string]map[string]any) int {
	var missing []templateEntry
	for _, e := range templateCatalog() {
		if body, ok := tree[e.Section]; ok {
			if _, ok2 := body[e.Key]; ok2 {
				continue // present and active
			}
		}
		if strings.Contains(raw, e.Key) {
			continue // mentioned somewhere (commented out counts) — leave alone
		}
		missing = append(missing, e)
	}
	if len(missing) == 0 {
		return 0
	}
	var b strings.Builder
	if !strings.HasSuffix(raw, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n# ---- New options documented by MailFerry v" + identity.Version +
		" on " + time.Now().Format("2006-01-02") + " ----\n")
	b.WriteString("# These are the built-in defaults, shown for reference only. To change\n")
	b.WriteString("# one, uncomment it under its [section] above (adding the section if it\n")
	b.WriteString("# is missing). This block is safe to edit or delete.\n")
	for _, e := range missing {
		b.WriteString("#\n# [" + e.Section + "]\n")
		for _, cline := range e.Comments {
			b.WriteString("# " + strings.TrimPrefix(cline, "# ") + "\n")
		}
		b.WriteString("# " + e.Line + "\n")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0 // best effort: read-only config is fine
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		return 0
	}
	return len(missing)
}

// LoadTOML applies the config file onto a defaults Run. Never fatal: every
// problem is a warning and the default stands. Returns (warnings, path,
// created).
func LoadTOML(r *Run, explicit string, generate bool) ([]string, string, bool) {
	var warns []string
	path := FindTOML(explicit)
	if _, err := os.Stat(path); err != nil {
		if explicit != "" {
			return []string{fmt.Sprintf("config file %s not found — using built-in defaults", path)}, path, false
		}
		if generate {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
				if err := os.WriteFile(path, []byte(TOMLTemplate), 0o644); err == nil {
					return nil, path, true
				}
			}
			warns = append(warns, fmt.Sprintf("could not create %s — using built-in defaults", path))
		}
		return warns, path, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{fmt.Sprintf("config %s: %v — using built-in defaults", path, err)}, path, false
	}
	tree, err := parseTOML(string(data))
	if err != nil {
		return []string{fmt.Sprintf("config %s: could not parse (%v) — using built-in defaults", path, err)}, path, false
	}
	if n := upgradeAppend(path, string(data), tree); n > 0 {
		warns = append(warns, fmt.Sprintf(
			"config: documented %d new option(s) at the end of %s (commented "+
				"defaults — your settings are untouched)", n, path))
	}

	num := func(v any) (float64, bool) {
		switch x := v.(type) {
		case int64:
			return float64(x), true
		case float64:
			return x, true
		}
		return 0, false
	}
	setInt := func(dst *int, v any, min float64) bool {
		if f, ok := num(v); ok && f >= min {
			*dst = int(f)
			return true
		}
		return false
	}
	setF := func(dst *float64, v any, min float64) bool {
		if f, ok := num(v); ok && f >= min {
			*dst = f
			return true
		}
		return false
	}
	setB := func(dst *bool, v any) bool {
		if b, ok := v.(bool); ok {
			*dst = b
			return true
		}
		return false
	}

	for sect, body := range tree {
		for key, val := range body {
			full := sect + "." + key
			ok := true
			switch full {
			case "migration.parallel_mailboxes":
				ok = setInt(&r.Workers, val, 1)
			case "migration.parallel_folders":
				ok = setInt(&r.MaxConnsPerBox, val, 1)
			case "migration.per_host_connections":
				ok = setInt(&r.PerHostConns, val, 1)
			case "migration.verify_after_copy":
				var x bool
				ok = setB(&x, val) // always on; accepted for transparency
			case "retry.batch_attempts":
				ok = setInt(&r.BatchAttempts, val, 1)
			case "retry.message_attempts":
				ok = setInt(&r.MsgRetries, val, 1)
			case "retry.reconnect_attempts":
				ok = setInt(&r.ReconnectAttempts, val, 1)
			case "retry.mailbox_attempts":
				ok = setInt(&r.Retries, val, 0)
			case "retry.retry_delay":
				ok = setF(&r.RetryDelay, val, 1)
			case "retry.reconnect_backoff":
				s, isS := val.(string)
				ok = isS && s == "exponential"
			case "recovery.isolate_failed_messages":
				ok = setB(&r.IsolateFailed, val)
			case "recovery.skip_known_failed_messages":
				ok = setB(&r.SkipKnownFailed, val)
			case "recovery.stale_timeout_seconds":
				ok = setF(&r.StaleTimeout, val, 0)
			case "recovery.recovery_retries":
				ok = setInt(&r.RecoveryRetries, val, 1)
			case "recovery.recovery_interval_seconds":
				ok = setF(&r.RecoveryInterval, val, 1)
			case "logging.level":
				s, isS := val.(string)
				if ok = isS && (s == "info" || s == "debug" || s == "trace"); ok {
					r.Debug = r.Debug || s == "debug"
					r.Trace = r.Trace || s == "trace"
				}
			case "logging.keep_days":
				ok = setInt(&r.LogKeepDays, val, 0)
			case "dashboard.refresh_ms":
				ok = setInt(&r.RefreshMS, val, 50)
			case "dashboard.show_transfer_speed":
				var x bool
				ok = setB(&x, val)
			case "database.heartbeat_seconds":
				ok = setF(&r.DBHeartbeat, val, 5)
			case "database.worker_timeout_seconds":
				ok = setF(&r.WorkerTimeout, val, 20)
			case "database.lock_timeout_seconds":
				ok = setF(&r.LockTimeout, val, 30)
			default:
				warns = append(warns, fmt.Sprintf(
					"config: unknown setting '%s' ignored (kept for forward compatibility)", full))
				continue
			}
			if !ok {
				warns = append(warns, fmt.Sprintf(
					"config: %s = %v invalid — using the default", full, val))
			}
		}
	}
	return warns, path, false
}
