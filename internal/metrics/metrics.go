// SPDX-FileCopyrightText: 2026 The latch Contributors
//
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"log/slog"
	"net/http"

	"codeberg.org/latch/latch/internal/cache"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	Requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "latch_requests_total",
		Help: "Total HTTP requests handled by the cache proxy.",
	}, []string{"host", "method", "cache", "path"})

	ResponseSize = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "latch_response_size_bytes",
		Help:    "HTTP response body size in bytes.",
		Buckets: prometheus.ExponentialBuckets(256, 4, 10),
	}, []string{"host", "method", "path"})

	RequestSize = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "latch_request_size_bytes",
		Help:    "HTTP request body size in bytes.",
		Buckets: prometheus.ExponentialBuckets(256, 4, 10),
	}, []string{"host", "method"})

	UpstreamLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "latch_upstream_latency_seconds",
		Help:    "Latency of upstream HTTP calls on cache misses.",
		Buckets: prometheus.DefBuckets,
	}, []string{"host", "path"})

	HitLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "latch_hit_latency_seconds",
		Help:    "Latency of cache-hit responses.",
		Buckets: prometheus.DefBuckets,
	}, []string{"host", "path"})

	Evictions = promauto.NewCounter(prometheus.CounterOpts{
		Name: "latch_cache_evictions_total",
		Help: "Total number of cache entries evicted.",
	})

	BPFRedirects = promauto.NewCounter(prometheus.CounterOpts{
		Name: "latch_bpf_redirects_total",
		Help: "Total TCP connections redirected by the BPF hook.",
	})

	CacheSkipsBodyTooLarge = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "latch_cache_skip_body_too_large_total",
		Help: "Responses forwarded but not cached because the body exceeded the per-response size limit.",
	}, []string{"host"})

	CacheSkipsRequestBodyTooLarge = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "latch_cache_skip_request_body_too_large_total",
		Help: "Requests forwarded but not cached because the request body exceeded the buffering limit.",
	}, []string{"host"})
)

func StartMetricsServer(cache cache.Cache, metricsAddr string) {
	prometheus.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "latch_cache_entries_total",
			Help: "Current number of entries in the cache.",
		}, func() float64 { return float64(cache.Len()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "latch_cache_size_bytes",
			Help: "Total bytes stored in the cache (body + headers).",
		}, func() float64 { return float64(cache.SizeBytes()) }),
	)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	go func() {
		slog.Info("metrics listening", "addr", metricsAddr)
		if err := http.ListenAndServe(metricsAddr, mux); err != nil {
			slog.Error("metrics server", "err", err)
		}
	}()
}
