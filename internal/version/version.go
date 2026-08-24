// SPDX-FileCopyrightText: 2026 The latch Contributors
//
// SPDX-License-Identifier: Apache-2.0

package version

// Version is set at build time via -ldflags "-X codeberg.org/latch/latch/internal/version.Version=<ver>".
// Falls back to "dev" for local builds without the flag.
var Version = "dev"
