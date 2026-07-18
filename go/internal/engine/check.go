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

// Preflight (`mailferry check`): connect, authenticate, list, estimate —
// nothing is ever written.
package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/imapx"
)

// PreflightResult summarises one CSV row's readiness.
type PreflightResult struct {
	Folders int
	Msgs    int64
	Bytes   int64
	SrcCaps string
	DstCaps string
	Err     error
}

// Preflight connects both endpoints of one spec, authenticates, builds the
// folder plan and estimates volume. Read-only by construction: only LIST /
// STATUS / NAMESPACE are issued.
func Preflight(cfg *config.Run, spec config.MailboxSpec) PreflightResult {
	var out PreflightResult
	to := time.Duration(cfg.Timeout * float64(time.Second))
	src := imapx.NewClient(imapx.Endpoint(spec.Src), to, cfg.TLSVerify, nil,
		spec.Label(), nil)
	dst := imapx.NewClient(imapx.Endpoint(spec.Dst), to, cfg.TLSVerify, nil,
		spec.Label(), nil)
	defer func() {
		for _, c := range []*imapx.Client{src, dst} {
			if c != nil && c.Alive() {
				c.Logout(5 * time.Second)
			}
		}
	}()
	step := func(c *imapx.Client) error {
		if err := c.Connect(); err != nil {
			return err
		}
		return c.Login()
	}
	if err := step(src); err != nil {
		out.Err = fmt.Errorf("source: %w", err)
		return out
	}
	if err := step(dst); err != nil {
		out.Err = fmt.Errorf("destination: %w", err)
		return out
	}
	plans, err := BuildPlan(src, dst, cfg, cfg.FolderMap(), func(string) {})
	if err != nil {
		out.Err = err
		return out
	}
	out.Folders = len(plans)
	for _, p := range plans {
		out.Msgs += p.EstMsgs
		if st, err := src.Status(p.SrcWire); err == nil {
			if sz, ok := st["SIZE"]; ok {
				out.Bytes += sz
			}
		}
	}
	capsOf := func(c *imapx.Client, role string) string {
		var bits []string
		if c.Has("COMPRESS=DEFLATE") && role == "src" {
			bits = append(bits, "COMPRESS")
		}
		if c.Has("UIDPLUS") {
			bits = append(bits, "UIDPLUS")
		}
		if c.Has("LITERAL+") {
			bits = append(bits, "LIT+")
		}
		return role + "[" + strings.Join(bits, " ") + "]"
	}
	out.SrcCaps = capsOf(src, "src")
	out.DstCaps = capsOf(dst, "dst")
	return out
}
