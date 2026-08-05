package ca

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Criterion 1: a generated root satisfies the CA parameters the spec requires.
func TestGenerateProducesConformantRoot(t *testing.T) {
	dir := t.TempDir()
	a, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cert := a.Certificate()
	if !cert.IsCA {
		t.Error("IsCA = false, want true")
	}
	if !cert.BasicConstraintsValid {
		t.Error("BasicConstraintsValid = false, want true")
	}
	if cert.MaxPathLen != 0 || !cert.MaxPathLenZero {
		t.Errorf("MaxPathLen = %d MaxPathLenZero = %v, want 0/true", cert.MaxPathLen, cert.MaxPathLenZero)
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("KeyUsage missing CertSign")
	}
	if cert.KeyUsage&x509.KeyUsageCRLSign == 0 {
		t.Error("KeyUsage missing CRLSign")
	}
	if cert.SignatureAlgorithm != x509.SHA256WithRSA {
		t.Errorf("SignatureAlgorithm = %v, want SHA256-RSA", cert.SignatureAlgorithm)
	}
	if got := cert.Subject.CommonName; got != "proxysim Local Root CA" {
		t.Errorf("CN = %q", got)
	}
	if key, ok := cert.PublicKey.(*rsa.PublicKey); !ok {
		t.Errorf("public key is %T, want RSA", cert.PublicKey)
	} else if key.N.BitLen() < 2048 {
		t.Errorf("RSA key is %d bits, want >= 2048", key.N.BitLen())
	}

	// Files landed where callers expect them.
	for _, name := range []string{crtName, keyName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s on disk: %v", name, err)
		}
	}
}

// Criterion 1 (optional cross-check): openssl agrees, when it is installed.
func TestGeneratedRootPassesOpenSSL(t *testing.T) {
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl not installed; Go-level assertions cover this criterion")
	}
	dir := t.TempDir()
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}

	out, err := exec.Command(openssl, "x509", "-in", filepath.Join(dir, crtName), "-noout", "-text").CombinedOutput()
	if err != nil {
		t.Fatalf("openssl: %v\n%s", err, out)
	}
	text := string(out)
	for _, want := range []string{"CA:TRUE", "pathlen:0", "Certificate Sign", "sha256WithRSAEncryption"} {
		if !strings.Contains(text, want) {
			t.Errorf("openssl output missing %q\n%s", want, text)
		}
	}
}

// Criterion 2: reloading yields a byte-identical CA. This is the criterion
// whose failure costs the user real time (every simulator needs reinstalling).
func TestLoadIsStableAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(dir)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	second, err := Load(dir)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Errorf("fingerprint changed across Load calls:\n  %s\n  %s", first.Fingerprint(), second.Fingerprint())
	}
}

// Criterion 3: the key is 0600 and the directory is 0700.
func TestPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "created-by-load")
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != dirPerm {
		t.Errorf("dir mode = %o, want %o", got, dirPerm)
	}

	ki, err := os.Stat(filepath.Join(dir, keyName))
	if err != nil {
		t.Fatal(err)
	}
	if got := ki.Mode().Perm(); got != keyPerm {
		t.Errorf("ca.key mode = %o, want %o", got, keyPerm)
	}
}

// Criterion 4: a present cert with a missing key fails loudly and does not
// touch the surviving file.
func TestHalfStateKeyMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	crtPath := filepath.Join(dir, crtName)
	before, err := os.ReadFile(crtPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, keyName)); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(dir); err == nil {
		t.Fatal("Load succeeded with ca.crt present but ca.key missing; want error")
	}

	after, err := os.ReadFile(crtPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("ca.crt was modified during a half-state Load")
	}
}

// Criterion 5: a corrupt cert fails with a message naming the file and does
// not regenerate it.
func TestCorruptCertRejectedWithoutRegeneration(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	crtPath := filepath.Join(dir, crtName)
	if err := os.WriteFile(crtPath, []byte("-----BEGIN CERTIFICATE-----\ntruncated"), crtPerm); err != nil {
		t.Fatal(err)
	}

	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load succeeded on a corrupt ca.crt; want error")
	}
	if !strings.Contains(err.Error(), crtName) {
		t.Errorf("error does not name the file: %v", err)
	}

	after, err := os.ReadFile(crtPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "truncated") {
		t.Error("corrupt ca.crt was regenerated instead of left in place")
	}
}

// Criterion 6: a cert and key from two different CAs are rejected.
func TestMismatchedKeyRejected(t *testing.T) {
	dirA := t.TempDir()
	if _, err := Load(dirA); err != nil {
		t.Fatalf("Load A: %v", err)
	}
	dirB := t.TempDir()
	if _, err := Load(dirB); err != nil {
		t.Fatalf("Load B: %v", err)
	}

	// Cross A's cert with B's key in a fresh dir.
	mixed := t.TempDir()
	copyFile(t, filepath.Join(dirA, crtName), filepath.Join(mixed, crtName))
	copyFile(t, filepath.Join(dirB, keyName), filepath.Join(mixed, keyName))

	if _, err := Load(mixed); err == nil {
		t.Fatal("Load accepted a cert/key pair from different CAs; want error")
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
