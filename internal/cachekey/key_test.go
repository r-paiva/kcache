// SPDX-FileCopyrightText: Copyright (c) 2026, the latch developers
//
// SPDX-License-Identifier: Apache-2.0

package cachekey_test

import (
	"net/http"
	"testing"

	"codeberg.org/latch/latch/internal/cachekey"
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
	cfg := cachekey.Config{VaryHeaders: []string{"Authorization"}}
	r1 := req("GET", "http://example.com/")
	r2 := req("GET", "http://example.com/")
	if cachekey.Generate(r1, nil, cfg) != cachekey.Generate(r2, nil, cfg) {
		t.Fatal("absent vary header should not break key equality between identical requests")
	}
}

func TestVaryHeaderPresentVsAbsentDiffers(t *testing.T) {
	cfg := cachekey.Config{VaryHeaders: []string{"Authorization"}}
	with := req("GET", "http://example.com/")
	with.Header.Set("Authorization", "Bearer token")
	without := req("GET", "http://example.com/")
	if cachekey.Generate(with, nil, cfg) == cachekey.Generate(without, nil, cfg) {
		t.Fatal("request with vary header should differ from one without it")
	}
}

func TestReqHostFieldOverridesURLHost(t *testing.T) {
	cfg := cachekey.Config{}
	r := req("GET", "http://url-host.example.com/path")
	r.Host = "override-host.example.com"
	k1 := cachekey.Generate(r, nil, cfg)
	k2 := cachekey.Generate(req("GET", "http://override-host.example.com/path"), nil, cfg)
	if k1 != k2 {
		t.Fatal("req.Host should take precedence over req.URL.Host")
	}
}

func TestNilAndEmptyBodyEquivalentWithIncludeBody(t *testing.T) {
	cfg := cachekey.Config{IncludeBody: true}
	r := req("POST", "http://example.com/")
	if cachekey.Generate(r, nil, cfg) != cachekey.Generate(r, []byte{}, cfg) {
		t.Fatal("nil and empty body should produce the same key")
	}
}

func TestPortDifferentKey(t *testing.T) {
	r := req("GET", "http://example.com/api")
	k80 := cachekey.Generate(r, nil, cachekey.Config{Port: 80})
	k443 := cachekey.Generate(r, nil, cachekey.Config{Port: 443})
	if k80 == k443 {
		t.Fatal("same host+path on different ports should produce different cache keys")
	}
}

func TestKeyIsHexSHA256(t *testing.T) {
	k := cachekey.Generate(req("GET", "http://example.com/"), nil, cachekey.Config{})
	if len(k) != 64 {
		t.Fatalf("expected 64-char hex key, got len %d", len(k))
	}
	for _, c := range k {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("key contains non-hex character: %c", c)
		}
	}
}
