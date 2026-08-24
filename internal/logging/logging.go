// SPDX-FileCopyrightText: 2026 The latch Contributors
//
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"log/slog"
	"os"
	"strings"
)

func New(logLevel string) {
	level := parseLogLevel(logLevel)
	opts := &slog.HandlerOptions{Level: level}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
