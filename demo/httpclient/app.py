#!/usr/bin/env python3

# SPDX-FileCopyrightText: 2026 Copyright (c) 2026, the k-cache developers
#
# SPDX-License-Identifier: Apache-2.0

"""
kcache canary — quiet background validator. Two check schedules:

  Local  (default every 10 min): HTTP + HTTPS requests to the in-cluster
    nginx backend, 2 hits per endpoint to confirm MISS then HIT.

  External (default every 1 hour): real public HTTPS URLs through kcache's
    TLS MITM, 2 hits each. An SSL failure here means the MITM cert isn't
    trusted — i.e. the CA isn't mounted or the cert pipeline is broken.
    Only ~4 req/hour total — not spammy.

Any wrong status, bad JSON, failed validator, or SSL error → exit(1)
→ CrashLoopBackOff → immediately visible.
"""
import http.client
import json
import os
import random
import ssl
import sys
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlparse
from urllib.request import Request, urlopen

HTTP_BASE         = os.environ.get("HTTP_BASE",          "http://kcache-kcache-demo-backend")
HTTPS_BASE        = os.environ.get("HTTPS_BASE",         "https://kcache-kcache-demo-backend")
LOCAL_INTERVAL    = float(os.environ.get("LOCAL_INTERVAL",    "600"))   # 10 min
EXTERNAL_INTERVAL = float(os.environ.get("EXTERNAL_INTERVAL", "3600"))  # 1 hour
POLL_SLEEP        = float(os.environ.get("POLL_SLEEP",         "60"))   # main loop tick
CA_BUNDLE         = os.environ.get("REQUESTS_CA_BUNDLE", "")
DEBUG             = os.environ.get("DEBUG", "").lower() in ("1", "true", "yes")

# (path, expected_status, validator)
LOCAL_CHECKS = [
    ("/api/status",
     200,
     lambda d: isinstance(d, dict) and d.get("status") == "ok"),
    ("/api/products",
     200,
     lambda d: isinstance(d, list) and len(d) == 10 and all("name" in p and "price" in p for p in d)),
    ("/api/users",
     200,
     lambda d: isinstance(d, list) and len(d) == 8 and all("role" in u and "region" in u for u in d)),
    ("/api/orders",
     200,
     lambda d: isinstance(d, list) and len(d) == 5 and all("total" in o and "status" in o for o in d)),
]

# Real public HTTPS endpoints. Validators check specific field values so a
# mangled or substituted body is caught.
EXTERNAL_CHECKS = [
    ("https://jsonplaceholder.typicode.com/todos/1",
     200,
     lambda d: (isinstance(d, dict)
                and d.get("id") == 1
                and d.get("userId") == 1
                and isinstance(d.get("title"), str)
                and isinstance(d.get("completed"), bool))),
    ("https://jsonplaceholder.typicode.com/users/1",
     200,
     lambda d: (isinstance(d, dict)
                and d.get("id") == 1
                and d.get("username") == "Bret"
                and "email" in d)),
    ("https://jsonplaceholder.typicode.com/posts/1",
     200,
     lambda d: (isinstance(d, dict)
                and d.get("id") == 1
                and d.get("userId") == 1
                and isinstance(d.get("title"), str)
                and isinstance(d.get("body"), str))),
]

ctx = ssl.create_default_context()
if CA_BUNDLE:
    ctx.load_verify_locations(CA_BUNDLE)

print("kcache canary", flush=True)
print(f"  local http:        {HTTP_BASE}", flush=True)
print(f"  local https:       {HTTPS_BASE}", flush=True)
print(f"  local interval:    every {LOCAL_INTERVAL:.0f}s", flush=True)
print(f"  external interval: every {EXTERNAL_INTERVAL:.0f}s ({len(EXTERNAL_CHECKS)} URLs, 2 hits each)", flush=True)
print(f"  CA:                {CA_BUNDLE or 'system default'}", flush=True)
print(f"  debug:             {'on (TLS cert printed per request)' if DEBUG else 'off (set DEBUG=1 to enable)'}", flush=True)
print(flush=True)


def fetch(url):
    if DEBUG and url.startswith("https://"):
        return _fetch_https_debug(url)
    with urlopen(Request(url), context=ctx, timeout=15) as r:
        return r.status, r.read(), r.headers.get("X-Cache", "-")


def _fetch_https_debug(url):
    parsed = urlparse(url)
    host, port = parsed.hostname, parsed.port or 443
    path = (parsed.path or "/") + (("?" + parsed.query) if parsed.query else "")
    conn = http.client.HTTPSConnection(host, port, context=ctx, timeout=15)
    try:
        conn.connect()
        cert    = conn.sock.getpeercert()
        subject = dict(x[0] for x in cert.get("subject", []))
        issuer  = dict(x[0] for x in cert.get("issuer", []))
        not_after = cert.get("notAfter", "?")
        print(f"  tls  {host}:{port}  "
              f"subject={dict(x[0] for x in cert.get('subject', [])).get('commonName', '?')!r}  "
              f"issuer={issuer.get('commonName', '?')!r}  "
              f"expires={not_after}", flush=True)
        conn.request("GET", path, headers={"Host": parsed.netloc, "Connection": "close"})
        r = conn.getresponse()
        body = r.read()
        x_cache = r.getheader("X-Cache", "-")
        return r.status, body, x_cache
    finally:
        conn.close()


def die(msg):
    print(f"\nFAIL: {msg}", flush=True)
    sys.exit(1)


def tag(x_cache):
    return {"HIT": "HIT ", "MISS": "MISS"}.get(x_cache, x_cache[:4].ljust(4))


def two_hit_check(label, url, expected_status, validate, die_on_network=True):
    """Hit url twice; validate content both times; assert second is HIT."""
    results = []
    for n in range(1, 3):
        try:
            status, body, x_cache = fetch(url)
        except ssl.SSLError as e:
            die(f"[{label}] TLS error (hit {n}): {e}\n"
                f"      MITM cert not trusted — CA not mounted or cert pipeline broken")
        except (HTTPError, URLError, OSError) as e:
            if die_on_network:
                die(f"[{label}] hit {n} → network error: {e}")
            print(f"  warn [{label}]  hit {n} → unreachable: {e}", flush=True)
            return

        if status != expected_status:
            die(f"[{label}] hit {n} → status {status}, want {expected_status}")

        try:
            data = json.loads(body)
        except Exception as e:
            die(f"[{label}] hit {n} → invalid JSON ({e}): {body[:120]!r}")

        if not validate(data):
            die(f"[{label}] hit {n} → validation failed: {str(data)[:120]}")

        results.append(x_cache)
        print(f"  ok  [{label}]  hit {n}  x-cache={tag(x_cache)}  {url}", flush=True)
        if DEBUG:
            print(f"       body: {json.dumps(data, separators=(',', ':'))[:200]}", flush=True)

    if len(results) == 2 and results[1] != "HIT":
        die(f"[{label}] {url} — second hit returned {results[1]!r}, expected HIT")


def run_local():
    print("── local checks", flush=True)
    for path, want_status, validate in LOCAL_CHECKS:
        two_hit_check("http ", HTTP_BASE  + path, want_status, validate, die_on_network=True)
        two_hit_check("https", HTTPS_BASE + path, want_status, validate, die_on_network=True)


def run_external():
    print("── external HTTPS checks", flush=True)
    for url, want_status, validate in EXTERNAL_CHECKS:
        two_hit_check("ext", url, want_status, validate, die_on_network=False)


last_local    = 0.0  # run immediately on first tick
last_external = 0.0

while True:
    now = time.monotonic()

    if now - last_local >= LOCAL_INTERVAL:
        run_local()
        last_local = time.monotonic()

    if now - last_external >= EXTERNAL_INTERVAL:
        run_external()
        last_external = time.monotonic()

    jitter = random.uniform(0, POLL_SLEEP * 0.1)
    time.sleep(POLL_SLEEP + jitter)
