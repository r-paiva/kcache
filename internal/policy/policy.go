// SPDX-FileCopyrightText: Copyright (c) 2026, the k-cache developers
//
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"net"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"

	"kache/internal/cachekey"
)

// Rule defines caching behaviour for requests originating from a matched pod.
type Rule struct {
	// Namespace and PodSelector scope this rule to specific pods.
	// An empty/nil PodSelector matches all pods in the namespace.
	// An empty Namespace matches any namespace (used for the fallback default).
	Namespace   string
	PodSelector labels.Selector

	Host    string        // exact hostname or "*" for any
	Port    uint16        // destination port; 0 means any
	Methods []string      // HTTP methods to cache; empty means all
	Paths   []string      // URL path prefixes to cache; empty means all
	TTL     time.Duration

	CacheBody    bool     // include request body in cache key
	VaryHeaders  []string // header names that partition the cache
	MaxBodyBytes int64    // per-rule body size limit; 0 inherits daemon default
}

func (r *Rule) KeyConfig() cachekey.Config {
	return cachekey.Config{
		IncludeBody: r.CacheBody,
		VaryHeaders: r.VaryHeaders,
	}
}

// Policy holds an ordered list of caching rules.
type Policy struct {
	rules []Rule
}

func New(rules []Rule) *Policy {
	return &Policy{rules: rules}
}

// Match returns the first rule that matches the request context, or nil.
// namespace and podLabels identify the source pod; host, port, method, path
// describe the outbound request.
func (p *Policy) Match(namespace string, podLabels map[string]string, host string, port uint16, method, path string) *Rule {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	podSet := labels.Set(podLabels)

	for i := range p.rules {
		r := &p.rules[i]

		// Namespace must match if set.
		if r.Namespace != "" && r.Namespace != namespace {
			continue
		}
		// Pod selector must match if set.
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
		return r
	}
	return nil
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
