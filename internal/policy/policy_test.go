// SPDX-FileCopyrightText: Copyright (c) 2026, the k-cache developers
//
// SPDX-License-Identifier: Apache-2.0

package policy_test

import (
	"testing"
	"time"

	"kache/internal/policy"
)

// match is a helper that calls Match with no namespace/pod-label scoping,
// matching how tests exercise host/port/method rules in isolation.
func match(p *policy.Policy, host string, port uint16, method string) *policy.Rule {
	return p.Match("", nil, host, port, method, "/")
}

func rules() []policy.Rule {
	return []policy.Rule{
		{Host: "api.example.com", Port: 80, TTL: time.Minute},
		{Host: "*", Port: 9090, TTL: 30 * time.Second},
	}
}

func TestExactHostMatch(t *testing.T) {
	p := policy.New(rules())
	r := match(p, "api.example.com", 80, "GET")
	if r == nil {
		t.Fatal("expected match")
	}
	if r.TTL != time.Minute {
		t.Fatalf("unexpected TTL: %v", r.TTL)
	}
}

func TestWildcardMatch(t *testing.T) {
	p := policy.New(rules())
	if r := match(p, "anything.internal", 9090, "GET"); r == nil {
		t.Fatal("expected wildcard match on port 9090")
	}
}

func TestNoMatch(t *testing.T) {
	p := policy.New(rules())
	if r := match(p, "other.com", 80, "GET"); r != nil {
		t.Fatal("expected no match for other.com:80")
	}
}

func TestPortMismatch(t *testing.T) {
	p := policy.New(rules())
	if r := match(p, "api.example.com", 443, "GET"); r != nil {
		t.Fatal("expected no match on wrong port")
	}
}

func TestHostWithPort(t *testing.T) {
	p := policy.New(rules())
	if r := match(p, "api.example.com:80", 80, "GET"); r == nil {
		t.Fatal("expected match when host includes port")
	}
}

func TestZeroPortMatchesAny(t *testing.T) {
	p := policy.New([]policy.Rule{{Host: "flex.com", Port: 0, TTL: time.Minute}})
	if r := match(p, "flex.com", 1234, "GET"); r == nil {
		t.Fatal("expected Port=0 rule to match any port")
	}
}

func TestFirstMatchWins(t *testing.T) {
	p := policy.New([]policy.Rule{
		{Host: "*", Port: 80, TTL: time.Minute},
		{Host: "api.example.com", Port: 80, TTL: 30 * time.Second},
	})
	r := match(p, "api.example.com", 80, "GET")
	if r == nil {
		t.Fatal("expected a match")
	}
	if r.TTL != time.Minute {
		t.Fatalf("first rule (TTL=1m) should win, got TTL=%v", r.TTL)
	}
}

func TestEmptyPolicy(t *testing.T) {
	p := policy.New(nil)
	if r := match(p, "anything.com", 80, "GET"); r != nil {
		t.Fatal("empty policy should always return nil")
	}
}

func TestMethodFilter(t *testing.T) {
	p := policy.New([]policy.Rule{
		{Host: "*", Port: 80, Methods: []string{"GET"}, TTL: time.Minute},
	})
	if r := match(p, "example.com", 80, "GET"); r == nil {
		t.Fatal("expected GET to match")
	}
	if r := match(p, "example.com", 80, "POST"); r != nil {
		t.Fatal("expected POST to be excluded by method filter")
	}
}

func TestEmptyMethodsMatchesAll(t *testing.T) {
	p := policy.New([]policy.Rule{
		{Host: "*", Port: 80, Methods: nil, TTL: time.Minute},
	})
	for _, m := range []string{"GET", "POST", "HEAD"} {
		if r := match(p, "example.com", 80, m); r == nil {
			t.Fatalf("expected %s to match when Methods is empty", m)
		}
	}
}

func TestPathFilter(t *testing.T) {
	p := policy.New([]policy.Rule{
		{Host: "*", Port: 80, Paths: []string{"/api/"}, TTL: time.Minute},
	})
	if r := p.Match("", nil, "svc", 80, "GET", "/api/products"); r == nil {
		t.Fatal("expected /api/products to match prefix /api/")
	}
	if r := p.Match("", nil, "svc", 80, "GET", "/static/app.js"); r != nil {
		t.Fatal("expected /static/app.js to not match prefix /api/")
	}
}

func TestNamespaceScoping(t *testing.T) {
	p := policy.New([]policy.Rule{
		{Namespace: "team-a", Host: "*", Port: 80, TTL: time.Minute},
		{Namespace: "team-b", Host: "*", Port: 80, TTL: 30 * time.Second},
	})
	r := p.Match("team-a", nil, "svc", 80, "GET", "/")
	if r == nil || r.TTL != time.Minute {
		t.Fatal("expected team-a rule to match")
	}
	r = p.Match("team-b", nil, "svc", 80, "GET", "/")
	if r == nil || r.TTL != 30*time.Second {
		t.Fatal("expected team-b rule to match")
	}
	if r := p.Match("team-c", nil, "svc", 80, "GET", "/"); r != nil {
		t.Fatal("expected no match for unknown namespace")
	}
}

func TestKeyConfig(t *testing.T) {
	r := policy.Rule{
		CacheBody:   true,
		VaryHeaders: []string{"Accept-Language"},
	}
	cfg := r.KeyConfig()
	if !cfg.IncludeBody {
		t.Fatal("CacheBody=true should map to IncludeBody=true in KeyConfig")
	}
	if len(cfg.VaryHeaders) != 1 || cfg.VaryHeaders[0] != "Accept-Language" {
		t.Fatalf("VaryHeaders not propagated correctly: %v", cfg.VaryHeaders)
	}
}
