package ca

import (
	"crypto/x509"
	"fmt"
	"sync"
	"testing"
	"time"
)

// newTestAuthority loads a fresh CA in a temp dir.
func newTestAuthority(t *testing.T) *Authority {
	t.Helper()
	a, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return a
}

// Criterion 1: a minted leaf satisfies the Apple rules that produce an opaque
// -1202 when violated.
func TestLeafSatisfiesAppleRules(t *testing.T) {
	a := newTestAuthority(t)
	cert, err := a.Leaf("api.example.com")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}
	leaf := cert.Leaf

	found := false
	for _, name := range leaf.DNSNames {
		if name == "api.example.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("hostname not in DNSNames SAN: %v", leaf.DNSNames)
	}

	hasServerAuth := false
	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			hasServerAuth = true
		}
	}
	if !hasServerAuth {
		t.Error("leaf missing ExtKeyUsageServerAuth")
	}

	if span := leaf.NotAfter.Sub(leaf.NotBefore); span >= 398*24*time.Hour {
		t.Errorf("validity span %v exceeds the 398-day cap", span)
	}
}

// Criterion 2: the leaf chains to the root.
func TestLeafVerifiesAgainstRoot(t *testing.T) {
	a := newTestAuthority(t)
	cert, err := a.Leaf("secure.example.com")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(a.Certificate())
	if _, err := cert.Leaf.Verify(x509.VerifyOptions{
		Roots:   roots,
		DNSName: "secure.example.com",
	}); err != nil {
		t.Fatalf("leaf failed to verify against root: %v", err)
	}
}

// Criterion 3: an IP host goes in IPAddresses, never DNSNames.
func TestLeafIPHandling(t *testing.T) {
	a := newTestAuthority(t)
	cert, err := a.Leaf("127.0.0.1")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}
	leaf := cert.Leaf

	if len(leaf.IPAddresses) != 1 || !leaf.IPAddresses[0].Equal([]byte{127, 0, 0, 1}) {
		t.Errorf("IPAddresses = %v, want [127.0.0.1]", leaf.IPAddresses)
	}
	if len(leaf.DNSNames) != 0 {
		t.Errorf("DNSNames = %v, want empty for an IP host", leaf.DNSNames)
	}

	// It must still verify when the client dials the IP.
	roots := x509.NewCertPool()
	roots.AddCert(a.Certificate())
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "127.0.0.1"}); err != nil {
		t.Fatalf("IP leaf failed to verify: %v", err)
	}
}

// Criterion 4: caching returns one pointer per host, distinct across hosts.
func TestLeafCaching(t *testing.T) {
	a := newTestAuthority(t)
	first, err := a.Leaf("example.com")
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Leaf("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("two Leaf calls for the same host returned different pointers")
	}

	other, err := a.Leaf("other.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Error("distinct hosts returned the same certificate pointer")
	}
}

// Criterion 6: a host with a port and the bare host share a cache entry.
func TestLeafStripsPort(t *testing.T) {
	a := newTestAuthority(t)
	withPort, err := a.Leaf("example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	bare, err := a.Leaf("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if withPort != bare {
		t.Error("example.com:443 and example.com resolved to different certificates")
	}
}

// Criterion 5: concurrent minting is race-clean and yields at most one cert per
// host across 10 hosts hit by 100 goroutines.
func TestLeafConcurrency(t *testing.T) {
	a := newTestAuthority(t)
	const hosts, goroutines = 10, 100

	var wg sync.WaitGroup
	results := make([]*x509.Certificate, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			host := fmt.Sprintf("host-%d.example.com", i%hosts)
			cert, err := a.Leaf(host)
			if err != nil {
				t.Errorf("Leaf(%s): %v", host, err)
				return
			}
			results[i] = cert.Leaf
		}(i)
	}
	wg.Wait()

	distinct := make(map[string]struct{})
	for _, c := range results {
		if c != nil {
			distinct[string(c.Raw)] = struct{}{}
		}
	}
	if len(distinct) > hosts {
		t.Errorf("minted %d distinct certificates for %d hosts", len(distinct), hosts)
	}
}
