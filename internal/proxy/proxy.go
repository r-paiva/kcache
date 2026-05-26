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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"kache/internal/cache"
	"kache/internal/cachekey"
	"kache/internal/metrics"
	"kache/internal/policy"
)

type OrigDstFunc func(conn net.Conn) (ip net.IP, port uint16, err error)

type NamespaceFunc func(podIP string) (namespace string, podLabels map[string]string)

type Proxy struct {
	cache        cache.Cache
	mu           sync.RWMutex
	policy       *policy.Policy
	origDst      OrigDstFunc
	namespaceFn  NamespaceFunc
	dialTimeout  time.Duration
	maxBodyBytes int64
	varyIndex    sync.Map // base key → []string of Vary header names seen from upstream
}

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

	path := req.URL.Path
	if path == "" {
		path = "/"
	}

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
		baseKey := cachekey.Generate(req, nil, cachekey.Config{})
		keyCfg := rule.KeyConfig()
		keyCfg.VaryHeaders = mergeVaryHeaders(keyCfg.VaryHeaders, p.lookupVaryIndex(baseKey))
		key := cachekey.Generate(req, body, keyCfg)

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
			varyHdr := respEntry.Header.Get("Vary")
			if varyHdr == "*" {
				metrics.Requests.WithLabelValues(host, req.Method, "bypass", path).Inc()
				resp.Write(w)     //nolint:errcheck
				resp.Body.Close() //nolint:errcheck
				return
			}
			allVary := mergeVaryHeaders(rule.KeyConfig().VaryHeaders, parseVaryHeader(varyHdr))
			p.storeVaryIndex(baseKey, allVary)
			keyCfg.VaryHeaders = allVary
			storeKey := cachekey.Generate(req, body, keyCfg)
			if storeKey != key {
				p.cache.Delete(key)
			}
			respEntry.TTL = rule.TTL
			p.cache.Set(storeKey, respEntry)
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
		resp.Write(w)     //nolint:errcheck
		resp.Body.Close() //nolint:errcheck
		return
	}

	slog.Debug("no policy match, forwarding transparently",
		"host", host, "method", req.Method, "path", req.URL.RequestURI())
	_, resp := p.fetchUpstream(req, body, origAddr)
	if resp == nil {
		metrics.Requests.WithLabelValues(host, req.Method, "error", path).Inc()
		writeBadGateway(w)
		return
	}
	metrics.Requests.WithLabelValues(host, req.Method, "bypass", path).Inc()
	resp.Write(w)     //nolint:errcheck
	resp.Body.Close() //nolint:errcheck
}

func (p *Proxy) fetchUpstream(req *http.Request, body []byte, addr string) (*cache.Entry, *http.Response) {
	host := req.Host
	start := time.Now()

	dialer := net.Dialer{Timeout: p.dialTimeout}
	upstream, err := dialer.Dial("tcp", addr)
	if err != nil {
		slog.Error("dial upstream", "addr", addr, "err", err)
		return nil, nil
	}

	if body != nil {
		req.Body = io.NopCloser(bytes.NewReader(body))
	} else if req.Body == nil {
		req.Body = http.NoBody
	}

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

type streamingBody struct {
	io.Reader
	closer io.Closer
}

func (s *streamingBody) Close() error { return s.closer.Close() }

func (p *Proxy) lookupVaryIndex(baseKey string) []string {
	if v, ok := p.varyIndex.Load(baseKey); ok {
		return v.([]string)
	}
	return nil
}

func (p *Proxy) storeVaryIndex(baseKey string, headers []string) {
	if len(headers) > 0 {
		p.varyIndex.Store(baseKey, headers)
	}
}

func parseVaryHeader(vary string) []string {
	if vary == "" || vary == "*" {
		return nil
	}
	parts := strings.Split(vary, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if h := strings.TrimSpace(p); h != "" {
			out = append(out, http.CanonicalHeaderKey(h))
		}
	}
	return out
}

func mergeVaryHeaders(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, h := range a {
		seen[http.CanonicalHeaderKey(h)] = struct{}{}
	}
	for _, h := range b {
		seen[http.CanonicalHeaderKey(h)] = struct{}{}
	}
	merged := make([]string, 0, len(seen))
	for h := range seen {
		merged = append(merged, h)
	}
	sort.Strings(merged)
	return merged
}

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
