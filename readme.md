<!--
SPDX-FileCopyrightText: 2026 Rui Paiva <kcache.catapult615@passfwd.com>

SPDX-License-Identifier: Apache-2.0
-->

# kcache

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
- Kubernetes 1.24+
- Supported CNIs: Cilium, Flannel, Calico

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
    - host: "inventory-service"
      port: 80
      methods: [GET]
      paths: ["/api/products", "/api/categories"]
      ttl: 5m
    - host: "pricing-service"
      port: 80
      methods: [GET]
      ttl: 30s
```

A pod with no matching policy is unaffected — traffic passes through transparently.

## Configuration flags

| Flag | Default | Description |
|------|---------|-------------|
| `-proxy-addr` | `0.0.0.0:8080` | Cache proxy listener address |
| `-metrics-addr` | `0.0.0.0:9090` | Prometheus metrics endpoint |
| `-max-cache-bytes` | `268435456` (256 MiB) | Total in-memory cache budget |
| `-max-body-bytes` | `1048576` (1 MiB) | Max response body size to cache |
| `-stats` | `0` (off) | Print a metrics summary on this interval (e.g. `10s`) |
| `-log-level` | `info` | Log level: `debug`, `info`, `warn`, `error` |

## Metrics

Prometheus metrics are exposed at `:9090/metrics`. All kcache metrics are prefixed with `kcache_`.

```sh
curl -s http://localhost:9090/metrics | grep '^kcache_'
```

## Current limitations

- **HTTP only** — HTTPS traffic is not intercepted. TLS MITM is on the roadmap.
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