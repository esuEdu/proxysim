package ui

import (
	"net/http"

	"proxysim/internal/flow"
)

// detailResponse is the per-flow detail wire shape. Flow is the raw 003 flow
// with bodies exactly as stored (still compressed), so its JSON key set stays
// the golden 003 set; Request and Response add the decoded views alongside,
// keeping "what crossed the wire" and "what it decodes to" both available.
type detailResponse struct {
	Flow     *flow.Flow `json:"flow"`
	Request  bodyView   `json:"request"`
	Response bodyView   `json:"response"`
}

// bodyView is a body decoded for display (spec 006). Data holds the decoded
// bytes (base64 in JSON) so arbitrary binary survives the trip; the browser
// decides text-vs-binary and pretty-prints. RawSize is the stored (compressed)
// size, reported separately because Data is post-decode and would otherwise
// make the on-wire byte count lie.
type bodyView struct {
	RawSize   int      `json:"raw_size"`
	Truncated bool     `json:"truncated"`            // capture hit the buffering cap
	MediaType string   `json:"media_type,omitempty"` // Content-Type, for the render decision
	Encodings []string `json:"encodings,omitempty"`  // content codings seen, outermost first
	Decoded   bool     `json:"decoded"`              // any decoding was applied
	Partial   bool     `json:"partial"`              // decoding stopped early (truncated/cap/unknown)
	Note      string   `json:"note,omitempty"`       // human reason when Partial
	Data      []byte   `json:"data,omitempty"`       // decoded bytes
}

// buildDetail assembles the full view for one flow, decoding both bodies.
func buildDetail(f *flow.Flow) detailResponse {
	return detailResponse{
		Flow:     f,
		Request:  decodeBody(f.RequestHeaders, f.RequestBody, f.RequestTruncated),
		Response: decodeBody(f.ResponseHeaders, f.ResponseBody, f.ResponseTruncated),
	}
}

// decodeBody decodes one body per its Content-Encoding, recording the raw size
// and what decoding did. An empty body yields a zero-value view (no Data).
func decodeBody(h http.Header, body []byte, truncated bool) bodyView {
	v := bodyView{RawSize: len(body), Truncated: truncated, MediaType: h.Get("Content-Type")}
	if len(body) == 0 {
		return v
	}

	decoded, report := flow.Decode(h.Get("Content-Encoding"), body, 0)
	v.Encodings = report.Encodings
	v.Decoded = report.Decoded
	v.Partial = report.Partial
	v.Note = report.Note
	v.Data = decoded
	return v
}
