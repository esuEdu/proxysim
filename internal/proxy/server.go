// Package proxy forwards proxied HTTP requests and captures each exchange as a
// flow.Flow. server.go implements plain http:// forwarding (spec 004) and the
// shared request-forwarding core; connect.go adds CONNECT, TLS termination, and
// the blind-tunnel fallback (spec 005) on the same Server.
package proxy

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"proxysim/internal/ca"
	"proxysim/internal/flow"
)

// hopByHop lists the connection-scoped headers that a proxy must not forward in
// either direction (RFC 9110 §7.6.1), plus Proxy-Connection, which is not in any
// RFC but is sent by real clients.
var hopByHop = map[string]struct{}{
	"Connection":          {},
	"Proxy-Connection":    {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// Server forwards proxied requests and emits a Flow per exchange. It handles
// both plain http:// forwarding and, when given an Authority, CONNECT with TLS
// termination. One Transport is shared across requests for connection pooling;
// it also performs the upstream TLS handshake for intercepted https requests.
type Server struct {
	sink      flow.Sink
	transport *http.Transport
	bodyCap   int64

	// authority mints leaves for TLS termination. When nil, the server cannot
	// intercept, so every CONNECT is blind-tunnelled.
	authority *ca.Authority

	// excluded holds user-configured host suffixes that are always tunnelled.
	excluded []string

	// learned holds hosts observed to reject our leaf (pinned apps). After the
	// first failed interception a host lands here and is tunnelled thereafter.
	mu      sync.RWMutex
	learned map[string]struct{}
}

// Option configures a Server.
type Option func(*Server)

// WithBodyCap overrides the per-body capture ceiling (default flow.DefaultBodyCap).
// It bounds only how much is retained in the Flow; full bodies are always forwarded.
func WithBodyCap(n int64) Option {
	return func(s *Server) { s.bodyCap = n }
}

// WithExcludedHosts marks host suffixes that must never be intercepted and are
// blind-tunnelled from the first connection. Match is by suffix on the hostname
// (port stripped), so "example.com" also excludes "api.example.com".
func WithExcludedHosts(suffixes ...string) Option {
	return func(s *Server) { s.excluded = append(s.excluded, suffixes...) }
}

// WithUpstreamTLS overrides the TLS config used when dialing origins for
// intercepted https requests. The default verifies against the system roots;
// this is the seam tests use to trust an httptest origin.
func WithUpstreamTLS(cfg *tls.Config) Option {
	return func(s *Server) { s.transport.TLSClientConfig = cfg }
}

// New returns a Server emitting completed flows to sink. authority may be nil,
// in which case CONNECT requests are always tunnelled rather than intercepted.
func New(sink flow.Sink, authority *ca.Authority, opts ...Option) *Server {
	s := &Server{
		sink:      sink,
		bodyCap:   flow.DefaultBodyCap,
		authority: authority,
		learned:   make(map[string]struct{}),
		transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:    100,
			IdleConnTimeout: 90 * time.Second,
			// Preserve content-encoding on the wire: without this, the transport
			// adds Accept-Encoding: gzip and decompresses transparently, so the
			// captured bytes would not be what crossed the wire. Decoding for
			// display is spec 006's job.
			DisableCompression: true,
		},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ServeHTTP dispatches by request shape: CONNECT starts a tunnel or an
// interception (spec 005); an absolute-form request is forwarded as cleartext
// (spec 004); anything else is not a proxy request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.handleConnect(w, r)
		return
	}
	if !r.URL.IsAbs() || r.URL.Host == "" {
		// An origin-form request means the client is not using us as a proxy.
		http.Error(w, "proxy requires an absolute-form request target", http.StatusBadRequest)
		return
	}
	s.forward(w, r, "http")
}

// forward relays one request upstream and captures it. scheme is the flow's
// scheme; r.URL must already be absolute (the plain path receives it that way,
// the TLS path reconstructs it from the CONNECT host before calling in).
func (s *Server) forward(w http.ResponseWriter, r *http.Request, scheme string) {
	started := time.Now()

	// End-to-end request headers: the same stripped set is both stored and
	// forwarded, so the flow records the client's intent, not proxy bookkeeping.
	reqHeaders := r.Header.Clone()
	if reqHeaders == nil {
		reqHeaders = http.Header{}
	}
	removeHopByHop(reqHeaders)

	f := &flow.Flow{
		ID:             flow.NextID(),
		Started:        started,
		Scheme:         scheme,
		Method:         r.Method,
		Host:           r.URL.Host,
		Path:           requestPath(r),
		RequestHeaders: reqHeaders,
		Intercepted:    true,
	}

	// Tee the request body: bytes stream upstream while the first cap bytes are
	// retained. A GET with no body reads as empty.
	var reqCapture *captureReader
	var body io.Reader
	if r.Body != nil {
		reqCapture = newCaptureReader(r.Body, s.bodyCap)
		body = reqCapture
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), body)
	if err != nil {
		s.fail(w, f, started, "building upstream request: "+err.Error())
		return
	}
	outReq.Header = reqHeaders
	outReq.ContentLength = r.ContentLength

	resp, err := s.transport.RoundTrip(outReq)
	if err != nil {
		if reqCapture != nil {
			f.RequestBody, f.RequestTruncated = reqCapture.result()
		}
		s.fail(w, f, started, err.Error())
		return
	}
	defer resp.Body.Close()

	// The request body has now been sent; record what we captured of it.
	if reqCapture != nil {
		f.RequestBody, f.RequestTruncated = reqCapture.result()
	}

	respHeaders := resp.Header.Clone()
	removeHopByHop(respHeaders)
	f.StatusCode = resp.StatusCode
	f.ResponseHeaders = respHeaders

	// Relay to the client: end-to-end headers, then status, then the streamed
	// body, teed into the capture buffer.
	copyHeaders(w.Header(), respHeaders)
	w.WriteHeader(resp.StatusCode)

	respCapture := newCaptureReader(resp.Body, s.bodyCap)
	if _, err := io.Copy(w, respCapture); err != nil {
		// The status line is already sent; record the truncated relay as an error
		// but still emit the flow so the partial exchange is visible.
		f.Error = "relaying response body: " + err.Error()
	}
	f.ResponseBody, f.ResponseTruncated = respCapture.result()
	f.Duration = time.Since(started)
	s.sink.Emit(f)
}

// fail responds 502 and emits an errored flow with response fields zero.
func (s *Server) fail(w http.ResponseWriter, f *flow.Flow, started time.Time, msg string) {
	f.Error = msg
	f.Duration = time.Since(started)
	http.Error(w, "proxysim: upstream request failed", http.StatusBadGateway)
	s.sink.Emit(f)
}

// requestPath returns the path plus query, matching the Flow.Path contract.
func requestPath(r *http.Request) string {
	p := r.URL.Path
	if p == "" {
		p = "/"
	}
	if r.URL.RawQuery != "" {
		p += "?" + r.URL.RawQuery
	}
	return p
}

// removeHopByHop strips connection-scoped headers in place. Headers named in the
// Connection header are themselves hop-by-hop, so they are removed first, before
// Connection itself.
func removeHopByHop(h http.Header) {
	for _, v := range h["Connection"] {
		for token := range strings.SplitSeq(v, ",") {
			if name := strings.TrimSpace(token); name != "" {
				h.Del(name)
			}
		}
	}
	for name := range hopByHop {
		h.Del(name)
	}
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// captureReader tees the bytes read from r into a capped buffer, retaining the
// first cap bytes for the Flow while letting the full stream pass through to its
// real destination. It tracks the total so truncation can be reported honestly.
type captureReader struct {
	r     io.Reader
	buf   bytes.Buffer
	cap   int64
	total int64
}

func newCaptureReader(r io.Reader, capBytes int64) *captureReader {
	return &captureReader{r: r, cap: capBytes}
}

func (c *captureReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.total += int64(n)
		if room := c.cap - int64(c.buf.Len()); room > 0 {
			c.buf.Write(p[:min(int64(n), room)])
		}
	}
	return n, err
}

// result returns the captured prefix and whether more bytes passed than were kept.
func (c *captureReader) result() (body []byte, truncated bool) {
	b := c.buf.Bytes()
	if len(b) == 0 {
		return nil, c.total > c.cap
	}
	return b, c.total > c.cap
}
