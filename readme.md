<!--
SPDX-FileCopyrightText: Copyright (c) 2026, the k-cache developers

SPDX-License-Identifier: Apache-2.0
-->

# k-cache

> **Early alpha.** HTTP only. Expect breaking changes.

A transparent, node-level HTTP cache for Kubernetes. Traffic is intercepted at the TC BPF layer on each node — pods make normal HTTP requests and responses are served from cache without any sidecar or code change.

## How it works

kcache runs as a DaemonSet. On each node it:

1. Attaches a TC BPF program to every pod veth interface it discovers.
2. Intercepts outbound HTTP connections and redirects them to the kcache proxy process.
3. Checks its in-memory cache; on a hit it replies immediately, on a miss it forwards to the upstream and caches the response.

Cache behaviour is controlled by `CachePolicy` CRDs — scoped by namespace and pod label selector.

## Requirements

- Linux kernel 6.6+
- Tested with CNIs: Cilium, Flannel, Calico

## Quick start (minikube)

```sh
# Start minikube with your preferred CNI (Cilium shown)
minikube start --cni=cilium --memory=4096

# Build image and deploy (applies CRD, helm install, restarts DaemonSet)
make image
make deploy-cilium

# Run the cache test
bash tests/test-cache.sh default cilium
```

Or for subsequent iterations:

```sh
make image deploy-cilium   # rebuild and redeploy
make test-cilium           # run integration tests
make monitoring-cilium     # install Prometheus + Grafana
```

> **Note on the CRD:** `helm upgrade` does not update CRDs. If you delete and
> re-apply the chart, or modify `CachePolicyRule`, run `make crd-cilium` to
> apply the latest schema manually. `make deploy-*` does this automatically.

## CachePolicy example

```yaml
apiVersion: kcache.io/v1alpha1
kind: CachePolicy
metadata:
  name: catalog-cache
  namespace: ecommerce
spec:
  podSelector:
    matchLabels:
      app: catalog-service
  rules:
    - host: "*"
      port: 80
      methods: [GET]
      ttl: 60s
```

A pod with no matching policy is unaffected — traffic passes through transparently.

## Metrics

Prometheus metrics are exposed at `:9090/metrics`. All kcache metrics are prefixed with `kcache_`.

```sh
curl -s http://localhost:9090/metrics | grep '^kcache_'
```

## Current limitations

- **HTTP only** — HTTPS traffic is not intercepted. to be implemented.
- **In-memory cache** — cache is per-node and not persisted across restarts.
- **No upstream `Vary` header support** — cache key does not automatically adapt to `Vary` response headers; use `varyHeaders` in the policy as a workaround.

## Building

```sh
# Install BPF toolchain (Debian/Ubuntu)
make deps

# Compile BPF program and Go binary
make

# Run locally (requires root)
make run
```
