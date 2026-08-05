// Package flow defines the record of one request/response exchange and the
// sinks that consume it. It is the seam between the capture engine and
// everything downstream — the console today, a desktop UI or a .har exporter
// later — so it deliberately imports nothing from internal/proxy or internal/ca:
// the contract points one way, from producers into flow, never back.
package flow

import (
	"net/http"
	"sync/atomic"
	"time"
)

// DefaultBodyCap is the byte ceiling producers apply when buffering a body.
// Capping happens at capture time (specs 004/005), not here; this package only
// stores the bytes and the Truncated flag that records whether the ceiling was
// hit. A UI that cannot tell "empty body" from "body we declined to buffer"
// would show a confident lie, so truncation is always explicit.
const DefaultBodyCap = 10 << 20 // 10 MiB

// Flow is one completed request/response exchange. It holds exactly what
// crossed the wire — bodies are the raw bytes, still gzipped or chunked as
// sent; decoding is a render-time concern (spec 006). Storing decoded bodies
// would destroy the ability to show what was actually sent and make byte counts
// lie.
//
// The JSON tags are a stable contract: renaming a field breaks every persisted
// capture, so they are explicit on every field.
type Flow struct {
	ID       uint64        `json:"id"` // monotonic, per-process
	Started  time.Time     `json:"started"`
	Duration time.Duration `json:"duration_ns"`

	Scheme string `json:"scheme"` // "http" | "https"
	Method string `json:"method"`
	Host   string `json:"host"`
	Path   string `json:"path"` // path + query

	RequestHeaders   http.Header `json:"request_headers"`
	RequestBody      []byte      `json:"request_body,omitempty"`
	RequestTruncated bool        `json:"request_truncated"`

	StatusCode        int         `json:"status_code"`
	ResponseHeaders   http.Header `json:"response_headers"`
	ResponseBody      []byte      `json:"response_body,omitempty"`
	ResponseTruncated bool        `json:"response_truncated"`

	Intercepted bool   `json:"intercepted"`     // false = blind tunnel, pinned host
	Error       string `json:"error,omitempty"` // a string, not an error, so it survives JSON
}

// lastID is the process-wide flow counter. IDs need only be unique and
// monotonic within a run; they are not persisted across restarts.
var lastID atomic.Uint64

// NextID returns the next flow ID, unique and monotonically increasing for the
// life of the process. It is safe to call concurrently; the first call returns 1.
func NextID() uint64 {
	return lastID.Add(1)
}
