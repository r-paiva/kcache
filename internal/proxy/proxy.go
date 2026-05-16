// SPDX-FileCopyrightText: Copyright (c) 2026, the k-cache developers
//
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"kache/internal/cache"
	"kache/internal/cachekey"
	"kache/internal/metrics"
	"kache/internal/policy"
)

// OrigDstFunc resolves the original destination for a BPF-redirected connection.
// In production this reads from the BPF port_orig_dst map.
// In tests a simple mock is injected instead.
type OrigDstFunc func(conn net.Conn) (ip net.IP, port uint16, err error)

// NamespaceFunc resolves the Kubernetes namespace and pod labels for a source IP.
// Returns empty strings/nil when running outside Kubernetes or pod is unknown.
type NamespaceFunc func(podIP string) (namespace string, podLabels map[string]string)

type Proxy struct {
	cache        cache.Cache
	mu           sync.RWMutex
	policy       *policy.Policy
	origDst      OrigDstFunc
	namespaceFn  NamespaceFunc
	dialTimeout  time.Duration
	maxBodyBytes int64 // 0 means unlimited
}

// New creates a Proxy. maxBodyBytes is the largest response body that will be cached; 0 disables the limit.
func New(c cache.Cache, p *policy.Policy, origDst OrigDstFunc, namespaceFn NamespaceFunc, maxBodyBytes int64) *Proxy {
	return &Proxy{
		cache:        c,
		policy:       p,
		origDst:      origDst,
		namespaceFn:  namespaceFn,
		dialTimeout:  10 * time.Second,
		maxBodyBytes: maxBodyBytes,
	}
}

// SetPolicy replaces the active policy atomically. Safe to call concurrently.
func (p *Proxy) SetPolicy(pol *policy.Policy) {
	p.mu.Lock()
	p.policy = pol
	p.mu.Unlock()
}

func (p *Proxy) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return p.Serve(ln)
}

func (p *Proxy) Serve(ln net.Listener) error {
	slog.Info("proxy listening", "addr", ln.Addr())
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		metrics.BPFRedirects.Inc()
		go p.handleConn(conn)
	}
}

func (p *Proxy) handleConn(conn net.Conn) {
	defer conn.Close()

	origIP, origPort, err := p.origDst(conn)
	if err != nil {
		slog.Error("orig dst lookup", "remote", conn.RemoteAddr(), "err", err)
		return
	}

	// Resolve namespace and pod labels from the source IP once per connection.
	var namespace string
	var podLabels map[string]string
	if p.namespaceFn != nil {
		if srcTCP, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
			namespace, podLabels = p.namespaceFn(srcTCP.IP.String())
		}
	}

	origAddr := net.JoinHostPort(origIP.String(), strconv.Itoa(int(origPort)))
	br := bufio.NewReader(conn)

	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "connection reset") {
				slog.Debug("read request", "err", err)
			}
			return
		}

		keepAlive := req.ProtoAtLeast(1, 1) && !strings.EqualFold(req.Header.Get("Connection"), "close")
		p.handleRequest(req, origAddr, origPort, namespace, podLabels, conn)

		if !keepAlive {
			return
		}
	}
}

func (p *Proxy) handleRequest(req *http.Request, origAddr string, origPort uint16, namespace string, podLabels map[string]string, w io.Writer) {
	host := req.Host
	if host == "" {
		host = origAddr
	}

	// Normalise path for metric labels: use the URL path only, no query string.
	// Falls back to "/" so the label is never empty.
	path := req.URL.Path
	if path == "" {
		path = "/"
	}

	// Read the request body so we can hash it for the cache key and replay it upstream.
	// If maxBodyBytes is set, cap the read: on overflow, stitch the partial read back
	// with the remaining stream so the upstream still receives the full body.
	var body []byte
	var reqBodyTooLarge bool
	if req.Body != nil {
		origBody := req.Body
		if p.maxBodyBytes > 0 {
			lr := &io.LimitedReader{R: origBody, N: p.maxBodyBytes + 1}
			partial, _ := io.ReadAll(lr)
			if lr.N == 0 {
				// Body exceeded limit; reassemble for streaming to upstream.
				req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(partial), origBody))
				reqBodyTooLarge = true
				metrics.CacheSkipsRequestBodyTooLarge.WithLabelValues(host).Inc()
				slog.Debug("request body too large to buffer, forwarding without caching",
					"host", host, "method", req.Method, "limit", p.maxBodyBytes)
			} else {
				body = partial
				origBody.Close()
				req.Body = nil
			}
		} else {
			body, _ = io.ReadAll(origBody)
			origBody.Close()
			req.Body = nil
		}
	}

	if len(body) > 0 {
		metrics.RequestSize.WithLabelValues(host, req.Method).Observe(float64(len(body)))
	}

	p.mu.RLock()
	pol := p.policy
	p.mu.RUnlock()
	rule := pol.Match(namespace, podLabels, host, origPort, req.Method, path)

	if rule != nil && !reqBodyTooLarge {
		key := cachekey.Generate(req, body, rule.KeyConfig())
		if entry, ok := p.cache.Get(key); ok {
			start := time.Now()
			writeEntry(w, entry)
			elapsed := time.Since(start)
			metrics.HitLatency.WithLabelValues(host, path).Observe(elapsed.Seconds())
			metrics.Requests.WithLabelValues(host, req.Method, "hit", path).Inc()
			metrics.ResponseSize.WithLabelValues(host, req.Method, path).Observe(float64(len(entry.Body)))
			slog.Debug("cache hit",
				"host", host, "method", req.Method, "path", req.URL.RequestURI(),
				"size", len(entry.Body), "latency", elapsed)
			return
		}

		slog.Debug("cache miss",
			"host", host, "method", req.Method, "path", req.URL.RequestURI(),
			"upstream", origAddr)

		respEntry, resp := p.fetchUpstream(req, body, origAddr)
		if resp == nil {
			metrics.Requests.WithLabelValues(host, req.Method, "error", path).Inc()
			writeBadGateway(w)
			return
		}
		if respEntry != nil {
			respEntry.TTL = rule.TTL
			p.cache.Set(key, respEntry)
			metrics.ResponseSize.WithLabelValues(host, req.Method, path).Observe(float64(len(respEntry.Body)))
			metrics.Requests.WithLabelValues(host, req.Method, "miss", path).Inc()
			resp.Header.Set("X-Cache", "MISS")
			slog.Debug("stored in cache",
				"host", host, "method", req.Method, "path", req.URL.RequestURI(),
				"status", resp.StatusCode, "size", len(respEntry.Body), "ttl", rule.TTL)
		} else {
			metrics.Requests.WithLabelValues(host, req.Method, "bypass", path).Inc()
			slog.Debug("upstream non-2xx, not cached",
				"host", host, "method", req.Method, "status", resp.StatusCode)
		}
		resp.Write(w)         //nolint:errcheck
		resp.Body.Close()     //nolint:errcheck
		return
	}

	// No matching rule: forward transparently without caching.
	slog.Debug("no policy match, forwarding transparently",
		"host", host, "method", req.Method, "path", req.URL.RequestURI())
	_, resp := p.fetchUpstream(req, body, origAddr)
	if resp == nil {
		metrics.Requests.WithLabelValues(host, req.Method, "error", path).Inc()
		writeBadGateway(w)
		return
	}
	metrics.Requests.WithLabelValues(host, req.Method, "bypass", path).Inc()
	resp.Write(w)         //nolint:errcheck
	resp.Body.Close()     //nolint:errcheck
}

// fetchUpstream dials the upstream, sends the request, reads the full response.
// Returns (entry, resp): entry is non-nil only for 2xx responses worth caching.
// When the response body exceeds maxBodyBytes the body is streamed; the caller
// must call resp.Body.Close() after consuming the response.
func (p *Proxy) fetchUpstream(req *http.Request, body []byte, addr string) (*cache.Entry, *http.Response) {
	host := req.Host
	start := time.Now()

	dialer := net.Dialer{Timeout: p.dialTimeout}
	upstream, err := dialer.Dial("tcp", addr)
	if err != nil {
		slog.Error("dial upstream", "addr", addr, "err", err)
		return nil, nil
	}

	// body non-nil: already buffered (small body or GET).
	// body nil + req.Body non-nil: streaming (request body exceeded buffer limit).
	// body nil + req.Body nil: no body (e.g. GET).
	if body != nil {
		req.Body = io.NopCloser(bytes.NewReader(body))
	} else if req.Body == nil {
		req.Body = http.NoBody
	}
	// Prevent upstream from keeping the connection open indefinitely.
	req.Header.Set("Connection", "close")
	if err := req.Write(upstream); err != nil {
		upstream.Close()
		slog.Error("write upstream request", "err", err)
		return nil, nil
	}

	resp, err := http.ReadResponse(bufio.NewReader(upstream), req)
	if err != nil {
		upstream.Close()
		slog.Error("read upstream response", "err", err)
		return nil, nil
	}

	urlPath := req.URL.Path
	if urlPath == "" {
		urlPath = "/"
	}
	metrics.UpstreamLatency.WithLabelValues(host, urlPath).Observe(time.Since(start).Seconds())

	// Read the response body. When maxBodyBytes is set, stop at the limit so we
	// never allocate an unbounded buffer for a single response.
	if p.maxBodyBytes > 0 {
		lr := &io.LimitedReader{R: resp.Body, N: p.maxBodyBytes + 1}
		partial, readErr := io.ReadAll(lr)
		if readErr != nil {
			resp.Body.Close()
			upstream.Close()
			slog.Error("read upstream body", "err", readErr)
			return nil, nil
		}
		if lr.N == 0 {
			// Body exceeded the limit. Stream the remainder from the live upstream
			// connection; ownership of upstream transfers to the caller via resp.Body.
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				metrics.CacheSkipsBodyTooLarge.WithLabelValues(host).Inc()
				slog.Debug("response too large to cache",
					"host", host, "limit", p.maxBodyBytes)
			}
			resp.Body = &streamingBody{
				Reader: io.MultiReader(bytes.NewReader(partial), resp.Body),
				closer: upstream,
			}
			return nil, resp
		}
		// Body fits within the limit; upstream is no longer needed.
		resp.Body.Close()
		upstream.Close()
		resp.Body = io.NopCloser(bytes.NewReader(partial))
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return &cache.Entry{
				StatusCode: resp.StatusCode,
				Header:     resp.Header.Clone(),
				Body:       partial,
				CachedAt:   time.Now(),
			}, resp
		}
		return nil, resp
	}

	// No size limit: buffer everything.
	respBody, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	upstream.Close()
	if readErr != nil {
		slog.Error("read upstream body", "err", readErr)
		return nil, nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &cache.Entry{
			StatusCode: resp.StatusCode,
			Header:     resp.Header.Clone(),
			Body:       respBody,
			CachedAt:   time.Now(),
		}, resp
	}
	return nil, resp
}

// streamingBody pairs a live upstream connection with the Reader draining it.
// Close() tears down the connection once the caller has consumed the body.
type streamingBody struct {
	io.Reader
	closer io.Closer
}

func (s *streamingBody) Close() error { return s.closer.Close() }

func writeEntry(w io.Writer, e *cache.Entry) {
	resp := &http.Response{
		StatusCode:    e.StatusCode,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        e.Header.Clone(),
		Body:          io.NopCloser(bytes.NewReader(e.Body)),
		ContentLength: int64(len(e.Body)),
	}
	resp.Header.Set("X-Cache", "HIT")
	resp.Write(w)
}

func writeBadGateway(w io.Writer) {
	body := []byte("bad gateway\n")
	fmt.Fprintf(w, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: %d\r\nContent-Type: text/plain\r\n\r\n%s",
		len(body), body)
}
