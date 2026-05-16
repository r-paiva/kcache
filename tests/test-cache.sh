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

req() {
  local pod=$1 url=$2
  local out
  out=$(K exec -n "$NS" "$pod" -- \
    curl -s -D - -o /dev/null "$url" 2>/dev/null \
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

# ── test 1: cold cache → miss then hit for every endpoint ─────────────────────

bold "── 1. prime all endpoints from ${FIRST}"
for url in "${ENDPOINTS[@]}"; do
  path="${url#$BASE}"
  echo -n "   ${path} req 1 (expect MISS): "; req "$FIRST" "$url"
  echo -n "   ${path} req 2 (expect HIT):  "; req "$FIRST" "$url"
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

bold "── done"
echo "   logs:    kubectl ${CTX:+--context=$CTX }logs -n $NS -l app.kubernetes.io/component=kcache -f"
echo "   grafana: kubectl ${CTX:+--context=$CTX }port-forward -n monitoring svc/grafana 3000:80"
