// Package ca manages proxysim's local trust anchor: a per-machine root
// certificate authority that is generated once and reused across restarts.
//
// Stability is the whole point. The root is installed into the Simulator's
// trust store by hand; if it regenerated on every launch, that one-time setup
// would become a recurring chore and every previously issued leaf would be
// orphaned. Load therefore persists the CA to disk and reloads it verbatim.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	crtName = "ca.crt"
	keyName = "ca.key"

	// dirPerm/keyPerm/crtPerm are enforced explicitly with Chmod after writing,
	// because os.WriteFile masks the mode with the process umask and the CA key
	// leaking to group/other is exactly the failure this tool must not have.
	dirPerm os.FileMode = 0o700
	keyPerm os.FileMode = 0o600
	crtPerm os.FileMode = 0o644

	// rootValidity is deliberately long: the root is a trust anchor installed
	// once, and is exempt from the leaf validity caps (see CLAUDE.md).
	rootValidity = 10 * 365 * 24 * time.Hour

	// clockSkew backdates NotBefore so a simulator whose clock trails the host
	// does not reject a freshly minted certificate as not-yet-valid.
	clockSkew = time.Hour
)

// Authority is a loaded root CA: the certificate presented to callers, the
// private key used to sign leaves, and the shared leaf key plus per-host leaf
// cache from spec 002. The root cert/key/PEM are immutable after Load; the leaf
// cache is guarded by mu and safe for concurrent use from the request path.
type Authority struct {
	cert    *x509.Certificate
	key     *rsa.PrivateKey
	certPEM []byte

	// leafKey is a single ECDSA P-256 key reused for every minted leaf.
	// Generating one keypair per host would add latency to the first
	// connection to each new domain; reusing one is standard for intercepting
	// proxies and costs nothing here, as the key never leaves the machine.
	leafKey *ecdsa.PrivateKey

	mu    sync.RWMutex
	cache map[string]*tls.Certificate
}

// Load returns the CA stored in dir, generating and persisting a new one if
// none is present. The four on-disk states are handled distinctly on purpose:
// a half-present CA (one file without the other) or a corrupt file is a signal
// that something went wrong, and silently regenerating would invalidate every
// trust store the old certificate was installed into. Those cases fail loudly.
func Load(dir string) (*Authority, error) {
	crtPath := filepath.Join(dir, crtName)
	keyPath := filepath.Join(dir, keyName)

	crtExists, err := fileExists(crtPath)
	if err != nil {
		return nil, err
	}
	keyExists, err := fileExists(keyPath)
	if err != nil {
		return nil, err
	}

	var a *Authority
	switch {
	case crtExists && keyExists:
		a, err = loadFrom(crtPath, keyPath)
	case !crtExists && !keyExists:
		a, err = generateInto(dir, crtPath, keyPath)
	case crtExists:
		return nil, fmt.Errorf("ca: found %s without %s: refusing to regenerate and invalidate an installed CA; delete %s to start over", crtName, keyName, crtPath)
	default:
		return nil, fmt.Errorf("ca: found %s without %s: refusing to proceed; delete %s to start over", keyName, crtName, keyPath)
	}
	if err != nil {
		return nil, err
	}

	// The shared leaf key and cache are per-process, not persisted: minting is
	// sub-millisecond with a reused key, so there is nothing to save on disk.
	a.leafKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ca: generating shared leaf key: %w", err)
	}
	a.cache = make(map[string]*tls.Certificate)
	return a, nil
}

// Certificate returns the root certificate, for callers that need to display,
// export, or install it into a trust store.
func (a *Authority) Certificate() *x509.Certificate { return a.cert }

// Fingerprint returns the SHA-256 fingerprint of the root certificate as
// uppercase hex octets separated by colons, matching the form shown by
// Keychain Access and openssl -fingerprint for user comparison.
func (a *Authority) Fingerprint() string {
	sum := sha256.Sum256(a.cert.Raw)
	const hexDigits = "0123456789ABCDEF"
	out := make([]byte, 0, len(sum)*3-1)
	for i, b := range sum {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hexDigits[b>>4], hexDigits[b&0x0f])
	}
	return string(out)
}

func loadFrom(crtPath, keyPath string) (*Authority, error) {
	certPEM, err := os.ReadFile(crtPath)
	if err != nil {
		return nil, fmt.Errorf("ca: reading %s: %w", crtPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("ca: reading %s: %w", keyPath, err)
	}

	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("ca: parsing %s: %w", crtPath, err)
	}
	key, err := parseKeyPEM(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("ca: parsing %s: %w", keyPath, err)
	}

	if err := verifyKeyMatchesCert(cert, key); err != nil {
		return nil, fmt.Errorf("ca: %s and %s do not belong to the same CA: %w", crtPath, keyPath, err)
	}

	return &Authority{cert: cert, key: key, certPEM: certPEM}, nil
}

func generateInto(dir, crtPath, keyPath string) (*Authority, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("ca: generating root key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "proxysim Local Root CA",
			Organization: []string{"proxysim"},
		},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(rootValidity),
		SignatureAlgorithm:    x509.SHA256WithRSA,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// We sign leaves only, never intermediates: path length zero.
		MaxPathLen:     0,
		MaxPathLenZero: true,
		// SubjectKeyId is intentionally left unset; x509 derives it from the
		// public key hash for CA certificates.
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("ca: self-signing root: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("ca: reparsing generated root: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	if err := persist(dir, crtPath, keyPath, certPEM, keyPEM); err != nil {
		return nil, err
	}

	return &Authority{cert: cert, key: key, certPEM: certPEM}, nil
}

func persist(dir, crtPath, keyPath string, certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("ca: creating %s: %w", dir, err)
	}
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("ca: securing %s: %w", dir, err)
	}

	// Write the key first and secure it before the cert exists, so there is no
	// window in which a readable key sits on disk.
	if err := os.WriteFile(keyPath, keyPEM, keyPerm); err != nil {
		return fmt.Errorf("ca: writing %s: %w", keyPath, err)
	}
	if err := os.Chmod(keyPath, keyPerm); err != nil {
		return fmt.Errorf("ca: securing %s: %w", keyPath, err)
	}

	if err := os.WriteFile(crtPath, certPEM, crtPerm); err != nil {
		return fmt.Errorf("ca: writing %s: %w", crtPath, err)
	}
	if err := os.Chmod(crtPath, crtPerm); err != nil {
		return fmt.Errorf("ca: securing %s: %w", crtPath, err)
	}
	return nil
}

func parseCertPEM(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no CERTIFICATE PEM block found")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parseKeyPEM(keyPEM []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return key, nil
}

func verifyKeyMatchesCert(cert *x509.Certificate, key *rsa.PrivateKey) error {
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("certificate public key is %T, not RSA", cert.PublicKey)
	}
	if pub.N.Cmp(key.N) != 0 || pub.E != key.E {
		return errors.New("private key does not match certificate public key")
	}
	return nil
}

func randomSerial() (*big.Int, error) {
	// 128 random bits, per RFC 5280's recommendation for serial entropy.
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("ca: generating serial: %w", err)
	}
	return serial, nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("ca: stat %s: %w", path, err)
	}
}
