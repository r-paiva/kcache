<!--
SPDX-FileCopyrightText: Copyright (c) 2026, the k-cache developers

SPDX-License-Identifier: Apache-2.0
-->

# k-cache

> **Early alpha.** Expect breaking changes.

A transparent, node-level HTTP/HTTPS cache for Kubernetes. Traffic is intercepted at the TC BPF layer on each node — pods make normal requests and responses are served from cache without any sidecar or code change.

## How it works

kcache runs as a DaemonSet. On each node it:

1. Attaches a TC BPF program to every pod veth interface it discovers.
2. Intercepts outbound HTTP (port 80) and HTTPS (port 443) connections and redirects them to the kcache proxy process.
3. For HTTP: checks the cache and replies on a hit, or forwards to upstream and stores the response on a miss.
4. For HTTPS: if a matching `CachePolicy` exists, performs TLS MITM using a cluster CA — the proxy terminates the client TLS, caches the response, and re-establishes TLS to the upstream. Connections with no matching policy are spliced through raw without interception.

Cache behaviour is controlled by `CachePolicy` CRDs — scoped by namespace and pod label selector.

## Docker Hub

```sh
# Container image (amd64 + arm64)
docker pull rpaiva0/kcache:dev

# Helm chart
helm install kcache oci://registry-1.docker.io/rpaiva0/kcache-demo --version 0.2.5
```

## Requirements

- Linux kernel 6.6+
- Tested with CNIs: Cilium, Flannel, Calico

## CachePolicy example

```yaml
apiVersion: kcache.io/v1alpha1
kind: CachePolicy
metadata:
  name: my-app
  namespace: my-namespace
spec:
  podSelector:
    matchLabels:
      app: my-app
  rules:
    - host: "*"
      port: 80
      methods: [GET]
      ttl: 60s
    - host: "api.example.com"
      port: 443
      methods: [GET]
      ttl: 300s
```

A pod with no matching policy is unaffected — traffic passes through transparently.

## TLS MITM setup

To enable HTTPS caching, kcache needs a CA certificate to sign per-SNI leaf certs on demand. Pods must trust this CA so they accept the MITM cert.

```sh
# Generate CA and store as a Kubernetes Secret
make gen-ca

# Sign the backend's TLS cert with the kcache CA (if using an in-cluster HTTPS backend)
make gen-backend-tls
```

Then set `kcache.tls.caSecretName` in your Helm values to the name of the CA secret. See `charts/kcache-demo/values.yaml` for the full TLS configuration reference.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-proxy-addr` | `0.0.0.0:8080` | Address for the cache proxy listener |
| `-metrics-addr` | `0.0.0.0:9090` | Address for the Prometheus metrics endpoint |
| `-log-level` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `-log-file` | _(disabled)_ | Path to write JSON logs to in addition to stderr |
| `-tls-ca-dir` | _(disabled)_ | Directory with `tls.crt` / `tls.key` for TLS MITM |
| `-iface-pattern` | auto | Regexp matching host-side veth names to attach BPF to |
| `-max-cache-bytes` | `256 MiB` | Total in-memory cache budget |
| `-max-body-bytes` | `1 MiB` | Max response body size to cache per request |

## Metrics

Prometheus metrics are exposed at `:9090/metrics`. All kcache metrics are prefixed with `kcache_`.

```sh
curl -s http://localhost:9090/metrics | grep '^kcache_'
# or
make metrics
```

## Building

```sh
# Install BPF toolchain (Debian/Ubuntu)
make deps

# Compile BPF program and Go binary
make build

# Run locally (requires root)
make run

# Run tests
make test

# Multi-arch image push to Docker Hub
make image-push
```
