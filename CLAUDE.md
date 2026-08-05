# proxysim — Project Constitution

This file is loaded into every agent session. It contains rules that outlive any
single feature. Feature-specific detail belongs in `specs/`, not here.

---

## Mission

A local HTTP/HTTPS forward proxy that intercepts, decrypts, and displays network
traffic from the iOS Simulator in real time. Comparable in scope to Charles or
Proxyman, but single-purpose and hackable.

Built for one developer debugging their own apps on their own machine.

---

## Non-Negotiable Constraints

### Security posture

- The proxy listener binds to `127.0.0.1` only. Never `0.0.0.0`, never a LAN
  address, not behind a flag, not "for testing". A misconfigured MITM proxy
  reachable from the network is a genuine hazard.
- The CA private key is written `0600`, in a directory created `0700`.
- `ca.key` and `ca.crt` must never be committed. They live in `~/.proxysim/` by
  default, outside the repo. `.gitignore` covers them anyway as a second line.
- The CA is generated locally per-machine and is never distributed. There is no
  scenario in which a prebuilt CA ships with this tool.

### Apple TLS requirements (the expensive-to-rediscover part)

The Simulator uses the real Security framework, so certificates that Go accepts
and OpenSSL validates will still be rejected if they violate these. Failures
surface as an opaque `NSURLErrorServerCertificateUntrusted` (-1202) with no
indication of which rule was broken. Every leaf certificate must satisfy:

| Rule | Detail |
|---|---|
| Key size | RSA ≥ 2048-bit, or ECC ≥ 256-bit |
| Signature hash | SHA-256 or stronger. Never SHA-1. |
| Hostname | Must be in `subjectAltName`. A hostname in CN alone is **ignored** as of iOS 13. |
| SAN entry type | DNS hostnames go in `DNSNames`; IP literals go in `IPAddresses`. An IP placed in a DNS-type entry will not match. |
| EKU | Must contain `id-kp-serverAuth`. |
| Validity | ≤ 825 days, enforced since iOS 13. |

The **root CA certificate** has its own separate requirement, and violating it
fails earlier and more confusingly than any of the above:

- The root **must carry a Key Usage extension (OID 2.5.29.15) including
  `keyCertSign`.** Without it, the certificate imports with every appearance of
  success but never shows up under Settings → General → About → Certificate
  Trust Settings, so it can never actually be trusted. There is no error
  message anywhere in this path. Confirmed by Apple Developer Technical Support
  (developer.apple.com/forums/thread/743058).
- In Go: `KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign`, plus
  `IsCA: true` and `BasicConstraintsValid: true`.
- Unlike leaves, the root itself may be long-lived (10 years is fine).

On the 398-day limit: Apple's stricter 398-day cap applies only to certificates
chaining to roots *preinstalled* with the OS, so a user-installed CA like ours is
technically exempt. We cap leaves at **397 days** regardless — it costs nothing
and makes us immune if that carve-out ever narrows.

The two-certificate structure — one long-lived root in the trust store, per-host
leaves signed by it — is mandatory rather than stylistic. The trust anchor has
to be installed once and stay stable, while the certificate presented for each
connection has to carry that connection's hostname in its SAN. One certificate
cannot do both jobs: a self-signed leaf would require a new trust-store install
for every host the app talks to.

### Interception has hard limits

Certificate pinning defeats this tool by design, and that is correct behaviour on
the app's part. When a host cannot be intercepted the proxy must fall back to a
blind TCP tunnel and log the host as un-inspectable — never fail the connection,
never retry, never present it as a bug to be fixed.

---

## Tech Stack

- **Go**, standard library first. `crypto/x509`, `crypto/tls`, and `net/http`
  cover essentially all of the engine.
- **Dependency bar:** a third-party package must earn its place by solving
  something the stdlib genuinely does not. `goproxy` and `martian` are
  explicitly *not* used — they abstract away the CONNECT/TLS-termination
  handling that is the interesting part of this project, and their logging hooks
  fit our flow model poorly. Brotli (`andybalholm/brotli`) is the expected first
  legitimate dependency, since Apple's URLSession advertises `br` and the stdlib
  has no decoder.
- **No web framework, no logging framework, no DI container.**

---

## Layout

```
proxysim/
├── CLAUDE.md
├── specs/                 # one spec per seam, numbered
├── main.go                # flag parsing, wiring, signal handling — thin
└── internal/
    ├── ca/                # root CA + leaf minting
    ├── proxy/             # HTTP forwarding, CONNECT, TLS termination
    └── flow/              # the Flow model + sinks (console, file, future UI)
```

`internal/` is deliberate: nothing here is a public API.

---

## Conventions

- Errors are wrapped with `fmt.Errorf("...: %w", err)` and carry the host or flow
  ID they relate to. A bare `return err` from a network path is a review failure.
- No `panic` outside `main` startup. A malformed request from a client must never
  take the proxy down.
- Every exported symbol gets a doc comment explaining *why*, not what.
  `// Leaf returns a certificate` is noise; the caching and SNI-fallback
  rationale is not.
- Concurrency: anything reachable from a request path must be race-clean. Tests
  run under `-race` and that is not optional.
- Comments explain non-obvious protocol behaviour (hop-by-hop headers, chunked
  encoding, the 200-then-TLS CONNECT dance). Assume the reader knows Go and does
  not know RFC 9110 by heart.

---

## Commands

```bash
go build ./...
go test ./... -race
go vet ./...
gofmt -l .                  # must print nothing

go run . -port 8888 -ca-dir ~/.proxysim
```

Manual end-to-end check (the only test that really counts):

```bash
curl -x 127.0.0.1:8888 --cacert ~/.proxysim/ca.crt https://example.com -v
```

---

## Simulator Integration Reference

The Simulator routes through the **host Mac's** network stack, so it inherits
macOS system proxy settings rather than having its own.

```bash
# trust the CA in the booted simulator (Xcode 11.4+)
xcrun simctl keychain booted add-root-cert ~/.proxysim/ca.crt

# Xcode Previews runs a separate simulator set and needs its own install
xcrun simctl --set previews keychain booted add-root-cert ~/.proxysim/ca.crt
```

Trust does not survive `simctl erase`. Re-run after resetting a device.

---

## Working Agreement

- Read the relevant `specs/NNN-*.md` before writing code. If the spec is
  ambiguous, ask — do not resolve the ambiguity silently and proceed.
- Implement one spec at a time. Do not anticipate a later spec's requirements.
- A spec is done when its acceptance criteria execute and pass, not when the code
  looks finished.
- If implementation reveals the spec was wrong, stop and say so. Update the spec
  first, then the code. The spec is the source of truth; code that silently
  diverges from it is the failure mode this whole approach exists to prevent.
