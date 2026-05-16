// SPDX-FileCopyrightText: Copyright (c) 2026, the k-cache developers
//
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// cache label values: "hit", "miss", "bypass", "error"
	// path is the URL path with query stripped (e.g. "/api/products").
	Requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kcache_requests_total",
		Help: "Total HTTP requests handled by the cache proxy.",
	}, []string{"host", "method", "cache", "path"})

	ResponseSize = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kcache_response_size_bytes",
		Help:    "HTTP response body size in bytes.",
		Buckets: prometheus.ExponentialBuckets(256, 4, 10),
	}, []string{"host", "method", "path"})

	RequestSize = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kcache_request_size_bytes",
		Help:    "HTTP request body size in bytes.",
		Buckets: prometheus.ExponentialBuckets(256, 4, 10),
	}, []string{"host", "method"})

	UpstreamLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kcache_upstream_latency_seconds",
		Help:    "Latency of upstream HTTP calls on cache misses.",
		Buckets: prometheus.DefBuckets,
	}, []string{"host", "path"})

	HitLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kcache_hit_latency_seconds",
		Help:    "Latency of cache-hit responses.",
		Buckets: prometheus.DefBuckets,
	}, []string{"host", "path"})

	Evictions = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kcache_cache_evictions_total",
		Help: "Total number of cache entries evicted.",
	})

	BPFRedirects = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kcache_bpf_redirects_total",
		Help: "Total TCP connections redirected by the BPF hook.",
	})

	CacheSkipsBodyTooLarge = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kcache_cache_skip_body_too_large_total",
		Help: "Responses forwarded but not cached because the body exceeded the per-response size limit.",
	}, []string{"host"})

	CacheSkipsRequestBodyTooLarge = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kcache_cache_skip_request_body_too_large_total",
		Help: "Requests forwarded but not cached because the request body exceeded the buffering limit.",
	}, []string{"host"})

	// Placeholder for Phase 3
	TLSHandshakes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kcache_tls_handshakes_total",
		Help: "Total TLS handshakes performed by the MITM layer.",
	}, []string{"host", "result"})
)
