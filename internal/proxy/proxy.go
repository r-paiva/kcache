// SPDX-FileCopyrightText: Copyright (c) 2026, the kcache developers
//
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
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
	"kache/internal/tlsmitm"
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
	ca           *tlsmitm.CA
	upstreamPool *x509.CertPool
}

func New(c cache.Cache, p *policy.Policy, origDst OrigDstFunc, namespaceFn NamespaceFunc, maxBodyBytes int64, ca *tlsmitm.CA) *Proxy {
	var upstreamPool *x509.CertPool
	if ca != nil {
		upstreamPool = ca.CertPool()
	}
	return &Proxy{
		cache:        c,
		policy:       p,
		origDst:      origDst,
		namespaceFn:  namespaceFn,
		dialTimeout:  10 * time.Second,
		maxBodyBytes: maxBodyBytes,
		ca:           ca,
		upstreamPool: upstreamPool,
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

// peekConn wraps a net.Conn so that Read is served from r first (holding bytes
// already peeked from the underlying conn), then falls through to the conn.
type peekConn struct {
	net.Conn
	r io.Reader
}

func (c *peekConn) Read(b []byte) (int, error) { return c.r.Read(b) }

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
	slog.Debug("conn intercepted", "src", conn.RemoteAddr(), "dst", origAddr, "port", origPort)
	br := bufio.NewReader(conn)

	if origPort == 443 {
		p.handleTLSConn(conn, br, origAddr, origPort, namespace, podLabels)
		return
	}

	plainDial := func(addr string) (net.Conn, error) {
		return (&net.Dialer{Timeout: p.dialTimeout}).Dial("tcp", addr)
	}

	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "connection reset") {
				slog.Debug("read request", "err", err)
			}
			return
		}

		keepAlive := req.ProtoAtLeast(1, 1) && !strings.EqualFold(req.Header.Get("Connection"), "close")
		p.handleRequest(req, origAddr, origPort, namespace, podLabels, conn, plainDial)

		if !keepAlive {
			return
		}
	}
}

func (p *Proxy) handleTLSConn(conn net.Conn, br *bufio.Reader, origAddr string, origPort uint16, namespace string, podLabels map[string]string) {
	sni, err := tlsmitm.PeekSNI(br)
	if err != nil {
		slog.Debug("TLS SNI peek failed, splicing", "remote", conn.RemoteAddr(), "err", err)
		p.spliceTo(conn, br, origAddr)
		return
	}

	p.mu.RLock()
	pol := p.policy
	p.mu.RUnlock()

	if p.ca == nil || !pol.HasRuleForHostPort(sni, origPort) {
		slog.Debug("no TLS MITM (no CA or no policy match), splicing",
			"sni", sni, "remote", conn.RemoteAddr())
		p.spliceTo(conn, br, origAddr)
		return
	}

	slog.Debug("TLS MITM: intercepting", "sni", sni, "src", conn.RemoteAddr(), "dst", origAddr)
	tlsCfg := &tls.Config{GetCertificate: p.ca.GetCertificate}
	tlsConn := tls.Server(&peekConn{Conn: conn, r: br}, tlsCfg)
	_ = tlsConn.SetDeadline(time.Now().Add(p.dialTimeout))
	if err := tlsConn.Handshake(); err != nil {
		slog.Debug("TLS handshake failed", "sni", sni, "err", err)
		return
	}
	_ = tlsConn.SetDeadline(time.Time{})
	slog.Debug("TLS MITM: handshake ok", "sni", sni)
	defer tlsConn.Close()

	tlsDial := func(addr string) (net.Conn, error) {
		return tls.DialWithDialer(
			&net.Dialer{Timeout: p.dialTimeout},
			"tcp", addr,
			&tls.Config{
				ServerName: sni,
				RootCAs:    p.upstreamPool,
			},
		)
	}

	tlsBR := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(tlsBR)
		if err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "connection reset") {
				slog.Debug("TLS read request", "sni", sni, "err", err)
			}
			return
		}
		if req.Host == "" {
			req.Host = sni
		}

		keepAlive := req.ProtoAtLeast(1, 1) && !strings.EqualFold(req.Header.Get("Connection"), "close")
		p.handleRequest(req, origAddr, origPort, namespace, podLabels, tlsConn, tlsDial)

		if !keepAlive {
			return
		}
	}
}

func (p *Proxy) spliceTo(conn net.Conn, br *bufio.Reader, origAddr string) {
	upstream, err := (&net.Dialer{Timeout: p.dialTimeout}).Dial("tcp", origAddr)
	if err != nil {
		slog.Debug("splice dial failed", "addr", origAddr, "err", err)
		return
	}
	tlsmitm.Splice(&peekConn{Conn: conn, r: br}, upstream)
}

func (p *Proxy) handleRequest(req *http.Request, origAddr string, origPort uint16, namespace string, podLabels map[string]string, w io.Writer, dial func(string) (net.Conn, error)) {
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
		baseKey := cachekey.Generate(req, nil, cachekey.Config{Port: origPort})
		keyCfg := rule.KeyConfig()
		keyCfg.Port = origPort
		keyCfg.VaryHeaders = mergeVaryHeaders(keyCfg.VaryHeaders, p.lookupVaryIndex(baseKey))
		key := cachekey.Generate(req, body, keyCfg)

		if entry, ok := p.cache.Get(key); ok {
			start := time.Now()
			replyWCachedEntry(w, entry)
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

		cachedResp, resp := p.fetchUpstream(req, body, origAddr, dial)
		if resp == nil {
			metrics.Requests.WithLabelValues(host, req.Method, "error", path).Inc()
			writeBadGateway(w)
			return
		}
		if cachedResp != nil {
			varyHdr := cachedResp.Header.Get("Vary")
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
			cachedResp.TTL = rule.TTL
			p.cache.Set(storeKey, cachedResp)
			metrics.ResponseSize.WithLabelValues(host, req.Method, path).Observe(float64(len(cachedResp.Body)))
			metrics.Requests.WithLabelValues(host, req.Method, "miss", path).Inc()
			resp.Header.Set("X-Cache", "MISS")
			slog.Debug("stored in cache",
				"host", host, "method", req.Method, "path", req.URL.RequestURI(),
				"status", resp.StatusCode, "size", len(cachedResp.Body), "ttl", rule.TTL)
		} else {
			metrics.Requests.WithLabelValues(host, req.Method, "bypass", path).Inc()
			slog.Debug("not cached",
				"host", host, "method", req.Method, "status", resp.StatusCode,
				"reason", reasonNotCached(resp.StatusCode, p.maxBodyBytes))
		}
		resp.Write(w)     //nolint:errcheck
		resp.Body.Close() //nolint:errcheck
		return
	}

	slog.Debug("no policy match, forwarding transparently",
		"host", host, "method", req.Method, "path", req.URL.RequestURI())
	_, resp := p.fetchUpstream(req, body, origAddr, dial)
	if resp == nil {
		metrics.Requests.WithLabelValues(host, req.Method, "error", path).Inc()
		writeBadGateway(w)
		return
	}
	metrics.Requests.WithLabelValues(host, req.Method, "bypass", path).Inc()
	resp.Write(w)     //nolint:errcheck
	resp.Body.Close() //nolint:errcheck
}

func (p *Proxy) fetchUpstream(req *http.Request, body []byte, addr string, dial func(string) (net.Conn, error)) (*cache.Entry, *http.Response) {
	// TODO: fetchUpstream should only return the resp, not a caching object
	host := req.Host
	start := time.Now()

	upstream, err := dial(addr)
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
	elapsed := time.Since(start)
	metrics.UpstreamLatency.WithLabelValues(host, urlPath).Observe(elapsed.Seconds())
	slog.Debug("upstream response", "host", host, "method", req.Method, "path", urlPath,
		"status", resp.StatusCode, "latency", elapsed)

	if p.maxBodyBytes == 0 {
		resp.Body = &streamingBody{Reader: resp.Body, closer: upstream}
		return nil, resp
	}

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
		return p.cacheEntryFromUpstreamResponse(resp, partial), resp
	}

	return nil, resp
}

type streamingBody struct {
	io.Reader
	closer io.Closer
}

func (p *Proxy) cacheEntryFromUpstreamResponse(resp *http.Response, respBody []byte) *cache.Entry {
	return &cache.Entry{
		StatusCode: resp.StatusCode,
		Header:     resp.Header.Clone(),
		Proto:      resp.Proto,
		ProtoMajor: resp.ProtoMajor,
		ProtoMinor: resp.ProtoMinor,
		Body:       respBody,
		CachedAt:   time.Now(),
	}
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

func replyWCachedEntry(w io.Writer, e *cache.Entry) {
	resp := &http.Response{
		StatusCode:    e.StatusCode,
		Proto:         e.Proto,
		ProtoMajor:    e.ProtoMajor,
		ProtoMinor:    e.ProtoMinor,
		Header:        e.Header.Clone(),
		Body:          io.NopCloser(bytes.NewReader(e.Body)),
		ContentLength: int64(len(e.Body)),
	}
	resp.Header.Set("X-Cache", "HIT")
	resp.Write(w)
}

func reasonNotCached(status int, maxBodyBytes int64) string {
	if maxBodyBytes == 0 {
		return "body caching disabled"
	}
	if status < 200 || status >= 300 {
		return "non-2xx status"
	}
	return "body too large"
}

func writeBadGateway(w io.Writer) {
	body := []byte("bad gateway\n")
	fmt.Fprintf(w, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: %d\r\nContent-Type: text/plain\r\n\r\n%s",
		len(body), body)
}
