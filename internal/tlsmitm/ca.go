// SPDX-FileCopyrightText: Copyright (c) 2026, the latch developers
//
// SPDX-License-Identifier: Apache-2.0

package tlsmitm

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type cachedCert struct {
	tlsCert   *tls.Certificate
	expiresAt time.Time
}

// CA holds a signing certificate and key used to issue per-SNI leaf
// certificates on demand for TLS MITM connections.
type CA struct {
	cert   *x509.Certificate
	key    any
	cache  sync.Map         // sni string → *cachedCert
	flight singleflight.Group
}

// CertPool returns a pool containing the system roots plus this CA's certificate,
// suitable for verifying upstream TLS connections to hosts whose certs are signed by this CA.
func (ca *CA) CertPool() *x509.CertPool {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	pool.AddCert(ca.cert)
	return pool
}

// LoadCA reads tls.crt and tls.key from dir and returns a CA ready for use.
// The Secret mounted by the Helm chart uses these filenames by default.
func LoadCA(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "tls.key"))
	if err != nil {
		return nil, fmt.Errorf("read CA key: %w", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("CA cert: no PEM block found")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("CA cert: not a CA certificate (BasicConstraints IsCA is false)")
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("CA key: no PEM block found")
	}

	var key any
	switch {
	case func() bool { k, e := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); key = k; return e == nil }():
	case func() bool { k, e := x509.ParseECPrivateKey(keyBlock.Bytes); key = k; return e == nil }():
	case func() bool { k, e := x509.ParsePKCS1PrivateKey(keyBlock.Bytes); key = k; return e == nil }():
	default:
		return nil, fmt.Errorf("CA key: unrecognised format")
	}

	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("CA key: does not implement crypto.Signer")
	}
	certPubDER, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("CA cert: marshal public key: %w", err)
	}
	keyPubDER, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return nil, fmt.Errorf("CA key: marshal public key: %w", err)
	}
	if !bytes.Equal(certPubDER, keyPubDER) {
		return nil, fmt.Errorf("CA cert and key do not match")
	}

	return &CA{cert: cert, key: key}, nil
}

// GetCertificate returns a leaf certificate for the SNI in hello, signing it
// with the CA key on first use and caching the result for 24 h.
func (ca *CA) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	sni := hello.ServerName
	if sni == "" {
		sni = "unknown"
	}

	if v, ok := ca.cache.Load(sni); ok {
		if e := v.(*cachedCert); time.Now().Before(e.expiresAt) {
			slog.Debug("TLS MITM: leaf cert cache hit", "sni", sni, "expires", e.expiresAt.Format(time.RFC3339))
			return e.tlsCert, nil
		}
	}

	v, err, _ := ca.flight.Do(sni, func() (any, error) {
		// Re-check: another goroutine may have generated and cached while we waited.
		if v, ok := ca.cache.Load(sni); ok {
			if e := v.(*cachedCert); time.Now().Before(e.expiresAt) {
				return e.tlsCert, nil
			}
		}

		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}

		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return nil, err
		}

		notAfter := time.Now().Add(24 * time.Hour)
		if ca.cert.NotAfter.Before(notAfter) {
			notAfter = ca.cert.NotAfter
		}

		tmpl := &x509.Certificate{
			SerialNumber: serial,
			Subject:      pkix.Name{CommonName: sni},
			DNSNames:     []string{sni},
			NotBefore:    time.Now().Add(-time.Minute),
			NotAfter:     notAfter,
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}

		certDER, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, leafKey.Public(), ca.key)
		if err != nil {
			return nil, err
		}

		tlsCert := &tls.Certificate{
			Certificate: [][]byte{certDER, ca.cert.Raw},
			PrivateKey:  leafKey,
		}

		cacheExpiry := notAfter.Add(-5 * time.Minute)
		ca.cache.Store(sni, &cachedCert{tlsCert: tlsCert, expiresAt: cacheExpiry})
		slog.Debug("TLS MITM: issued leaf cert", "sni", sni, "not_after", notAfter.Format(time.RFC3339))
		return tlsCert, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*tls.Certificate), nil
}
