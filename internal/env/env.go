// SPDX-FileCopyrightText: 2026 The latch Contributors
//
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"log/slog"
	"os"

	"codeberg.org/latch/latch/internal/constants"
	"github.com/caarlos0/env/v11"
)

type EnvConfig struct {
	LogLevel      string `env:"LOG_LEVEL" envDefault:"info"`
	ProxyAddr     string `env:"PROXY_ADDR" envDefault:"0.0.0.0:8080"`
	MetricsAddr   string `env:"METRICS_ADDR" envDefault:"0.0.0.0:9090"`
	MaxCacheBytes int64  `env:"MAX_CACHE_BYTES" envDefault:"268435456"` // 256<<20
	MaxBodyBytes  int64  `env:"MAX_BODY_BYTES" envDefault:"1048576"`    // 1<<20
	TlsCaDir      string `env:"TLS_CA_DIR" envDefault:""`
	ProcRoot      string `env:"PROC_ROOT" envDefault:"/proc"`
}

func New() EnvConfig {
	var cfg EnvConfig

	err := env.Parse(&cfg)
	if err != nil {
		slog.Error("load env config", "err", err)
		os.Exit(constants.ExitInitEnvConfigError)
	}

	return cfg
}
