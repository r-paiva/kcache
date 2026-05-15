// SPDX-FileCopyrightText: 2026 Rui Paiva <kcache.catapult615@passfwd.com>
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cilium/ebpf/rlimit"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"kache/internal/cache"
	"kache/internal/iface"
	"kache/internal/k8s"
	"kache/internal/metrics"
	"kache/internal/origdst"
	"kache/internal/policy"
	"kache/internal/proxy"
)

func main() {
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
	proxyAddr := flag.String("proxy-addr", "0.0.0.0:8080", "Address for the cache proxy listener")
	metricsAddr := flag.String("metrics-addr", "0.0.0.0:9090", "Address for the Prometheus metrics endpoint")
	statsInterval := flag.Duration("stats", 0, "Print a metrics summary on this interval (e.g. 10s). 0 disables.")
	maxCacheBytes := flag.Int64("max-cache-bytes", 256<<20, "Total byte budget for the in-memory cache (0 = unlimited).")
	maxBodyBytes := flag.Int64("max-body-bytes", 1<<20, "Maximum response body size to cache per request (0 = unlimited).")
	flag.Parse()

	level := parseLogLevel(*logLevel)
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if err := rlimit.RemoveMemlock(); err != nil {
		slog.Error("remove memlock", "err", err)
		os.Exit(1)
	}

	var objs kcacheObjects
	if err := loadKcacheObjects(&objs, nil); err != nil {
		slog.Error("load BPF objects", "err", err)
		os.Exit(1)
	}
	defer objs.Close()

	// Populate the proxy redirect target so the TC ingress program knows
	// where to send intercepted connections.
	// NODE_IP is injected via the downward API in Kubernetes; falls back to
	// 127.0.0.1 for local testing.
	redirectIP := net.ParseIP(os.Getenv("NODE_IP")).To4()
	if redirectIP == nil {
		redirectIP = net.IPv4(127, 0, 0, 1).To4()
	}
	_, portStr, err := net.SplitHostPort(*proxyAddr)
	if err != nil {
		slog.Error("parse proxy-addr", "err", err)
		os.Exit(1)
	}
	redirectPort, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		slog.Error("parse proxy port", "err", err)
		os.Exit(1)
	}

	var proxyTgt struct {
		IP   uint32
		Port uint16
		Pad  uint16
	}
	proxyTgt.IP = binary.BigEndian.Uint32(redirectIP)
	proxyTgt.Port = uint16(redirectPort)
	tgtKey := uint32(0)
	if err := objs.ProxyTgtMap.Put(&tgtKey, &proxyTgt); err != nil {
		slog.Error("set BPF proxy target", "err", err)
		os.Exit(1)
	}
	slog.Info("BPF proxy target configured", "ip", redirectIP.String(), "port", redirectPort)

	// Attach TC BPF programs to all existing pod veths and watch for new or disconnected ones.
	mgr := iface.New(objs.TcIngress, objs.TcEgress)
	if err := mgr.AttachExisting(); err != nil {
		slog.Error("attach TC to existing veths", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Watch(ctx)

	slog.Info("TC BPF programs attached to pod veths")
	if level == slog.LevelDebug {
		slog.Debug("BPF kernel logs available", "cmd", "sudo cat /sys/kernel/debug/tracing/trace_pipe")
	}

	var c cache.Cache
	c = cache.New(*maxCacheBytes, func(n int) {
		metrics.Evictions.Add(float64(n))
	})

	// CacheEntries and CacheSizeBytes are read on every Prometheus scrape
	// rather than pushed on every mutation — zero overhead on the hot path.
	prometheus.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "kcache_cache_entries_total",
			Help: "Current number of entries in the cache.",
		}, func() float64 { return float64(c.Len()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "kcache_cache_size_bytes",
			Help: "Total bytes stored in the cache (body + headers).",
		}, func() float64 { return float64(c.SizeBytes()) }),
	)

	origDstFn := func(conn net.Conn) (net.IP, uint16, error) {
		peer, ok := conn.RemoteAddr().(*net.TCPAddr)
		if !ok {
			return nil, 0, nil
		}
		return origdst.Lookup(objs.OrigDst, peer)
	}

	// Try to connect to the k8s API for CachePolicy support.
	// Declare p before the watcher so the onChange closure captures
	// the variable, not a value — p will be assigned before the watcher
	// calls onChange for the first time.
	var p *proxy.Proxy

	watcher, err := k8s.New(func(pol *policy.Policy) {
		p.SetPolicy(pol)
		slog.Info("cache policy reloaded")
	})
	if err != nil {
		slog.Warn("k8s watcher unavailable, running without CachePolicy support", "err", err)
		p = proxy.New(c, policy.New(nil), origDstFn, nil, *maxBodyBytes)
	} else {
		p = proxy.New(c, policy.New(nil), origDstFn, watcher.NamespaceLookup, *maxBodyBytes)
		go watcher.Run(ctx)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	go func() {
		slog.Info("metrics listening", "addr", *metricsAddr)
		if err := http.ListenAndServe(*metricsAddr, mux); err != nil {
			slog.Error("metrics server", "err", err)
		}
	}()

	go func() {
		if err := p.ListenAndServe(*proxyAddr); err != nil {
			slog.Error("proxy", "err", err)
			os.Exit(1)
		}
	}()

	if *statsInterval > 0 {
		go printStatsSummary(ctx, *statsInterval, *metricsAddr)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	slog.Info("shutting down")
}

func printStatsSummary(ctx context.Context, interval time.Duration, metricsAddr string) {
	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			resp, err := client.Get("http://" + metricsAddr + "/metrics")
			if err != nil {
				slog.Warn("stats fetch failed", "err", err)
				continue
			}
			var lines []string
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				line := sc.Text()
				if strings.HasPrefix(line, "kcache_") && !strings.HasPrefix(line, "#") {
					lines = append(lines, "  "+line)
				}
			}
			resp.Body.Close()
			if len(lines) > 0 {
				slog.Info("stats\n" + strings.Join(lines, "\n"))
			}
		}
	}
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
