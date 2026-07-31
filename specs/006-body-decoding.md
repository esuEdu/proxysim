# 006 — Body Decoding for Display

**Status:** ready to implement
**Package:** `internal/flow`
**Depends on:** 003 (the Flow whose raw bytes this decodes for rendering)

---

## Intent

Flows store exactly what crossed the wire — a gzip or brotli body is kept
compressed (spec 003's central rule). That is correct for fidelity but useless
for reading: the console today, and a UI later, need the *decoded* body to show
the user. 006 adds the decoder that turns stored bytes into readable ones at
render time, without ever mutating what was stored.

This is a presentation concern, not a capture one. The distinction matters: if
decoding happened at capture time the byte counts would lie and we would lose the
ability to show what was actually sent. Decoding belongs here, downstream of the
Flow, run on demand by whatever is displaying it.

## Scope

In: decoding `Content-Encoding` — `gzip`, `deflate`, `br`, `identity`, stacked
combinations, and graceful handling of unknown, truncated, or bomb-sized bodies.
Wiring it into the console sink so verbose output shows decoded bodies.

Out: `Transfer-Encoding`/chunked (already normalised away by `net/http` — see
004), storing decoded bytes anywhere (never — raw is canonical), request/response
body *capture* (004/005), any UI beyond the console.

---

## Why a dependency here

The stdlib covers `gzip` (`compress/gzip`) and DEFLATE (`compress/flate`,
`compress/zlib`). It has **no brotli decoder**, and Apple's URLSession advertises
`Accept-Encoding: gzip, deflate, br` — we saw exactly that in the simulator
captures. Brotli bodies are therefore common and unreadable without a decoder.

`github.com/andybalholm/brotli` is the expected first dependency named in
CLAUDE.md. It is decode-capable, pure Go, and widely used. This is the one place
the dependency bar is met: the stdlib genuinely does not solve it. Nothing else
new is pulled in.

## The rules that bite

**`deflate` is ambiguous.** The HTTP token historically means zlib-wrapped
DEFLATE (RFC 1950), but many servers send raw DEFLATE (RFC 1951) under the same
name. Try zlib first; on failure, fall back to raw flate. Getting this wrong
shows a decode error on perfectly valid bodies.

**Encodings stack.** `Content-Encoding` is a comma-separated list applied in
order, so `deflate, gzip` was deflated first and gzipped second. Decode in
**reverse**: gunzip, then inflate. Trim and lowercase each token.

**Truncated bodies are incomplete streams.** A body captured with
`ResponseTruncated: true` is a compressed stream cut off mid-way, so any decoder
will hit an unexpected EOF. That is expected, not an error: return what decoded
and mark it partial. The user must see the readable prefix, never a bare "decode
failed" that hides a 9 MB body because its last chunk is missing.

**Decompression bombs.** A few KB of gzip can expand to gigabytes. Decoding is
bounded by a cap; output beyond it is dropped and marked partial. A debugging
tool should show the first N MB and say it stopped, not exhaust memory.

**Unknown encodings pass through.** An encoding we do not implement (e.g. `zstd`)
is not an error: return the raw bytes and note the encoding so the UI can say
"shown compressed — zstd not supported" rather than rendering garbage.

---

## Behaviour

Given the `Content-Encoding` header value and the raw body:

1. Parse the codings (split on comma, trim, lowercase). Empty / absent /
   `identity` → return the body unchanged, `Decoded: false`.
2. Apply decoders in reverse order, each bounded by the cap.
3. If every coding is known and the stream is complete → `Decoded: true`,
   `Partial: false`.
4. If a decoder hits truncation or the cap → `Partial: true`, `decoded` holds the
   bytes obtained so far.
5. If a coding is unknown → stop, return the bytes decoded up to that point (or
   raw if none), with a `Note` naming the unsupported coding.

Decoding must not mutate the input slice; callers still hold the canonical raw
bytes.

## Interface sketch

Indicative, not binding:

```go
// Decode returns body decoded per contentEncoding, bounded by cap bytes. It
// never returns an error for a body it cannot fully decode — a truncated,
// bomb-sized, or unknown-encoding body yields the best partial result with the
// report explaining what happened. The input slice is never modified.
func Decode(contentEncoding string, body []byte, cap int) (decoded []byte, report DecodeReport)

type DecodeReport struct {
    Encodings []string // codings seen, outermost (wire order) first
    Decoded   bool     // any decoding actually applied
    Partial   bool     // stopped early: truncated stream, hit the cap, or unknown coding
    Note      string   // human-facing reason when Partial or unknown, else empty
}
```

A `cap` of 0 means use `DefaultBodyCap`.

### Console integration

The console sink's verbose body rendering (spec 003) currently sees a gzipped
body as opaque binary. After 006 it must:

1. `Decode` the body using the flow's `Content-Encoding` and the truncation flag.
2. Decide textual-vs-binary on the **decoded** bytes, then pretty-print JSON as
   before.
3. When decoding was applied, annotate the body line (e.g. `gzip → 4.2KiB`), and
   when `Partial`, show the decode note alongside the existing truncation marker
   — the two are distinct (a body can be fully captured but only partially
   decodable, or fully decodable but truncated in capture).

The stored `Flow` bytes remain the raw compressed bytes; only the rendering
changes.

---

## Acceptance criteria

Each executable.

1. **gzip.** A gzip-compressed payload decodes to the original; `Decoded:true`,
   `Partial:false`.
2. **deflate, both flavours.** Both zlib-wrapped and raw-DEFLATE bodies labelled
   `deflate` decode to the original.
3. **brotli.** A `br` body decodes to the original.
4. **identity / absent.** `""`, `identity`, and whitespace return the body
   unchanged with `Decoded:false`, and the returned slice equals the input.
5. **Stacked.** A body compressed deflate-then-gzip and labelled `deflate, gzip`
   decodes fully to the original.
6. **Unknown coding.** `zstd` returns the raw body, `Partial:true`, `Note`
   naming zstd; no error, no panic.
7. **Truncated stream.** The first half of a gzip stream yields `Partial:true`
   and a non-empty decoded prefix — no error surfaced.
8. **Bomb cap.** A body that inflates past the cap returns exactly cap bytes,
   `Partial:true`.
9. **Input immutability.** The input slice is byte-identical after `Decode`
   returns.
10. **Console.** A gzipped JSON response renders pretty-printed decoded JSON in
    verbose mode, the summary/annotation shows it was gzip, and the flow's stored
    `ResponseBody` is still gzip (unchanged).
11. `go test ./internal/flow -race` passes; `go vet` clean; `gofmt -l` silent;
    `go mod tidy` leaves a single new dependency (brotli) in `go.mod`.

---

## Open questions

- **`[]byte` vs `io.Reader`.** The console wants bytes; a UI streaming a large
  body might prefer a bounded reader. Ship the `[]byte` form now (simplest, and
  the cap makes it safe); add a reader variant if the UI needs it. Additive, so
  defer.
- **Cap source.** One global `DefaultBodyCap` is reused. A separate, larger
  decode cap may make sense (compressed capture cap ≠ decoded display cap), but
  start with one number and split only if it bites.
- **`zstd`.** Not advertised by URLSession, so out for now. If a target app
  negotiates it, it is another decode-only dependency (`klauspost/compress`),
  decided the same way brotli was.
- **Charset decoding.** `Content-Encoding` is not `Content-Type` charset; a body
  that decompresses to non-UTF-8 text (e.g. Shift-JIS) is still shown by the
  existing textual/binary heuristic. Charset transcoding is a separate, later
  concern — noted so it is not silently conflated with content-encoding.
