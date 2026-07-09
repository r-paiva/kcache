// SPDX-FileCopyrightText: Copyright (c) 2026, the k-cache developers
//
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"log/slog"
	"net"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"

	"kache/internal/cachekey"
)

type Rule struct {
	Namespace   string
	PodSelector labels.Selector

	Host    string
	Port    uint16
	Methods []string
	Paths   []string
	TTL     time.Duration

	CacheBody    bool
	VaryHeaders  []string
	MaxBodyBytes int64
}

func (r *Rule) KeyConfig() cachekey.Config {
	return cachekey.Config{
		IncludeBody: r.CacheBody,
		VaryHeaders: r.VaryHeaders,
	}
}

type Policy struct {
	rules []Rule
}

func New(rules []Rule) *Policy {
	return &Policy{rules: rules}
}

func (p *Policy) Match(namespace string, podLabels map[string]string, host string, port uint16, method, path string) *Rule {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	podSet := labels.Set(podLabels)

	for i := range p.rules {
		r := &p.rules[i]

		if r.Namespace != "" && r.Namespace != namespace {
			continue
		}
		if r.PodSelector != nil && !r.PodSelector.Empty() && !r.PodSelector.Matches(podSet) {
			continue
		}
		if r.Port != 0 && r.Port != port {
			continue
		}
		if r.Host != "*" && r.Host != host {
			continue
		}
		if len(r.Methods) > 0 && !methodAllowed(r.Methods, method) {
			continue
		}
		if len(r.Paths) > 0 && !pathAllowed(r.Paths, path) {
			continue
		}
		slog.Debug("policy match", "host", host, "port", port, "method", method, "path", path,
			"rule_host", r.Host, "ttl", r.TTL, "namespace", r.Namespace)
		return r
	}
	return nil
}

// HasRuleForHostPort reports whether any rule targets host on port, ignoring
// method, path, namespace and pod selector. Used as a pre-MITM gate before
// the HTTP request headers are available.
func (p *Policy) HasRuleForHostPort(host string, port uint16) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	for i := range p.rules {
		r := &p.rules[i]
		if r.Port != 0 && r.Port != port {
			continue
		}
		if r.Host != "*" && r.Host != host {
			continue
		}
		return true
	}
	return false
}

func methodAllowed(allowed []string, method string) bool {
	for _, m := range allowed {
		if m == method {
			return true
		}
	}
	return false
}

func pathAllowed(prefixes []string, path string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}
