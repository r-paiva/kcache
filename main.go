// SPDX-FileCopyrightText: 2026 The latch Contributors
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"codeberg.org/latch/latch/bpf"
	"codeberg.org/latch/latch/internal/cache"
	"codeberg.org/latch/latch/internal/constants"
	"codeberg.org/latch/latch/internal/env"
	"codeberg.org/latch/latch/internal/iface"
	"codeberg.org/latch/latch/internal/k8s"
	"codeberg.org/latch/latch/internal/logging"
	"codeberg.org/latch/latch/internal/metrics"
	"codeberg.org/latch/latch/internal/origdst"
	"codeberg.org/latch/latch/internal/podveth"
	"codeberg.org/latch/latch/internal/policy"
	"codeberg.org/latch/latch/internal/proxy"
	"codeberg.org/latch/latch/internal/tlsmitm"
	"codeberg.org/latch/latch/internal/version"
)

func main() {
	config := env.New()
	logging.New(config.LogLevel)
	slog.Info("starting latch", "version", version.Version)

	bpfProgram := bpf.Load(config.ProxyAddr)
	defer bpfProgram.Close() //nolint:errcheck

	mgr := iface.New(bpfProgram.TcIngress, bpfProgram.TcEgress)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Watch(ctx)

	slog.Debug("BPF kernel logs available", "cmd", "sudo cat /sys/kernel/debug/tracing/trace_pipe")

	cacheStore := cache.New(config.MaxCacheBytes, func(n int) {
		metrics.Evictions.Add(float64(n))
	})

	origDstFn := func(conn net.Conn) (net.IP, uint16, error) {
		peer, ok := conn.RemoteAddr().(*net.TCPAddr)
		if !ok {
			return nil, 0, nil
		}
		return origdst.Lookup(bpfProgram.OrigDst, peer)
	}

	var ca *tlsmitm.CA
	if config.TlsCaDir != "" {
		var caErr error
		ca, caErr = tlsmitm.LoadCA(config.TlsCaDir)
		if caErr != nil {
			slog.Warn("TLS MITM disabled: failed to load CA", "dir", config.TlsCaDir, "err", caErr)
		} else {
			slog.Info("TLS MITM enabled", "ca-dir", config.TlsCaDir)
		}
	} else {
		slog.Info("TLS MITM disabled")
	}

	var p *proxy.Proxy

	var currentPolicy atomic.Pointer[policy.Policy]
	currentPolicy.Store(policy.New(nil))

	watcher, err := k8s.New(func(pol *policy.Policy) {
		p.SetPolicy(pol)
		currentPolicy.Store(pol)
		slog.Info("cache policy reloaded")
	})
	if err != nil {
		slog.Error("k8s watcher unavailable, cannot run policy-gated attachment", "err", err)
		os.Exit(constants.ExitInitK8SWatcherError)
	}
	p = proxy.New(cacheStore, currentPolicy.Load(), origDstFn, watcher.NamespaceLookup, config.MaxBodyBytes, ca)

	resolver := podveth.NewResolver(config.ProcRoot)
	covered := func(ns string, podLabels map[string]string) bool {
		return currentPolicy.Load().Covered(ns, podLabels)
	}
	trigger := make(chan struct{}, 1)
	watcher.SetReconcileTrigger(func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	})

	go watcher.Run(ctx)
	go podveth.Enforce(ctx, resolver, 30*time.Second, trigger, watcher.NamespaceLookup, covered, mgr)
	slog.Info("policy-gated attachment running")

	go func() {
		if err := p.ListenAndServe(config.ProxyAddr); err != nil {
			slog.Error("proxy", "err", err)
			os.Exit(constants.ExitInitProxyError)
		}
	}()

	metrics.StartMetricsServer(cacheStore, config.MetricsAddr)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	slog.Info("shutting down")
	os.Exit(constants.ExitSuccess)
}
