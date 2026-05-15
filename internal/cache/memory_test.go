// SPDX-FileCopyrightText: 2026 Rui Paiva <kcache.catapult615@passfwd.com>
//
// SPDX-License-Identifier: Apache-2.0

package cache_test

import (
	"net/http"
	"testing"
	"time"

	"kache/internal/cache"
)

func entry(body string, ttl time.Duration) *cache.Entry {
	return &cache.Entry{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       []byte(body),
		CachedAt:   time.Now(),
		TTL:        ttl,
	}
}

func entryWithHeaders(body string, ttl time.Duration, headers http.Header) *cache.Entry {
	return &cache.Entry{
		StatusCode: 200,
		Header:     headers,
		Body:       []byte(body),
		CachedAt:   time.Now(),
		TTL:        ttl,
	}
}

func TestGetMiss(t *testing.T) {
	c := cache.New(0, nil)
	if _, ok := c.Get("missing"); ok {
		t.Fatal("expected miss")
	}
}

func TestSetAndGet(t *testing.T) {
	c := cache.New(0, nil)
	c.Set("k", entry("hello", time.Minute))
	e, ok := c.Get("k")
	if !ok {
		t.Fatal("expected hit")
	}
	if string(e.Body) != "hello" {
		t.Fatalf("unexpected body: %q", e.Body)
	}
}

func TestExpiredEntryTreatedAsMiss(t *testing.T) {
	c := cache.New(0, nil)
	e := entry("data", time.Millisecond)
	e.CachedAt = time.Now().Add(-time.Second) // already expired
	c.Set("k", e)
	if _, ok := c.Get("k"); ok {
		t.Fatal("expected expired entry to be a miss")
	}
}

func TestDelete(t *testing.T) {
	c := cache.New(0, nil)
	c.Set("k", entry("v", time.Minute))
	c.Delete("k")
	if _, ok := c.Get("k"); ok {
		t.Fatal("expected miss after delete")
	}
}

func TestSizeBytes(t *testing.T) {
	c := cache.New(0, nil)
	c.Set("a", entry("hello", time.Minute))  // 5 bytes body, no headers
	c.Set("b", entry("world!", time.Minute)) // 6 bytes body, no headers
	if got := c.SizeBytes(); got != 11 {
		t.Fatalf("expected 11 bytes, got %d", got)
	}
}

func TestSizeBytes_IncludesHeaders(t *testing.T) {
	c := cache.New(0, nil)
	// body: "hi" = 2 bytes
	// header key:   "Content-Type" = 12 bytes
	// header value: "text/plain"   = 10 bytes
	// total: 24 bytes
	h := http.Header{"Content-Type": {"text/plain"}}
	c.Set("k", entryWithHeaders("hi", time.Minute, h))
	if got := c.SizeBytes(); got != 24 {
		t.Fatalf("expected 24 bytes (body + headers), got %d", got)
	}
}

func TestOverwriteUpdatesSize(t *testing.T) {
	c := cache.New(0, nil)
	c.Set("k", entry("hi", time.Minute))    // 2 bytes
	c.Set("k", entry("hello", time.Minute)) // 5 bytes
	if got := c.SizeBytes(); got != 5 {
		t.Fatalf("expected 5 bytes after overwrite, got %d", got)
	}
}

func TestEvictionWhenFull(t *testing.T) {
	// Each body is 1 byte; cap at 2 bytes so the third entry forces an eviction.
	evicted := 0
	c := cache.New(2, func(n int) { evicted += n })

	c.Set("a", entry("x", time.Minute)) // 1 byte → total 1
	c.Set("b", entry("y", time.Minute)) // 1 byte → total 2, at cap
	c.Set("c", entry("z", time.Minute)) // 1 byte → exceeds cap, evicts one

	if c.Len() > 2 {
		t.Fatalf("cache grew beyond byte cap: len=%d", c.Len())
	}
	if evicted == 0 {
		t.Fatal("expected at least one eviction")
	}
}

func TestByteCap(t *testing.T) {
	evicted := 0
	c := cache.New(10, func(n int) { evicted += n }) // 10-byte cap

	c.Set("a", entry("12345", time.Minute)) // 5 bytes
	c.Set("b", entry("67890", time.Minute)) // 5 bytes → total 10, at cap

	// Adding a third entry must evict something to stay within 10 bytes.
	c.Set("c", entry("abcde", time.Minute))
	if got := c.SizeBytes(); got > 10 {
		t.Fatalf("cache exceeded byte cap: %d bytes", got)
	}
	if evicted == 0 {
		t.Fatal("expected at least one eviction to enforce byte cap")
	}
}

func TestByteCap_EntryLargerThanBudget(t *testing.T) {
	c := cache.New(3, nil)                  // only 3 bytes total
	c.Set("a", entry("12345", time.Minute)) // 5 bytes — exceeds cap; must be dropped
	if c.Len() != 0 {
		t.Fatalf("oversized entry should not be stored, got len=%d", c.Len())
	}
}

func TestSizeBytesAfterLazyDelete(t *testing.T) {
	c := cache.New(0, nil)
	e := entry("hello", time.Millisecond)
	e.CachedAt = time.Now().Add(-time.Second) // already expired
	c.Set("k", e)

	// Get triggers an inline delete of the expired entry.
	if _, ok := c.Get("k"); ok {
		t.Fatal("expected miss for expired entry")
	}
	if got := c.SizeBytes(); got != 0 {
		t.Fatalf("SizeBytes should be 0 after lazy delete, got %d", got)
	}
}

func TestEvictionPrefersExpired(t *testing.T) {
	// Each body is 1 byte; cap at 2 bytes so the third entry forces an eviction.
	// The expired entry should be chosen over the live one.
	evicted := 0
	c := cache.New(2, func(n int) { evicted += n })

	expired := entry("x", time.Millisecond)
	expired.CachedAt = time.Now().Add(-time.Second) // already expired
	c.Set("expired", expired)
	c.Set("live", entry("y", time.Minute)) // total 2 bytes, at cap

	// Adding a third entry must evict the expired one, leaving the live one.
	c.Set("third", entry("z", time.Minute))

	if _, ok := c.Get("live"); !ok {
		t.Fatal("non-expired entry should survive when an expired entry is available to evict")
	}
	if evicted == 0 {
		t.Fatal("expected at least one eviction")
	}
}

func TestEvictionFreesCorrectBytes(t *testing.T) {
	// Cap at 5 bytes. Setting a second 5-byte entry evicts the first.
	// Verify totalBytes returns to 5 (not 0 or 10) after the eviction.
	c := cache.New(5, nil)
	c.Set("a", entry("hello", time.Minute)) // 5 bytes, at cap
	c.Set("b", entry("world", time.Minute)) // evicts "a", stores "b"

	if got := c.SizeBytes(); got != 5 {
		t.Fatalf("expected 5 bytes after eviction, got %d", got)
	}
	if c.Len() != 1 {
		t.Fatalf("expected 1 entry after eviction, got %d", c.Len())
	}
}

func TestEvictionFreesCorrectBytes_IncludesHeaders(t *testing.T) {
	// body: "hi" = 2 bytes, "Content-Type: text/plain" = 22 bytes → 24 bytes per entry.
	// Cap at 24 bytes so the second entry evicts the first.
	h := http.Header{"Content-Type": {"text/plain"}}
	c := cache.New(24, nil)
	c.Set("a", entryWithHeaders("hi", time.Minute, h)) // 24 bytes, at cap
	c.Set("b", entryWithHeaders("hi", time.Minute, h)) // evicts "a", stores "b"

	if got := c.SizeBytes(); got != 24 {
		t.Fatalf("expected 24 bytes after eviction (body+headers), got %d", got)
	}
}
