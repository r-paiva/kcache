// SPDX-FileCopyrightText: 2026 Rui Paiva <kcache.catapult615@passfwd.com>
//
// SPDX-License-Identifier: Apache-2.0

package cachekey_test

import (
	"net/http"
	"testing"

	"kache/internal/cachekey"
)

func req(method, url string) *http.Request {
	r, _ := http.NewRequest(method, url, nil)
	return r
}

func TestSameRequestSameKey(t *testing.T) {
	cfg := cachekey.Config{}
	k1 := cachekey.Generate(req("GET", "http://example.com/foo"), nil, cfg)
	k2 := cachekey.Generate(req("GET", "http://example.com/foo"), nil, cfg)
	if k1 != k2 {
		t.Fatal("same request produced different keys")
	}
}

func TestDifferentMethodDifferentKey(t *testing.T) {
	cfg := cachekey.Config{}
	k1 := cachekey.Generate(req("GET", "http://example.com/"), nil, cfg)
	k2 := cachekey.Generate(req("POST", "http://example.com/"), nil, cfg)
	if k1 == k2 {
		t.Fatal("GET and POST should produce different keys")
	}
}

func TestDifferentPathDifferentKey(t *testing.T) {
	cfg := cachekey.Config{}
	k1 := cachekey.Generate(req("GET", "http://example.com/a"), nil, cfg)
	k2 := cachekey.Generate(req("GET", "http://example.com/b"), nil, cfg)
	if k1 == k2 {
		t.Fatal("different paths should produce different keys")
	}
}

func TestBodyIncluded(t *testing.T) {
	cfg := cachekey.Config{IncludeBody: true}
	r := req("POST", "http://example.com/")
	k1 := cachekey.Generate(r, []byte(`{"x":1}`), cfg)
	k2 := cachekey.Generate(r, []byte(`{"x":2}`), cfg)
	if k1 == k2 {
		t.Fatal("different bodies should produce different keys when IncludeBody=true")
	}
}

func TestBodyIgnoredWhenNotConfigured(t *testing.T) {
	cfg := cachekey.Config{IncludeBody: false}
	r := req("POST", "http://example.com/")
	k1 := cachekey.Generate(r, []byte(`{"x":1}`), cfg)
	k2 := cachekey.Generate(r, []byte(`{"x":2}`), cfg)
	if k1 != k2 {
		t.Fatal("bodies should be ignored when IncludeBody=false")
	}
}

func TestVaryHeaders(t *testing.T) {
	cfg := cachekey.Config{VaryHeaders: []string{"Authorization"}}
	r1 := req("GET", "http://example.com/")
	r1.Header.Set("Authorization", "Bearer token-a")
	r2 := req("GET", "http://example.com/")
	r2.Header.Set("Authorization", "Bearer token-b")

	if cachekey.Generate(r1, nil, cfg) == cachekey.Generate(r2, nil, cfg) {
		t.Fatal("different Authorization headers should produce different keys")
	}
}

func TestVaryHeaderOrderIndependent(t *testing.T) {
	cfg := cachekey.Config{VaryHeaders: []string{"X-A", "X-B"}}
	r := req("GET", "http://example.com/")
	r.Header.Set("X-A", "1")
	r.Header.Set("X-B", "2")
	k1 := cachekey.Generate(r, nil, cfg)

	cfg2 := cachekey.Config{VaryHeaders: []string{"X-B", "X-A"}}
	k2 := cachekey.Generate(r, nil, cfg2)
	if k1 != k2 {
		t.Fatal("vary header order should not affect the key")
	}
}

func TestQueryStringDifferentKey(t *testing.T) {
	cfg := cachekey.Config{}
	k1 := cachekey.Generate(req("GET", "http://example.com/search?q=1"), nil, cfg)
	k2 := cachekey.Generate(req("GET", "http://example.com/search?q=2"), nil, cfg)
	if k1 == k2 {
		t.Fatal("different query strings should produce different keys")
	}
}

func TestHostDifferentKey(t *testing.T) {
	cfg := cachekey.Config{}
	k1 := cachekey.Generate(req("GET", "http://service-a.internal/api"), nil, cfg)
	k2 := cachekey.Generate(req("GET", "http://service-b.internal/api"), nil, cfg)
	if k1 == k2 {
		t.Fatal("different hosts should produce different cache keys")
	}
}

func TestVaryHeaderAbsentSameKey(t *testing.T) {
	// Two requests that both lack the vary header must share the same cache key.
	cfg := cachekey.Config{VaryHeaders: []string{"Authorization"}}
	r1 := req("GET", "http://example.com/")
	r2 := req("GET", "http://example.com/")
	if cachekey.Generate(r1, nil, cfg) != cachekey.Generate(r2, nil, cfg) {
		t.Fatal("absent vary header should not break key equality between identical requests")
	}
}
