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

// Package config: run configuration, wrapper-compatible CSV parsing and
// the optional mailferry.toml (same keys as the Python engine).
package config

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Endpoint struct {
	Host     string
	Port     int
	Security string // ssl | tls | none
	User     string
	Password string
}

func (e Endpoint) Label() string { return e.User + "@" + e.Host }

type MailboxSpec struct {
	Index int
	Src   Endpoint
	Dst   Endpoint
}

func (m MailboxSpec) Label() string { return m.Src.User + "@" + m.Src.Host }
func (m MailboxSpec) Key() string {
	return strings.Join([]string{m.Src.Host, m.Src.User, m.Dst.Host, m.Dst.User}, "\x1f")
}

// Run holds every tunable. Field defaults mirror the Python engine.
type Run struct {
	CSVFile           string
	Workers           int
	LogsDir           string
	DBPath            string
	Ephemeral         bool
	Force             bool
	SkipCompleted     bool
	Retries           int
	RetryDelay        float64
	Order             string
	MaxConnsPerBox    int
	PerHostConns      int
	Timeout           float64 // inactivity watchdog seconds
	StaleTimeout      float64
	RecoveryRetries   int
	RecoveryInterval  float64
	LockTimeout       float64
	WorkerTimeout     float64
	BatchAttempts     int
	ReconnectAttempts int
	IsolateFailed     bool
	SkipKnownFailed   bool
	LogKeepDays       int
	DBHeartbeat       float64
	Include           []string
	Exclude           []string
	MapFile           string
	GmailAllMail      bool
	Subscribe         bool
	SyncFlags         bool
	RescanDest        bool
	NoDedupScan       bool
	NoTUI             bool
	Trace             bool
	Debug             bool
	CheckOnly         bool
	Compress          string // auto | off (COMPRESS=DEFLATE when offered)
	Baseline          bool   // RFC-3501-only conservative mode
	ResetStaleLocks   bool   // headless verified takeover of ambiguous leases
	JSONLogs          bool   // NDJSON event log alongside session.log
	JSONProgress      bool   // NDJSON progress snapshots (1 Hz)
	MsgRetries        int
	BatchBytes        int64
	FetchWindow       int
	AppendWindow      int
	TLSVerify         bool
	RefreshMS         int
	RunID             string
}

// Defaults returns a Run with the Python engine's defaults.
func Defaults() *Run {
	return &Run{
		Workers: 10, LogsDir: "./logs", DBPath: "./migration.db",
		Retries: 2, RetryDelay: 30, Order: "csv",
		MaxConnsPerBox: 3, PerHostConns: 8, Timeout: 120,
		StaleTimeout: 300, RecoveryRetries: 3, RecoveryInterval: 30,
		LockTimeout: 300, WorkerTimeout: 60,
		BatchAttempts: 3, ReconnectAttempts: 5,
		IsolateFailed: true, SkipKnownFailed: true,
		LogKeepDays: 30, DBHeartbeat: 15,
		MsgRetries: 3, BatchBytes: 8 << 20, FetchWindow: 8, AppendWindow: 8,
		TLSVerify: true, RefreshMS: 250, Compress: "auto",
		RunID: time.Now().Format("20060102-150405"),
	}
}

// ParseCSV reads the wrapper-compatible mailbox CSV.
func ParseCSV(path string) ([]MailboxSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("CSV parse error: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("CSV is empty")
	}
	head := map[string]int{}
	for i, h := range rows[0] {
		head[strings.ToLower(strings.TrimSpace(h))] = i
	}
	need := []string{"oldhost", "oldport", "oldsecurity", "olduser", "oldpassword",
		"newhost", "newport", "newsecurity", "newuser", "newpassword"}
	for _, n := range need {
		if _, ok := head[n]; !ok {
			return nil, fmt.Errorf("CSV missing required column %q", n)
		}
	}
	get := func(row []string, key string) string {
		i := head[key]
		if i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}
	port := func(row []string, key string, sec string) int {
		p, err := strconv.Atoi(get(row, key))
		if err == nil && p > 0 {
			return p
		}
		if sec == "ssl" {
			return 993
		}
		return 143
	}
	sec := func(s string) (string, error) {
		s = strings.ToLower(s)
		switch s {
		case "ssl", "tls", "none":
			return s, nil
		case "starttls":
			return "tls", nil
		case "":
			return "ssl", nil
		}
		return "", fmt.Errorf("security must be ssl, tls or none (got %q)", s)
	}
	var out []MailboxSpec
	for n, row := range rows[1:] {
		if len(row) == 0 || (len(row) == 1 && strings.TrimSpace(row[0]) == "") {
			continue
		}
		ssec, err := sec(get(row, "oldsecurity"))
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", n+2, err)
		}
		dsec, err := sec(get(row, "newsecurity"))
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", n+2, err)
		}
		spec := MailboxSpec{
			Index: len(out) + 1,
			Src: Endpoint{Host: get(row, "oldhost"), Port: port(row, "oldport", ssec),
				Security: ssec, User: get(row, "olduser"), Password: get(row, "oldpassword")},
			Dst: Endpoint{Host: get(row, "newhost"), Port: port(row, "newport", dsec),
				Security: dsec, User: get(row, "newuser"), Password: get(row, "newpassword")},
		}
		if spec.Src.Host == "" || spec.Src.User == "" || spec.Dst.Host == "" || spec.Dst.User == "" {
			return nil, fmt.Errorf("row %d: host/user columns must not be empty", n+2)
		}
		out = append(out, spec)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("CSV has a header but no mailbox rows")
	}
	return out, nil
}

// FolderMap loads an explicit "Source = Destination" mapping file.
func (r *Run) FolderMap() map[string]string {
	out := map[string]string{}
	if r.MapFile == "" {
		return out
	}
	data, err := os.ReadFile(r.MapFile)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}
	return out
}

// CSVTemplate is written by `mailferry init`.
const CSVTemplate = `oldhost,oldport,oldsecurity,olduser,oldpassword,newhost,newport,newsecurity,newuser,newpassword
imap.example.com,993,ssl,jane@example.com,SourcePassword,imap.example.org,993,ssl,jane@example.org,DestinationPassword
`
