// SPDX-FileCopyrightText: 2026 The latch Contributors
//
// SPDX-License-Identifier: Apache-2.0

package cachekey

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"sort"
)

type Config struct {
	IncludeBody bool
	VaryHeaders []string
	Port        uint16
}

func Generate(req *http.Request, body []byte, cfg Config) string {
	h := sha256.New()

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	_, _ = fmt.Fprintf(h, "host=%s\nmethod=%s\npath=%s\nport=%d\n", host, req.Method, req.URL.RequestURI(), cfg.Port)

	if cfg.IncludeBody && len(body) > 0 {
		h.Write(body)
		h.Write([]byte{'\n'})
	}

	if len(cfg.VaryHeaders) > 0 {
		pairs := make([]string, 0, len(cfg.VaryHeaders))
		for _, name := range cfg.VaryHeaders {
			pairs = append(pairs, fmt.Sprintf("%s=%s", name, req.Header.Get(name)))
		}
		sort.Strings(pairs)
		for _, p := range pairs {
			_, _ = fmt.Fprintf(h, "%s\n", p)
		}
	}

	return fmt.Sprintf("%x", h.Sum(nil))
}
