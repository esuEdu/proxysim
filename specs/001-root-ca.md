# 001 — Root Certificate Authority

**Status:** ready to implement
**Package:** `internal/ca`
**Depends on:** nothing

---

## Intent

proxysim needs a stable local trust anchor. The user installs it into the
Simulator's trust store once; every intercepted host then gets a certificate
signed by it. "Stable" is the operative word — if the CA regenerates on restart,
the user must reinstall it in every simulator, and that turns a one-time setup
step into a recurring annoyance.

## Scope

In: generating a root CA when absent, loading it when present, persisting both
halves to disk with correct permissions.

Out: minting per-host leaf certificates (→ 002), installing the CA into any trust
store (→ 007), CA rotation or expiry handling.

---

## Behaviour

On startup, given a directory (default `~/.proxysim`):

1. If both `ca.crt` and `ca.key` exist → load and use them.
2. If neither exists → generate a new CA, write both, log the path clearly enough
   that the user can act on it.
3. If exactly one exists → **fail loudly**. Do not silently regenerate. A missing
   key next to a present cert means something went wrong, and overwriting the
   cert would silently invalidate every trust store it has been installed into.
4. If the files exist but are malformed or the key does not match the cert →
   fail with a message naming the file and suggesting deletion.

Directory is created `0700` if missing. `ca.key` is written `0600`, `ca.crt`
`0644`.

## Certificate parameters

| Field | Value |
|---|---|
| Key | RSA 2048 |
| Signature | SHA-256 |
| Subject CN | `proxysim Local Root CA` |
| Subject O | `proxysim` |
| Validity | 10 years (roots are exempt from the leaf validity caps) |
| `IsCA` | true |
| `BasicConstraintsValid` | true |
| `MaxPathLen` | 0, with `MaxPathLenZero: true` — we sign leaves only, never intermediates |
| `KeyUsage` | `CertSign \| CRLSign \| DigitalSignature` |
| Serial | cryptographically random, 128-bit |
| `NotBefore` | now − 1h, to absorb clock skew between host and simulator |

RSA rather than ECDSA for the root: it is generated once so the speed penalty is
irrelevant, and it maximises compatibility if this CA is ever pointed at an
Android emulator or an older client. Leaf keys are a separate decision (→ 002).

Do not set `SubjectKeyId` manually — Go derives it from the public key hash for
CA certificates.

## Interface sketch

Indicative, not binding:

```go
type Authority struct { /* ... */ }

// Load returns the CA in dir, generating and persisting one if absent.
func Load(dir string) (*Authority, error)

// Certificate is the root cert, for callers that need to display or export it.
func (a *Authority) Certificate() *x509.Certificate

// Fingerprint returns the SHA-256 fingerprint, for user-facing display.
func (a *Authority) Fingerprint() string
```

---

## Acceptance criteria

Each must be executable, not eyeballed.

1. **Generation.** Against an empty temp dir, `Load` produces `ca.crt` and
   `ca.key`. `openssl x509 -in ca.crt -noout -text` reports `CA:TRUE`,
   `pathlen:0`, `Certificate Sign`, and `Signature Algorithm: sha256WithRSAEncryption`.
2. **Stability.** Calling `Load` twice on the same dir yields the same
   fingerprint. This is the criterion that matters most — it is the one whose
   failure costs the user real time.
3. **Permissions.** `ca.key` is mode `0600`; the directory is `0700`.
4. **Half-state.** With `ca.key` deleted and `ca.crt` present, `Load` returns an
   error and leaves `ca.crt` byte-identical.
5. **Corruption.** With `ca.crt` truncated, `Load` returns an error naming the
   file. It does not regenerate.
6. **Key match.** A `ca.crt` from one CA next to a `ca.key` from another is
   rejected.
7. `go test ./internal/ca -race` passes; `gofmt -l` is silent.

---

## Open questions

- Should `-ca-dir` accept a path relative to the repo for throwaway testing? Lean
  yes, but it must not become the default — a CA inside the working tree is one
  `git add -A` away from being published.
