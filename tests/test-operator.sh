#!/usr/bin/env bash

# SPDX-FileCopyrightText: Copyright (c) 2026, the k-cache developers
#
# SPDX-License-Identifier: Apache-2.0

# test-operator.sh — tests CachePolicy lifecycle: create, update, delete,
# path filtering, and podSelector scoping.
#   ./tests/test-operator.sh [namespace] [kubectl-context]

set -euo pipefail

NS="${1:-default}"
CTX="${2:-}"

K() { kubectl ${CTX:+--context="$CTX"} "$@"; }

red()   { printf '\033[31m  FAIL\033[0m %s\n' "$*"; }
green() { printf '\033[32m  PASS\033[0m %s\n' "$*"; }
bold()  { printf '\033[1m%s\033[0m\n' "$*"; }

FAILED=0
pass() { green "$*"; }
fail() { red   "$*"; FAILED=1; }

# ── discover resources ─────────────────────────────────────────────────────────
BACKEND_SVC=$(K get svc -n "$NS" \
  -l "app.kubernetes.io/component=backend" \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
[[ -z "$BACKEND_SVC" ]] && { echo "error: no backend service found" >&2; exit 1; }

POD=$(K get pods -n "$NS" \
  -l "app.kubernetes.io/component=curl-client" \
  --field-selector=status.phase=Running \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
[[ -z "$POD" ]] && { echo "error: no running curl-client pod found" >&2; exit 1; }

BASE="http://${BACKEND_SVC}"

# ── helpers ────────────────────────────────────────────────────────────────────

xcache() {
  local pod=$1 url=$2
  K exec -n "$NS" "$pod" -- \
    curl -s -D - -o /dev/null "$url" 2>/dev/null \
    | grep -i "^x-cache:" | awk '{print $2}' | tr -d '\r' || echo "NONE"
}

# Delete ALL CachePolicies in the namespace so no leftover policy interferes.
cleanup_policies() {
  K delete cachepolicy --all -n "$NS" --ignore-not-found 2>/dev/null || true
}

apply_policy() {
  K apply -f - 2>/dev/null <<EOF
apiVersion: kcache.io/v1alpha1
kind: CachePolicy
metadata:
  name: kcache-test
  namespace: ${NS}
spec:
$1
EOF
}

# Poll until xcache() returns the expected value or timeout (seconds) expires.
wait_xcache() {
  local pod=$1 url=$2 want=$3 timeout=${4:-15}
  for _ in $(seq 1 "$timeout"); do
    [[ "$(xcache "$pod" "$url")" == "$want" ]] && return 0
    sleep 1
  done
  return 1
}

# Send two requests to warm the cache; return X-Cache of the second.
prime() {
  xcache "$1" "$2" > /dev/null
  sleep 0.3
  xcache "$1" "$2"
}

# ── header ─────────────────────────────────────────────────────────────────────
bold "======================================================"
bold " kcache operator test"
echo " context   : ${CTX:-$(kubectl config current-context 2>/dev/null)}"
echo " namespace : $NS"
echo " backend   : $BASE"
echo " pod       : $POD"
bold "======================================================"
echo ""

# ── baseline: remove all policies and confirm bypass ──────────────────────────
cleanup_policies
if ! wait_xcache "$POD" "${BASE}/api/status" "NONE" 15; then
  echo "error: cannot establish bypass baseline — is kcache running?" >&2
  exit 1
fi

# Path allocation (each test uses a fresh path to avoid cache bleed):
#  Test 1  bypass check          /api/status
#  Test 2  basic MISS→HIT        /api/products
#  Test 3  delete→bypass         /api/products  (policy removed, cache ignored)
#  Test 4  path filter           /api/users (cached)  /api/orders (not cached)
#  Test 5  podSelector mismatch  /static/styles.css
#  Test 6  podSelector match     /static/app.js
#  Test 7  TTL expiry            /                    (5 s TTL)

# ── 1: no policy → bypass ─────────────────────────────────────────────────────
bold "── 1. no policy → transparent bypass"
if [[ "$(xcache "$POD" "${BASE}/api/status")" == "NONE" ]]; then
  pass "no X-Cache header when no policy exists"
else
  fail "expected bypass (no X-Cache) with no policy"
fi
echo ""

# ── 2: create policy → MISS then HIT ──────────────────────────────────────────
bold "── 2. create policy → MISS then HIT"
apply_policy '
  podSelector: {}
  rules:
    - host: "*"
      port: 80
      methods: [GET]
      ttl: 60s'

if wait_xcache "$POD" "${BASE}/api/products" "MISS" 15; then
  pass "first request is MISS after policy created"
else
  fail "timed out waiting for MISS — policy may not have been picked up"
fi

if result=$(prime "$POD" "${BASE}/api/products") && [[ "$result" == "HIT" ]]; then
  pass "second request is HIT"
else
  fail "expected HIT on second request, got: $result"
fi
echo ""

# ── 3: delete policy → bypass ─────────────────────────────────────────────────
bold "── 3. delete policy → transparent bypass"
# The cache still holds the /api/products entry, but with no matching rule
# the proxy bypasses the cache entirely — no X-Cache header.
cleanup_policies
if wait_xcache "$POD" "${BASE}/api/products" "NONE" 15; then
  pass "no X-Cache header after policy deleted (cache bypassed without a rule)"
else
  fail "expected bypass after policy deleted"
fi
echo ""

# ── 4: path filtering ─────────────────────────────────────────────────────────
bold "── 4. path filtering — /api/users cached, /api/orders bypassed"
apply_policy '
  podSelector: {}
  rules:
    - host: "*"
      port: 80
      methods: [GET]
      paths: ["/api/users", "/api/status"]
      ttl: 60s'

wait_xcache "$POD" "${BASE}/api/users" "MISS" 15 || true

if result=$(prime "$POD" "${BASE}/api/users") && [[ "$result" == "HIT" ]]; then
  pass "/api/users is cached (HIT)"
else
  fail "/api/users should be cached, got: $result"
fi

# /api/orders is not in the paths list — should bypass with no X-Cache header.
if result=$(xcache "$POD" "${BASE}/api/orders") && [[ "$result" == "NONE" ]]; then
  pass "/api/orders is bypassed (not in paths list)"
else
  fail "/api/orders should be bypassed (NONE), got: $result"
fi
echo ""

# ── 5: podSelector mismatch → bypass ──────────────────────────────────────────
bold "── 5. podSelector mismatch → bypass"
cleanup_policies
apply_policy '
  podSelector:
    matchLabels:
      app: non-existent-service
  rules:
    - host: "*"
      port: 80
      methods: [GET]
      ttl: 60s'

# Give the watcher time to pick up the new policy.
sleep 3
if result=$(xcache "$POD" "${BASE}/static/styles.css") && [[ "$result" == "NONE" ]]; then
  pass "policy with non-matching podSelector does not cache"
else
  fail "expected bypass when podSelector does not match, got: $result"
fi
echo ""

# ── 6: podSelector match → caching resumes ────────────────────────────────────
bold "── 6. podSelector match → caching resumes"
cleanup_policies
apply_policy "
  podSelector:
    matchLabels:
      app.kubernetes.io/component: curl-client
  rules:
    - host: \"*\"
      port: 80
      methods: [GET]
      ttl: 60s"

wait_xcache "$POD" "${BASE}/static/app.js" "MISS" 15 || true

if result=$(prime "$POD" "${BASE}/static/app.js") && [[ "$result" == "HIT" ]]; then
  pass "policy with matching podSelector caches correctly"
else
  fail "expected HIT with matching selector, got: $result"
fi
echo ""

# ── 7: TTL expiry ─────────────────────────────────────────────────────────────
bold "── 7. TTL expiry — entry expires after 5 s"
cleanup_policies
apply_policy '
  podSelector: {}
  rules:
    - host: "*"
      port: 80
      methods: [GET]
      ttl: 5s'

# Use "/" — a fresh path not cached in any previous test.
wait_xcache "$POD" "${BASE}/" "MISS" 15 || true
xcache "$POD" "${BASE}/" > /dev/null   # store with 5 s TTL

echo "   waiting 7s for TTL to expire..."
sleep 7

if result=$(xcache "$POD" "${BASE}/") && [[ "$result" == "MISS" ]]; then
  pass "entry expired correctly after 5 s TTL"
else
  fail "expected MISS after TTL expiry, got: $result"
fi
echo ""

# ── cleanup ────────────────────────────────────────────────────────────────────
cleanup_policies

bold "── done"
echo ""
if [[ $FAILED -eq 0 ]]; then
  green "all tests passed"
  exit 0
else
  red "one or more tests failed"
  exit 1
fi
