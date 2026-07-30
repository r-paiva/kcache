// SPDX-FileCopyrightText: Copyright (c) 2026, the latch developers
//
// SPDX-License-Identifier: Apache-2.0

package proxy_test

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codeberg.org/latch/latch/internal/cache"
	"codeberg.org/latch/latch/internal/policy"
	"codeberg.org/latch/latch/internal/proxy"
)

// mockOrigDst always resolves to the given address, simulating BPF map lookup
func mockOrigDst(addr net.Addr) proxy.OrigDstFunc {
	tcp := addr.(*net.TCPAddr)
	return func(_ net.Conn) (net.IP, uint16, error) {
		return tcp.IP, uint16(tcp.Port), nil
	}
}

// startProxy creates a proxy, starts it on a random port, and returns its address.
func startProxy(t *testing.T, c cache.Cache, pol *policy.Policy, upstream *httptest.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := proxy.New(c, pol, mockOrigDst(upstream.Listener.Addr()), nil, 1<<20, nil)
	go p.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

// doRequest sends a raw HTTP/1.1 request over a fresh TCP connection and returns the response.
func doRequest(t *testing.T, proxyAddr, method, path, host string) *http.Response {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req, _ := http.NewRequest(method, "http://"+host+path, nil)
	req.Header.Set("Connection", "close")
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body) //nolint:errcheck
	return resp
}

func TestCacheMissThenHit(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"value":1}`))
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Minute}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	resp1 := doRequest(t, proxyAddr, "GET", "/api/data", "example.com")
	if resp1.StatusCode != 200 {
		t.Fatalf("miss: expected 200, got %d", resp1.StatusCode)
	}
	if resp1.Header.Get("X-Cache") == "HIT" {
		t.Fatal("first request should not be a cache hit")
	}

	resp2 := doRequest(t, proxyAddr, "GET", "/api/data", "example.com")
	if resp2.StatusCode != 200 {
		t.Fatalf("hit: expected 200, got %d", resp2.StatusCode)
	}
	if resp2.Header.Get("X-Cache") != "HIT" {
		t.Fatal("second request should be a cache hit")
	}

	if calls != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", calls)
	}
}

func TestDifferentPathsDifferentCacheEntries(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(r.URL.Path))
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Minute}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	doRequest(t, proxyAddr, "GET", "/a", "example.com")
	doRequest(t, proxyAddr, "GET", "/b", "example.com")

	if calls != 2 {
		t.Fatalf("expected 2 upstream calls for different paths, got %d", calls)
	}
}

func TestNon2xxResponseNotCached(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Minute}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	doRequest(t, proxyAddr, "GET", "/missing", "example.com")
	doRequest(t, proxyAddr, "GET", "/missing", "example.com")

	if calls != 2 {
		t.Fatalf("404 response should not be cached; expected 2 upstream calls, got %d", calls)
	}
}

func TestBodyTooLargeNotCached(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("this response is definitely over ten bytes long"))
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Minute}})
	c := cache.New(256<<20, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// maxBodyBytes=10, response is larger — should never be cached.
	p := proxy.New(c, pol, mockOrigDst(upstream.Listener.Addr()), nil, 10, nil)
	go p.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	proxyAddr := ln.Addr().String()

	resp1 := doRequest(t, proxyAddr, "GET", "/big", "example.com")
	if resp1.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp1.StatusCode)
	}
	doRequest(t, proxyAddr, "GET", "/big", "example.com")

	if calls != 2 {
		t.Fatalf("oversized response should not be cached; expected 2 upstream calls, got %d", calls)
	}
	if c.Len() != 0 {
		t.Fatalf("cache should be empty, got %d entries", c.Len())
	}
}

func TestNoPolicyMatchForwardsTransparently(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// Policy matches a different port — upstream port won't match.
	pol := policy.New([]policy.Rule{{Host: "*", Port: 9999, TTL: time.Minute}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	doRequest(t, proxyAddr, "GET", "/", "example.com")
	doRequest(t, proxyAddr, "GET", "/", "example.com")

	if calls != 2 {
		t.Fatalf("no policy match should forward both requests, got %d upstream calls", calls)
	}
}

// ── additional helpers ────────────────────────────────────────────────────────

// doPost sends a POST with body over a fresh TCP connection and discards the response body.
func doPost(t *testing.T, proxyAddr, path, host string, body []byte) *http.Response {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req, _ := http.NewRequest("POST", "http://"+host+path, bytes.NewReader(body))
	req.Header.Set("Connection", "close")
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body) //nolint:errcheck
	return resp
}

// doGetWithHeader sends a GET with one extra header and discards the response body.
func doGetWithHeader(t *testing.T, proxyAddr, path, host, headerName, headerVal string) *http.Response {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req, _ := http.NewRequest("GET", "http://"+host+path, nil)
	req.Header.Set("Connection", "close")
	req.Header.Set(headerName, headerVal)
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body) //nolint:errcheck
	return resp
}

// doGetWithHeaderReadBody sends a GET with one extra header and returns the response and body.
func doGetWithHeaderReadBody(t *testing.T, proxyAddr, path, host, headerName, headerVal string) (*http.Response, []byte) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req, _ := http.NewRequest("GET", "http://"+host+path, nil)
	req.Header.Set("Connection", "close")
	req.Header.Set(headerName, headerVal)
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

// doGetReadBody sends a GET and returns the response together with the body bytes.
func doGetReadBody(t *testing.T, proxyAddr, path, host string) (*http.Response, []byte) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req, _ := http.NewRequest("GET", "http://"+host+path, nil)
	req.Header.Set("Connection", "close")
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

// ── new tests ─────────────────────────────────────────────────────────────────

func TestKeepAlive(t *testing.T) {
	// Two sequential requests over a single persistent TCP connection.
	// The second request to the same path should be served from cache.
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok")) //nolint:errcheck
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Minute}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)

	// send writes a request and reads the response over the shared connection.
	send := func(path string) *http.Response {
		req, _ := http.NewRequest("GET", "http://example.com"+path, nil)
		if err := req.Write(conn); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body) //nolint:errcheck
		resp.Body.Close()
		return resp
	}

	resp1 := send("/a") // miss
	resp2 := send("/a") // hit — same path, same connection
	resp3 := send("/b") // miss — different path

	if resp1.Header.Get("X-Cache") == "HIT" {
		t.Fatal("first request should not be a cache hit")
	}
	if resp2.Header.Get("X-Cache") != "HIT" {
		t.Fatal("second request over keep-alive connection should be a cache hit")
	}
	if resp3.Header.Get("X-Cache") == "HIT" {
		t.Fatal("request to a different path should not be a cache hit")
	}
	if calls != 2 {
		t.Fatalf("expected 2 upstream calls (two unique paths), got %d", calls)
	}
}

func TestTTLExpiry(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: 50 * time.Millisecond}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	doRequest(t, proxyAddr, "GET", "/ttl", "example.com") // miss — cached for 50 ms
	time.Sleep(100 * time.Millisecond)                    // wait for TTL to lapse
	doRequest(t, proxyAddr, "GET", "/ttl", "example.com") // expired → miss again

	if calls != 2 {
		t.Fatalf("expected 2 upstream calls after TTL expiry, got %d", calls)
	}
}

func TestUpstreamUnreachable(t *testing.T) {
	// Grab a free port, stop listening, then point the proxy at that dead address.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().(*net.TCPAddr)
	ln.Close()

	pol := policy.New([]policy.Rule{{Host: "*", Port: uint16(deadAddr.Port), TTL: time.Minute}})
	c := cache.New(256<<20, nil)

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := proxy.New(c, pol, mockOrigDst(deadAddr), nil, 0, nil)
	go p.Serve(proxyLn) //nolint:errcheck
	t.Cleanup(func() { proxyLn.Close() })

	resp := doRequest(t, proxyLn.Addr().String(), "GET", "/", "example.com")
	if resp.StatusCode != 502 {
		t.Fatalf("expected 502 Bad Gateway for unreachable upstream, got %d", resp.StatusCode)
	}
}

func TestRequestBodyTooLargeForwarded(t *testing.T) {
	// When the request body exceeds maxBodyBytes the proxy must still stream
	// the full body to upstream and must not cache the response.
	var receivedBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Minute, CacheBody: true}})
	c := cache.New(256<<20, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	const bigBody = "hello world!!" // 13 bytes — exceeds maxBodyBytes=5
	p := proxy.New(c, pol, mockOrigDst(upstream.Listener.Addr()), nil, 5, nil)
	go p.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	proxyAddr := ln.Addr().String()

	resp1 := doPost(t, proxyAddr, "/api", "example.com", []byte(bigBody))
	if resp1.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp1.StatusCode)
	}
	if string(receivedBody) != bigBody {
		t.Fatalf("upstream received %q, want %q", receivedBody, bigBody)
	}

	// Same request again — must not be served from cache.
	resp2 := doPost(t, proxyAddr, "/api", "example.com", []byte(bigBody))
	if resp2.Header.Get("X-Cache") == "HIT" {
		t.Fatal("oversized request body should never be cached")
	}
	if c.Len() != 0 {
		t.Fatalf("cache should be empty, got %d entries", c.Len())
	}
}

func TestPOSTBodyInCacheKey(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Minute, CacheBody: true}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	// Two POSTs with the same body — second should hit cache.
	doPost(t, proxyAddr, "/api", "example.com", []byte(`{"x":1}`))
	r2 := doPost(t, proxyAddr, "/api", "example.com", []byte(`{"x":1}`))
	if r2.Header.Get("X-Cache") != "HIT" {
		t.Fatal("identical POST body should produce a cache hit on second request")
	}

	// Different body — must miss.
	r3 := doPost(t, proxyAddr, "/api", "example.com", []byte(`{"x":2}`))
	if r3.Header.Get("X-Cache") == "HIT" {
		t.Fatal("different POST body should not be a cache hit")
	}

	if calls != 2 { // {"x":1} miss + {"x":2} miss = 2; {"x":1} second = cache hit
		t.Fatalf("expected 2 upstream calls, got %d", calls)
	}
}

func TestOversizedResponseBodyDelivered(t *testing.T) {
	// Even when the response body is too large to cache it must still be
	// delivered in full to the client (tests the streamingBody path).
	const wantBody = "this response is definitely over ten bytes long"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(wantBody)) //nolint:errcheck
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Minute}})
	c := cache.New(256<<20, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := proxy.New(c, pol, mockOrigDst(upstream.Listener.Addr()), nil, 10, nil)
	go p.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })

	_, gotBody := doGetReadBody(t, ln.Addr().String(), "/big", "example.com")
	if string(gotBody) != wantBody {
		t.Fatalf("streaming body: got %q, want %q", gotBody, wantBody)
	}
}

func TestUpstreamVaryAutoPartition(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Vary", "Accept-Encoding")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(r.Header.Get("Accept-Encoding"))) //nolint:errcheck
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Minute}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	doGetWithHeader(t, proxyAddr, "/data", "example.com", "Accept-Encoding", "gzip")
	r2 := doGetWithHeader(t, proxyAddr, "/data", "example.com", "Accept-Encoding", "gzip")
	if r2.Header.Get("X-Cache") != "HIT" {
		t.Fatal("second request with same Accept-Encoding should be a cache hit")
	}
	doGetWithHeader(t, proxyAddr, "/data", "example.com", "Accept-Encoding", "identity")
	if calls != 2 {
		t.Fatalf("expected 2 upstream calls (gzip + identity), got %d", calls)
	}
}

func TestVaryStarSkipCache(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Vary", "*")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Minute}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	doRequest(t, proxyAddr, "GET", "/dynamic", "example.com")
	doRequest(t, proxyAddr, "GET", "/dynamic", "example.com")
	if calls != 2 {
		t.Fatalf("Vary: * should never be cached; expected 2 upstream calls, got %d", calls)
	}
	if c.Len() != 0 {
		t.Fatalf("cache should be empty after Vary: * responses, got %d entries", c.Len())
	}
}

func TestVaryHeadersPartitionCache(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{
		Host:        "*",
		Port:        upstreamPort,
		TTL:         time.Minute,
		VaryHeaders: []string{"Authorization"},
	}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	doGetWithHeader(t, proxyAddr, "/api", "example.com", "Authorization", "Bearer a")
	doGetWithHeader(t, proxyAddr, "/api", "example.com", "Authorization", "Bearer b")
	if calls != 2 {
		t.Fatalf("different Authorization values must produce separate cache entries; got %d upstream calls", calls)
	}

	// Same token as first request — must be a cache hit now.
	r := doGetWithHeader(t, proxyAddr, "/api", "example.com", "Authorization", "Bearer a")
	if r.Header.Get("X-Cache") != "HIT" {
		t.Fatal("repeated Authorization value should be a cache hit")
	}
	if calls != 2 {
		t.Fatalf("third request should not have reached upstream; calls: %d", calls)
	}
}

func TestVaryBodyCorrectnessPerVariant(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "Accept-Encoding")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("encoding:" + r.Header.Get("Accept-Encoding"))) //nolint:errcheck
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Minute}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	_, body1 := doGetWithHeaderReadBody(t, proxyAddr, "/data", "example.com", "Accept-Encoding", "gzip")
	_, body2 := doGetWithHeaderReadBody(t, proxyAddr, "/data", "example.com", "Accept-Encoding", "identity")

	r3, body3 := doGetWithHeaderReadBody(t, proxyAddr, "/data", "example.com", "Accept-Encoding", "gzip")
	r4, body4 := doGetWithHeaderReadBody(t, proxyAddr, "/data", "example.com", "Accept-Encoding", "identity")

	if r3.Header.Get("X-Cache") != "HIT" {
		t.Fatal("gzip variant second request should be a cache hit")
	}
	if r4.Header.Get("X-Cache") != "HIT" {
		t.Fatal("identity variant second request should be a cache hit")
	}
	if string(body3) != string(body1) {
		t.Fatalf("gzip cache hit: got body %q, want %q", body3, body1)
	}
	if string(body4) != string(body2) {
		t.Fatalf("identity cache hit: got body %q, want %q", body4, body2)
	}
}

func TestPolicyAndUpstreamVaryMerge(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Vary", "Accept-Encoding")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{
		Host:        "*",
		Port:        upstreamPort,
		TTL:         time.Minute,
		VaryHeaders: []string{"Authorization"},
	}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	// Three distinct combinations of the two vary dimensions.
	send := func(auth, enc string) *http.Response {
		conn, err := net.Dial("tcp", proxyAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		req, _ := http.NewRequest("GET", "http://example.com/api", nil)
		req.Header.Set("Connection", "close")
		req.Header.Set("Authorization", auth)
		req.Header.Set("Accept-Encoding", enc)
		if err := req.Write(conn); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), req)
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body) //nolint:errcheck
		resp.Body.Close()
		return resp
	}

	send("Bearer a", "gzip")     // miss
	send("Bearer a", "identity") // miss — different Accept-Encoding
	send("Bearer b", "gzip")     // miss — different Authorization
	if calls != 3 {
		t.Fatalf("expected 3 upstream calls for 3 unique variants, got %d", calls)
	}

	r := send("Bearer a", "gzip") // should hit
	if r.Header.Get("X-Cache") != "HIT" {
		t.Fatal("repeated (auth, encoding) pair should be a cache hit")
	}
	if calls != 3 {
		t.Fatalf("fourth request should not have reached upstream; calls: %d", calls)
	}
}

func TestResponseNoStoreNotCached(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Minute}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	doRequest(t, proxyAddr, "GET", "/x", "example.com")
	doRequest(t, proxyAddr, "GET", "/x", "example.com")

	if calls != 2 {
		t.Fatalf("Cache-Control: no-store response must not be cached; expected 2 calls, got %d", calls)
	}
}

func TestResponsePrivateNotCached(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Cache-Control", "private, max-age=60")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Minute}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	doRequest(t, proxyAddr, "GET", "/x", "example.com")
	doRequest(t, proxyAddr, "GET", "/x", "example.com")

	if calls != 2 {
		t.Fatalf("Cache-Control: private response must not be cached; expected 2 calls, got %d", calls)
	}
}

func TestResponseMaxAgeOverridesPolicyTTL(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Cache-Control", "max-age=0")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	// Long policy TTL, but the response caps freshness at 0s, so the second
	// request must re-fetch rather than hit the cache.
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Hour}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	doRequest(t, proxyAddr, "GET", "/x", "example.com")
	time.Sleep(20 * time.Millisecond)
	doRequest(t, proxyAddr, "GET", "/x", "example.com")

	if calls != 2 {
		t.Fatalf("max-age=0 must override policy TTL; expected 2 calls, got %d", calls)
	}
}

func TestResponseSMaxAgePreferredOverMaxAge(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// max-age would keep it fresh, but s-maxage=0 takes precedence.
		w.Header().Set("Cache-Control", "max-age=3600, s-maxage=0")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Hour}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	doRequest(t, proxyAddr, "GET", "/x", "example.com")
	time.Sleep(20 * time.Millisecond)
	doRequest(t, proxyAddr, "GET", "/x", "example.com")

	if calls != 2 {
		t.Fatalf("s-maxage=0 must take precedence over max-age; expected 2 calls, got %d", calls)
	}
}

func TestRequestNoCacheBypassesLookup(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Minute}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	doRequest(t, proxyAddr, "GET", "/x", "example.com")                                     // populates cache
	resp := doGetWithHeader(t, proxyAddr, "/x", "example.com", "Cache-Control", "no-cache") // must skip the hit
	if resp.Header.Get("X-Cache") == "HIT" {
		t.Fatal("Cache-Control: no-cache request must not be served from cache")
	}
	if calls != 2 {
		t.Fatalf("no-cache must force revalidation; expected 2 calls, got %d", calls)
	}

	// The no-cache response is still storable, so a plain request now hits.
	if r := doRequest(t, proxyAddr, "GET", "/x", "example.com"); r.Header.Get("X-Cache") != "HIT" {
		t.Fatalf("subsequent plain request should hit; calls: %d", calls)
	}
}

func TestRequestNoStoreBypassesAndDoesNotStore(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamPort := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port)
	pol := policy.New([]policy.Rule{{Host: "*", Port: upstreamPort, TTL: time.Minute}})
	c := cache.New(256<<20, nil)
	proxyAddr := startProxy(t, c, pol, upstream)

	doGetWithHeader(t, proxyAddr, "/x", "example.com", "Cache-Control", "no-store")
	// Nothing should have been stored, so a follow-up plain request misses too.
	if r := doRequest(t, proxyAddr, "GET", "/x", "example.com"); r.Header.Get("X-Cache") == "HIT" {
		t.Fatal("no-store request response must not be stored")
	}
	if calls != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", calls)
	}
}
