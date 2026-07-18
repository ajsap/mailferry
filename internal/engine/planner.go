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

package engine

// Folder enumeration, namespace/delimiter translation, special-use role
// mapping (localisation-proof), include/exclude filters, explicit mapping.

import (
	"path/filepath"
	"strings"

	"github.com/ajsap/mailferry/v2/internal/config"
	"github.com/ajsap/mailferry/v2/internal/imapx"
)

type FolderPlan struct {
	SrcDisplay string
	SrcWire    string
	DstDisplay string
	DstWire    string
	Attrs      []string
	EstMsgs    int64
	UIDNext    int64
}

var specialUse = map[string]bool{
	"\\SENT": true, "\\DRAFTS": true, "\\TRASH": true,
	"\\JUNK": true, "\\ARCHIVE": true, "\\FLAGGED": true,
}
var gmailVirtual = map[string]bool{"\\ALL": true, "\\IMPORTANT": true}
var nameRoles = map[string]string{
	"sent": "\\SENT", "sent items": "\\SENT", "sent messages": "\\SENT",
	"drafts": "\\DRAFTS", "trash": "\\TRASH", "deleted items": "\\TRASH",
	"junk": "\\JUNK", "spam": "\\JUNK", "junk e-mail": "\\JUNK", "archive": "\\ARCHIVE",
}

func roleOf(attrs []string, leaf string) string {
	for _, a := range attrs {
		up := strings.ToUpper(a)
		if specialUse[up] {
			return up
		}
	}
	return nameRoles[strings.ToLower(leaf)]
}

func matchAny(pats []string, name string) bool {
	for _, p := range pats {
		if ok, _ := filepath.Match(p, name); ok {
			return true
		}
	}
	return false
}

// BuildPlan enumerates source folders and maps each to a destination name.
func BuildPlan(src, dst *imapx.Client, cfg *config.Run, mapping map[string]string,
	logf func(string)) ([]FolderPlan, error) {
	srcEntries, err := src.ListAll()
	if err != nil {
		return nil, err
	}
	dstEntries, err := dst.ListAll()
	if err != nil {
		return nil, err
	}
	srcPfxWire, srcNSDelim := src.NamespaceInfo()
	dstPfxWire, dstNSDelim := dst.NamespaceInfo()

	entryDelim := func(entries []imapx.ListEntry) string {
		for _, e := range entries {
			if e.Delim != "" {
				return e.Delim
			}
		}
		return ""
	}
	srcDelim := srcNSDelim
	if srcDelim == "" {
		srcDelim = entryDelim(srcEntries)
	}
	if srcDelim == "" {
		srcDelim = "/"
	}
	dstDelim := dstNSDelim
	if dstDelim == "" {
		dstDelim = entryDelim(dstEntries)
	}
	if dstDelim == "" {
		dstDelim = "/"
	}
	srcPfx := imapx.DecodeMUTF7(srcPfxWire)
	dstPfx := imapx.DecodeMUTF7(dstPfxWire)
	if strings.EqualFold(strings.TrimSuffix(srcPfx, srcDelim), "inbox") && srcPfx != "" {
		if !strings.HasSuffix(srcPfx, srcDelim) {
			srcPfx += srcDelim
		}
	}
	if dstPfx != "" && !strings.HasSuffix(dstPfx, dstDelim) {
		dstPfx += dstDelim
	}

	// destination special-use role map
	dstRoles := map[string]string{}
	for _, e := range dstEntries {
		disp := imapx.DecodeMUTF7(e.Wire)
		leaf := disp
		if dstDelim != "" {
			parts := strings.Split(disp, dstDelim)
			leaf = parts[len(parts)-1]
		}
		if role := roleOf(e.Attrs, leaf); role != "" {
			if _, seen := dstRoles[role]; !seen {
				dstRoles[role] = e.Wire
			}
		}
	}

	var plans []FolderPlan
	for _, e := range srcEntries {
		up := make([]string, len(e.Attrs))
		noselect := false
		virtual := false
		for i, a := range e.Attrs {
			up[i] = strings.ToUpper(a)
			if up[i] == "\\NOSELECT" || up[i] == "\\NONEXISTENT" {
				noselect = true
			}
			if gmailVirtual[up[i]] {
				virtual = true
			}
		}
		if noselect {
			continue
		}
		display := imapx.DecodeMUTF7(e.Wire)
		delim := e.Delim
		if delim == "" {
			delim = srcDelim
		}
		parts := strings.Split(display, delim)
		leaf := parts[len(parts)-1]
		if virtual && !cfg.GmailAllMail {
			logf("plan: skipping Gmail virtual folder " + display +
				" (use --gmail-all-mail to include)")
			continue
		}
		if len(cfg.Include) > 0 && !matchAny(cfg.Include, display) {
			continue
		}
		if matchAny(cfg.Exclude, display) {
			logf("plan: excluded " + display)
			continue
		}

		var dstDisplay string
		if strings.EqualFold(display, "INBOX") {
			dstDisplay = "INBOX"
		} else if m, ok := mapping[display]; ok {
			dstDisplay = m
		} else if role := roleOf(up, leaf); role != "" && dstRoles[role] != "" {
			dstDisplay = imapx.DecodeMUTF7(dstRoles[role])
		} else {
			body := display
			if srcPfx != "" && strings.HasPrefix(body, srcPfx) {
				body = body[len(srcPfx):]
			}
			var segs []string
			for _, s := range strings.Split(body, delim) {
				if s != "" {
					segs = append(segs, s)
				}
			}
			joined := strings.Join(segs, dstDelim)
			if strings.EqualFold(joined, "INBOX") {
				dstDisplay = "INBOX"
			} else if dstPfx != "" {
				dstDisplay = dstPfx + joined
			} else {
				dstDisplay = joined
			}
		}
		plans = append(plans, FolderPlan{
			SrcDisplay: display, SrcWire: e.Wire,
			DstDisplay: dstDisplay, DstWire: imapx.EncodeMUTF7(dstDisplay),
			Attrs: up,
		})
	}

	// message-count estimates (drives ordering and progress totals)
	for i := range plans {
		st, err := src.Status(plans[i].SrcWire)
		if err == nil {
			plans[i].EstMsgs = st["MESSAGES"]
			plans[i].UIDNext = st["UIDNEXT"]
		}
	}

	// INBOX first, then largest first (finish long poles early)
	for i := 1; i < len(plans); i++ {
		for j := i; j > 0 && planLess(plans[j], plans[j-1]); j-- {
			plans[j], plans[j-1] = plans[j-1], plans[j]
		}
	}
	// dedupe destination-name collisions
	seen := map[string]string{}
	for i := range plans {
		low := strings.ToLower(plans[i].DstDisplay)
		if prev, ok := seen[low]; ok && prev != plans[i].SrcDisplay {
			plans[i].DstDisplay += "-mf"
			plans[i].DstWire = imapx.EncodeMUTF7(plans[i].DstDisplay)
		}
		seen[strings.ToLower(plans[i].DstDisplay)] = plans[i].SrcDisplay
	}
	return plans, nil
}

func planLess(a, b FolderPlan) bool {
	ai, bi := 1, 1
	if strings.EqualFold(a.SrcDisplay, "INBOX") {
		ai = 0
	}
	if strings.EqualFold(b.SrcDisplay, "INBOX") {
		bi = 0
	}
	if ai != bi {
		return ai < bi
	}
	return a.EstMsgs > b.EstMsgs
}
