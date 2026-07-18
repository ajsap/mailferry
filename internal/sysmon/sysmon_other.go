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

//go:build !linux && !darwin

package sysmon

import "runtime"

// otherSampler covers Windows and any other platform without a dedicated
// implementation. There is no portable, subprocess-free way to read
// system-wide CPU%, load average, or total/used memory here, so only a
// coarse process memory figure is reported; everything else stays at its
// zero value (HasCPU/HasLoad false, MemTotal/MemUsed 0).
type otherSampler struct{}

func newSampler() sampler {
	return &otherSampler{}
}

func (otherSampler) Sample() Snap {
	var snap Snap

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	snap.RSS = int64(ms.Sys)

	return snap
}
