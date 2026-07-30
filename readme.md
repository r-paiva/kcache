<!--
SPDX-FileCopyrightText: Copyright (c) 2026, the latch developers

SPDX-License-Identifier: Apache-2.0
-->

# latch

> **Early alpha.** Expect breaking changes.

A transparent, node-level HTTP/HTTPS cache for Kubernetes. Traffic is intercepted at the TC BPF layer on each node — pods make normal requests and responses are served from cache without any sidecar or code change.

This repository holds the latch source (Go + BPF). Related repos:

- **[latch-charts](https://codeberg.org/latch/latch-charts)** — the official Helm chart and `CachePolicy` examples.

## How it works

latch runs as a DaemonSet. On each node it:

1. Attaches a TC BPF program to every pod veth interface it discovers.
2. Intercepts outbound HTTP (port 80) and HTTPS (port 443) connections and redirects them to the latch proxy process.
3. For HTTP: checks the cache and replies on a hit, or forwards to upstream and stores the response on a miss.
4. For HTTPS: if a matching `CachePolicy` exists, performs TLS MITM using a cluster CA — the proxy terminates the client TLS, caches the response, and re-establishes TLS to the upstream. Connections with no matching policy are spliced through raw without interception.

Cache behaviour is controlled by `CachePolicy` CRDs — scoped by namespace and pod label selector.

## Docker Hub

```sh
# Container image (amd64 + arm64)
docker pull rpaiva0/latch:dev

# Helm chart — packaged in the latch-charts repo
helm install latch oci://registry-1.docker.io/rpaiva0/latch
```

## Requirements

- Linux kernel 6.6+
- Tested with CNIs: Cilium, Flannel, Calico

## CachePolicy example

```yaml
apiVersion: latch.io/v1alpha1
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

To enable HTTPS caching, latch needs a CA certificate to sign per-SNI leaf certs on demand. Pods must trust this CA so they accept the MITM cert.

1. Create a Kubernetes Secret holding the CA `tls.crt` / `tls.key`.
2. Set `latch.tls.caSecretName` in your Helm values to that Secret's name.
3. Mount the CA cert into your workloads (or add it to their trust store) so they accept the MITM leaf certs.

See the `latch` chart's `values.yaml` in **latch-charts** for the full TLS configuration reference. The **latch-homelab** repo has `make gen-ca` / `make gen-backend-tls` helpers that generate a CA and a CA-signed backend cert for local testing.

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

Prometheus metrics are exposed at `:9090/metrics`. All latch metrics are prefixed with `latch_`.

```sh
curl -s http://localhost:9090/metrics | grep '^latch_'
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

```
