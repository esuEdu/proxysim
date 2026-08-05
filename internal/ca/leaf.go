package ca

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net"
	"time"
)

// leafValidity is capped at 397 days. Apple enforces a 398-day ceiling on leaf
// certificates; staying a day under it keeps us immune even if the carve-out
// for user-installed roots ever narrows (see CLAUDE.md).
const leafValidity = 397 * 24 * time.Hour

// Leaf returns a certificate valid for host, minting and caching one on first
// use and returning the cached pointer thereafter. host may carry a port
// (example.com:443); it is stripped so that host-with-port and bare host share
// one cache entry.
//
// Concurrent callers for the same uncached host may each mint a certificate,
// but only one is ever stored and returned, so a given host always resolves to
// a single stable *tls.Certificate.
func (a *Authority) Leaf(host string) (*tls.Certificate, error) {
	key := hostOnly(host)

	a.mu.RLock()
	cert, ok := a.cache[key]
	a.mu.RUnlock()
	if ok {
		return cert, nil
	}

	// Mint outside the lock; keygen and signing must not block other hosts.
	minted, err := a.mintLeaf(key)
	if err != nil {
		return nil, err
	}

	// Double-checked store: if another goroutine won the race for this host,
	// keep theirs and discard ours so every future call returns one pointer.
	a.mu.Lock()
	defer a.mu.Unlock()
	if existing, ok := a.cache[key]; ok {
		return existing, nil
	}
	a.cache[key] = minted
	return minted, nil
}

// TLSConfigFor builds a server-side TLS config whose certificate is chosen by
// SNI. Clients connecting to a bare IP often send no SNI; when ServerName is
// empty the config falls back to connectHost, the host named in the enclosing
// CONNECT request, which is why that host is captured in the closure.
func (a *Authority) TLSConfigFor(connectHost string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12, // Apple requires TLS 1.2 or newer
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if name == "" {
				name = connectHost
			}
			return a.Leaf(name)
		},
	}
}

func (a *Authority) mintLeaf(host string) (*tls.Certificate, error) {
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:       serial,
		Subject:            pkix.Name{CommonName: host},
		NotBefore:          now.Add(-clockSkew),
		NotAfter:           now.Add(leafValidity),
		SignatureAlgorithm: x509.SHA256WithRSA, // signed by the RSA root
		KeyUsage:           x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:        []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	// The SAN entry type must match the host kind: an IP literal placed in a
	// DNS-type entry will not match, and the failure is a silent -1202. A
	// hostname in the CN alone is ignored as of iOS 13, so the SAN is the only
	// place identity is asserted.
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &a.leafKey.PublicKey, a.key)
	if err != nil {
		return nil, fmt.Errorf("ca: minting leaf for %q: %w", host, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("ca: reparsing leaf for %q: %w", host, err)
	}

	// Present the leaf alone: the root is already in the client's trust store,
	// so re-sending it is wasted bytes.
	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  a.leafKey,
		Leaf:        leaf,
	}, nil
}

// hostOnly strips a trailing port from host, leaving the bare hostname or IP.
// A host with no port (or an unparseable one) is returned unchanged.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
