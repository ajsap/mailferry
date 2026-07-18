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
