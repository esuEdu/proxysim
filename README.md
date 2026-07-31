# proxysim

A local HTTP/HTTPS forward proxy and traffic sniffer for the iOS Simulator.
Charles/Proxyman in miniature, built to be understood rather than to be complete.

**Status:** specs written, no implementation yet.

## How this repo works

`CLAUDE.md` is the constitution — mission, security rules, tech stack, and the
Apple TLS requirements that make certificate work here non-obvious. It is loaded
into every Claude Code session.

`specs/` holds one spec per architectural seam. Each states intent, scope, and
executable acceptance criteria. Specs are written just ahead of implementation,
not all at once.

| Spec | Seam | Status |
|---|---|---|
| 001 | Root CA lifecycle | ready |
| 002 | Per-host leaf minting | ready |
| 003 | Flow model — the engine/UI contract | ready |
| 004 | Plain HTTP forwarding | ready |
| 005 | CONNECT + TLS termination, MITM exclusions | ready |
| 006 | Body decoding (gzip/deflate/br, chunked) | not written |
| 007 | `simctl` trust automation | not written |
| 008 | Desktop UI | not written |

Build order is 001 → 002 → 003 → 005. 003 lands before any capture code, since
both 004 and 005 write into it.

## Working on it

```
> read CLAUDE.md and specs/001-root-ca.md, then plan the implementation
```

Plan first (shift+tab), review, then implement. A spec is done when its
acceptance criteria run and pass.

Before writing 004–006, spike the parts no document can settle: confirm a booted
simulator actually routes through the proxy port, that `simctl keychain add-root-cert`
sticks, and which of your target apps pin certificates. What you learn getting
one certificate accepted will rewrite half the remaining specs.

## Not a general-purpose tool

Loopback only, one developer, one machine. It decrypts TLS, which is only
acceptable because it is local and explicit. See the security section of
`CLAUDE.md` before changing anything about how it binds or where the CA key
lives.
