#!/usr/bin/env bash

# SPDX-FileCopyrightText: Copyright (c) 2026, the k-cache developers
#
# SPDX-License-Identifier: Apache-2.0

# test-cache.sh — verify kcache is intercepting and caching HTTP responses.
#   ./tests/test-cache.sh [namespace] [kubectl-context]
#   ./tests/test-cache.sh                    # default namespace, current context
#   ./tests/test-cache.sh default flannel    # target the flannel cluster

set -euo pipefail

NS="${1:-default}"
CTX="${2:-}"

# Build kubectl command — optionally scoped to a specific context.
K() { kubectl ${CTX:+--context="$CTX"} "$@"; }

# ── helpers ────────────────────────────────────────────────────────────────────

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
bold()  { printf '\033[1m%s\033[0m\n'  "$*"; }

CA_CERT=/etc/ssl/kcache-ca.crt

req() {
  local pod=$1 url=$2
  local out
  out=$(K exec -n "$NS" "$pod" -- \
    curl -s -D - -o /dev/null --cacert "$CA_CERT" "$url" 2>/dev/null \
    | grep -iE "^HTTP/|^x-cache:" | tr -d '\r') || true

  local status xcache
  status=$(echo "$out" | grep -i "^HTTP/" | awk '{print $1, $2, $3}' || true)
  xcache=$(echo "$out" | grep -i "^x-cache:" | awk '{print $2}' || true)

  if [[ "$xcache" == "HIT" ]]; then
    green "  ${status}  X-Cache: HIT"
  elif [[ "$xcache" == "MISS" ]]; then
    red   "  ${status}  X-Cache: MISS"
  else
    printf '  %s  (no X-Cache header)\n' "$status"
  fi
}

# req_body <pod> <url> — prints formatted status to stderr; emits body on stdout.
req_body() {
  local pod=$1 url=$2
  local full
  full=$(K exec -n "$NS" "$pod" -- \
    curl -s -D - --cacert "$CA_CERT" "$url" 2>/dev/null | tr -d '\r') || true

  local status xcache body
  status=$(echo "$full" | grep -i "^HTTP/"    | awk '{print $1, $2, $3}' || true)
  xcache=$(echo "$full" | grep -i "^x-cache:" | awk '{print $2}'         || true)
  body=$(  echo "$full" | awk '/^$/{p=1;next} p' | head -c 4096)

  if [[ "$xcache" == "HIT" ]]; then
    green "  ${status}  X-Cache: HIT" >&2
  elif [[ "$xcache" == "MISS" ]]; then
    red   "  ${status}  X-Cache: MISS" >&2
  else
    printf '  %s  (no X-Cache header)\n' "$status" >&2
  fi

  printf '%s' "$body"
}

# req_pair <label> <pod> <url> — MISS then HIT; fails if bodies differ.
req_pair() {
  local label=$1 pod=$2 url=$3
  local miss_body hit_body

  echo -n "   ${label} req 1 (expect MISS): "
  miss_body=$(req_body "$pod" "$url")
  echo -n "   ${label} req 2 (expect HIT):  "
  hit_body=$(req_body "$pod" "$url")

  if [[ -z "$miss_body" ]]; then
    red "   FAIL: ${label} MISS response had empty body"
    return 1
  fi
  if [[ "$hit_body" != "$miss_body" ]]; then
    red "   FAIL: ${label} HIT body differs from MISS body"
    red "     MISS: ${miss_body:0:120}"
    red "     HIT:  ${hit_body:0:120}"
    return 1
  fi
}

# req_vary <pod> <url> <accept-encoding>
# Prints X-Cache status to stderr (visible on terminal).
# Writes the response body to stdout (for capture with $(...)).
req_vary() {
  local pod=$1 url=$2 encoding=$3
  local full xcache body
  full=$(K exec -n "$NS" "$pod" -- \
    curl -s -D - --cacert "$CA_CERT" -H "Accept-Encoding: $encoding" "$url" 2>/dev/null \
    | tr -d '\r') || true

  xcache=$(echo "$full" | grep -i "^x-cache:" | awk '{print $2}')
  vary=$(echo "$full"   | grep -i "^vary:"    | head -1)
  body=$(echo "$full"  | awk '/^$/{p=1;next} p{print;exit}')

  if   [[ "$xcache" == "HIT"  ]]; then green "  X-Cache: HIT   body: ${body}" >&2
  elif [[ "$xcache" == "MISS" ]]; then red   "  X-Cache: MISS  body: ${body}${vary:+  (${vary})}" >&2
  else printf '  (no X-Cache)  body: %s\n' "$body" >&2
  fi

  printf '%s' "$body"
}

# ── discover resources ─────────────────────────────────────────────────────────

BACKEND_SVC=$(K get svc -n "$NS" \
  -l "app.kubernetes.io/component=backend" \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)

if [[ -z "$BACKEND_SVC" ]]; then
  echo "error: no backend service found in namespace '$NS'${CTX:+ (context: $CTX)}." >&2
  exit 1
fi

PODS=()
while IFS= read -r p; do
  [[ -n "$p" ]] && PODS+=("$p")
done < <(K get pods -n "$NS" \
  -l "app.kubernetes.io/component=curl-client" \
  --field-selector=status.phase=Running \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)

if [[ ${#PODS[@]} -eq 0 ]]; then
  echo "error: no running curl-client pods in namespace '$NS'${CTX:+ (context: $CTX)}." >&2
  exit 1
fi

BASE="http://${BACKEND_SVC}"
FIRST="${PODS[0]}"

ENDPOINTS=(
  "${BASE}/"
  "${BASE}/api/status"
  "${BASE}/api/products"
  "${BASE}/api/users"
  "${BASE}/api/orders"
  "${BASE}/static/styles.css"
  "${BASE}/static/app.js"
)

bold "======================================================"
bold " kcache cache test"
echo " context   : ${CTX:-$(kubectl config current-context 2>/dev/null)}"
echo " namespace : $NS"
echo " backend   : $BASE"
echo " pods      : ${PODS[*]}"
bold "======================================================"
echo ""

# ── pre-flight: verify curl clients are regular pods (no hostNetwork) ──────────

bold "── pre-flight: pod network isolation check"
FAIL_PREFLIGHT=0
for POD in "${PODS[@]}"; do
  HN=$(K get pod -n "$NS" "$POD" -o jsonpath='{.spec.hostNetwork}' 2>/dev/null)
  if [[ "$HN" == "true" ]]; then
    red "  WARN: $POD has hostNetwork=true — interception test is not meaningful"
    FAIL_PREFLIGHT=1
  else
    green "  $POD hostNetwork=false (regular pod netns) ✓"
  fi
done
if [[ $FAIL_PREFLIGHT -eq 0 ]]; then
  echo "  TC BPF intercepts across pod netns boundaries — no hostNetwork needed on clients"
fi
echo ""

# ── test 1: cold cache → miss then hit for every endpoint ─────────────────────

bold "── 1. prime all endpoints from ${FIRST}"
for url in "${ENDPOINTS[@]}"; do
  path="${url#$BASE}"
  req_pair "$path" "$FIRST" "$url"
done
echo ""

# ── test 2: cross-pod cache sharing ───────────────────────────────────────────

if [[ ${#PODS[@]} -gt 1 ]]; then
  bold "── 2. cross-pod sharing (expect HIT on all endpoints)"
  for POD in "${PODS[@]:1}"; do
    bold "   pod: ${POD}"
    for url in "${ENDPOINTS[@]}"; do
      path="${url#$BASE}"
      echo -n "     ${path}: "; req "$POD" "$url"
    done
  done
  echo ""
fi

# ── test 3: vary header partitioning ──────────────────────────────────────────

bold "── 3. vary header partitioning (${BASE}/vary-test)"

FAIL=0

echo "   req 1 gzip     (expect MISS):"
body_gzip_1=$(req_vary "$FIRST" "${BASE}/vary-test" "gzip")
echo ""

echo "   req 2 gzip     (expect HIT, body must match req 1):"
body_gzip_2=$(req_vary "$FIRST" "${BASE}/vary-test" "gzip")
echo ""

echo "   req 3 identity (expect MISS — different variant):"
body_id_1=$(req_vary   "$FIRST" "${BASE}/vary-test" "identity")
echo ""

echo "   req 4 identity (expect HIT, body must match req 3):"
body_id_2=$(req_vary   "$FIRST" "${BASE}/vary-test" "identity")
echo ""

if [[ "$body_gzip_2" != "$body_gzip_1" ]]; then
  red   "   FAIL: gzip HIT returned wrong body (got '$body_gzip_2', want '$body_gzip_1')"
  FAIL=1
fi
if [[ "$body_id_2" != "$body_id_1" ]]; then
  red   "   FAIL: identity HIT returned wrong body (got '$body_id_2', want '$body_id_1')"
  FAIL=1
fi
if [[ "$body_gzip_1" == "$body_id_1" ]]; then
  red   "   FAIL: gzip and identity variants returned identical bodies ('$body_gzip_1')"
  FAIL=1
fi

if [[ $FAIL -eq 0 ]]; then
  green "   vary partitioning OK — each variant cached and served correctly"
fi
echo ""

# ── test 4: HTTPS interception (skipped if no CA cert is mounted) ─────────────

HTTPS_BASE="https://${BACKEND_SVC}"
HTTPS_ENDPOINTS=(
  "${HTTPS_BASE}/"
  "${HTTPS_BASE}/api/status"
  "${HTTPS_BASE}/api/products"
)

HAS_CA=$(K exec -n "$NS" "$FIRST" -- test -f "$CA_CERT" 2>/dev/null && echo yes || echo no)

if [[ "$HAS_CA" == "yes" ]]; then
  bold "── 4. HTTPS interception (TLS MITM via kcache CA)"
  echo "   CA cert: $CA_CERT"
  echo ""

  bold "   prime HTTPS endpoints from ${FIRST}"
  for url in "${HTTPS_ENDPOINTS[@]}"; do
    path="${url#$HTTPS_BASE}"
    req_pair "$path" "$FIRST" "$url"
  done
  echo ""

  if [[ ${#PODS[@]} -gt 1 ]]; then
    bold "   cross-pod HTTPS sharing (expect HIT)"
    for POD in "${PODS[@]:1}"; do
      bold "   pod: ${POD}"
      for url in "${HTTPS_ENDPOINTS[@]}"; do
        path="${url#$HTTPS_BASE}"
        echo -n "     ${path}: "; req "$POD" "$url"
      done
    done
    echo ""
  fi
else
  bold "── 4. HTTPS interception"
  echo "   skipped — CA cert not mounted (run 'make gen-ca && make deploy-kcache')"
  echo ""
fi

bold "── done"
echo "   logs:    kubectl ${CTX:+--context=$CTX }logs -n $NS -l app.kubernetes.io/component=kcache -f"
echo "   grafana: kubectl ${CTX:+--context=$CTX }port-forward -n monitoring svc/grafana 3000:80"
