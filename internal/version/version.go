// SPDX-FileCopyrightText: Copyright (c) 2026, the kcache developers
//
// SPDX-License-Identifier: Apache-2.0

package version

// Version is set at build time via -ldflags "-X kache/internal/version.Version=<ver>".
// Falls back to "dev" for local builds without the flag.
var Version = "dev"
