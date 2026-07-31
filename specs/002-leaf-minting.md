# 002 — Leaf Certificate Minting

**Status:** ready to implement
**Package:** `internal/ca`
**Depends on:** 001

---

## Intent

When the proxy terminates TLS for `api.example.com`, it must present a
certificate the Simulator will accept for that exact hostname, generated on the
spot. This is where Apple's certificate rules bite: a leaf that is wrong in any
of the ways listed in `CLAUDE.md` produces an opaque `-1202` with no clue as to
which rule failed.

## Scope

In: minting a leaf for a given hostname, caching, the `tls.Config.GetCertificate`
callback, SNI handling.

Out: the CONNECT handshake and TLS termination itself (→ 005), deciding which
hosts to intercept (→ 005).

---

## Behaviour

Given a hostname:

1. Strip any port. `example.com:443` and `example.com` are the same cache key.
2. On cache hit, return the cached certificate.
3. On miss, mint, cache, return.
4. Concurrent requests for the same uncached host must not corrupt the cache.
   Minting the same cert twice under a race is acceptable; a torn map is not.

### SNI and the fallback path

The certificate is selected from `ClientHelloInfo.ServerName`. Some clients —
notably anything connecting to a bare IP — send no SNI. When `ServerName` is
empty, fall back to the host from the enclosing CONNECT request, which the
callback must therefore have captured in a closure.

If the fallback host is an IP literal, it goes in `IPAddresses`, **not**
`DNSNames`. An IP placed in a DNS-type SAN entry will not match, and this is the
single most likely place to get it silently wrong.

## Certificate parameters

| Field | Value |
|---|---|
| Key | ECDSA P-256, **one key generated at startup and reused for every leaf** |
| Signature | SHA-256, by the RSA root from 001 |
| Subject CN | the hostname (cosmetic; nothing validates it any more) |
| SAN | hostname in `DNSNames`, or the IP in `IPAddresses` — never both, never the wrong one |
| `KeyUsage` | `DigitalSignature \| KeyEncipherment` |
| `ExtKeyUsage` | `ServerAuth` — mandatory |
| `NotBefore` | now − 1h |
| `NotAfter` | now + 397 days |
| Serial | random 128-bit |
| Chain presented | leaf only. The root is in the client's trust store; sending it again is wasted bytes. |

On the shared leaf key: generating a fresh keypair per host would add latency to
the first connection to every new domain. Reusing one key across leaves is
standard practice for intercepting proxies (mitmproxy does the same) and costs
nothing in this threat model — the key never leaves the machine. ECDSA P-256 is
chosen here, rather than RSA as for the root, precisely because generation is
fast and Apple accepts ECC ≥ 256-bit.

Wildcards are out of scope. Mint per exact hostname.

## Interface sketch

```go
// Leaf returns a cached or freshly minted certificate valid for host.
func (a *Authority) Leaf(host string) (*tls.Certificate, error)

// TLSConfigFor builds a server config whose GetCertificate resolves by SNI,
// falling back to connectHost when the client sends none.
func (a *Authority) TLSConfigFor(connectHost string) *tls.Config
```

---

## Acceptance criteria

1. **Apple rule conformance.** For a minted leaf,
   `openssl x509 -noout -text` shows `X509v3 Subject Alternative Name` containing
   the hostname, `TLS Web Server Authentication` under Extended Key Usage, and a
   `notAfter − notBefore` span under 398 days. Assert these in a Go test reading
   the parsed `x509.Certificate` rather than by shelling out.
2. **Chain validity.** The leaf verifies against the root via
   `x509.Certificate.Verify` with a pool containing only the root, and
   `DNSName` set to the host.
3. **IP handling.** `Leaf("127.0.0.1")` produces a cert with `IPAddresses`
   populated and `DNSNames` empty.
4. **Caching.** Two calls for the same host return the identical
   `*tls.Certificate` pointer. A different host returns a different one.
5. **Concurrency.** 100 goroutines calling `Leaf` across 10 hosts under `-race`
   produces no race and at most 10 distinct certificates.
6. **End-to-end.** With the proxy running:
   ```bash
   curl -x 127.0.0.1:8888 --cacert ~/.proxysim/ca.crt https://example.com -o /dev/null -sv
   ```
   completes with `HTTP/1.1 200` and no TLS verification error. This is the real
   test; the unit tests only tell you *why* it failed.
7. **The one that actually matters.** A booted simulator with the CA installed
   loads an HTTPS page through the proxy. Nothing above proves the Apple rules
   are satisfied — only this does.

---

## Open questions

- Cache eviction: unbounded is fine for a debugging session, but a long-running
  instance hitting thousands of hosts will grow. Defer until it is observed.
- Should the cache persist to disk across restarts? Probably not — minting is
  sub-millisecond with a reused key.
