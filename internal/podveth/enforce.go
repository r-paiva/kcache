// SPDX-FileCopyrightText: 2026 The latch Contributors
//
// SPDX-License-Identifier: Apache-2.0

package podveth

import (
	"context"
	"log/slog"
	"time"
)

// NamespaceFunc returns a pod's namespace and labels for its IP, or an empty
// namespace when the IP isn't a tracked Kubernetes pod
type NamespaceFunc func(podIP string) (namespace string, labels map[string]string)

// CoveredFunc reports whether a pod's namespace+labels match a CachePolicy.
type CoveredFunc func(namespace string, labels map[string]string) bool

// Attacher applies a desired set of host veth ifindexes — attach the missing detach the rest
type Attacher interface {
	Reconcile(desired map[int]struct{})
}

type podResolver interface {
	Resolve() (map[string]int, error)
}

// Enforce reconciles policy-gated attachment: resolve the node's pods to their
// host veths, keep those a CachePolicy matches, hand that set to the Attacher
// it reconciles on every trigger (pod/policy change)
func Enforce(ctx context.Context, r podResolver, interval time.Duration, trigger <-chan struct{}, nsFn NamespaceFunc, coveredFn CoveredFunc, attacher Attacher) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	enforceOnce(r, nsFn, coveredFn, attacher) // attach without waiting for the first event

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			enforceOnce(r, nsFn, coveredFn, attacher)
		case <-trigger:
			enforceOnce(r, nsFn, coveredFn, attacher)
		}
	}
}

func enforceOnce(r podResolver, nsFn NamespaceFunc, coveredFn CoveredFunc, attacher Attacher) {
	mappings, err := r.Resolve()
	if err != nil {
		// Skip rather than reconcile an empty set, which would detach everything.
		slog.Warn("podveth: resolve failed, skipping reconcile", "err", err)
		return
	}

	desired := make(map[int]struct{})
	for ip, hostIdx := range mappings {
		namespace, podLabels := nsFn(ip)
		if namespace == "" {
			continue
		}
		if coveredFn(namespace, podLabels) {
			desired[hostIdx] = struct{}{}
		}
	}

	attacher.Reconcile(desired)
}
