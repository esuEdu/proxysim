package proxy

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"proxysim/internal/flow"
)

const connectEstablished = "HTTP/1.1 200 Connection Established\r\n\r\n"

// handleConnect answers a CONNECT and either blind-tunnels the host or
// terminates its TLS to inspect it. Excluded and previously-pinned hosts are
// tunnelled from the start; everything else is intercepted.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	hostPort := r.Host // "host:port", per the CONNECT target

	// Origin filter (spec 009) is the outer gate: a connection from a process we
	// were not asked to watch is tunnelled silently — passed through untouched and
	// never surfaced as a flow, unlike an excluded host which is shown as
	// tunnelled. Decided here, before the host matters.
	if !s.originAllows(r) {
		s.tunnel(w, hostPort, "", false)
		return
	}
	if s.shouldTunnel(hostPort) {
		s.tunnel(w, hostPort, "", true)
		return
	}
	s.intercept(w, hostPort)
}

// shouldTunnel reports whether a host must be tunnelled rather than intercepted:
// when there is no CA to mint leaves, when it matches a configured exclusion, or
// when interception was already observed to fail for it.
func (s *Server) shouldTunnel(hostPort string) bool {
	if s.authority == nil {
		return true
	}
	host := hostname(hostPort)
	for _, suffix := range s.excluded {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	s.mu.RLock()
	_, learned := s.learned[host]
	s.mu.RUnlock()
	return learned
}

// learn records a host whose interception the client refused, so subsequent
// connections to it tunnel instead of breaking again.
func (s *Server) learn(hostPort string) {
	host := hostname(hostPort)
	s.mu.Lock()
	s.learned[host] = struct{}{}
	s.mu.Unlock()
}

// intercept terminates the client's TLS with a minted leaf and forwards the
// decrypted requests. If the client rejects our certificate — the signature of a
// pinned app — the host is remembered so the next connection tunnels.
func (s *Server) intercept(w http.ResponseWriter, hostPort string) {
	started := time.Now()
	conn, err := hijack(w)
	if err != nil {
		http.Error(w, "proxysim: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.WriteString(conn, connectEstablished); err != nil {
		conn.Close()
		return
	}

	cfg := s.authority.TLSConfigFor(hostPort)
	// Force HTTP/1.1: we cannot yet parse an intercepted h2 stream (out of scope).
	cfg.NextProtos = []string{"http/1.1"}

	tlsConn := tls.Server(conn, cfg)
	if err := tlsConn.Handshake(); err != nil {
		// The client would not accept our leaf. Its ClientHello is already
		// consumed, so this connection cannot be rescued into a tunnel — but the
		// host is marked so the next one is tunnelled from the start.
		s.learn(hostPort)
		s.emitTunnel(hostPort, started, "interception refused by client: "+err.Error())
		tlsConn.Close()
		return
	}

	s.serveDecrypted(tlsConn, hostPort)
}

// serveDecrypted runs an HTTP/1.1 server over the terminated TLS connection,
// forwarding each request (keep-alive included) through the shared core. It
// returns when the connection closes.
func (s *Server) serveDecrypted(conn net.Conn, connectHost string) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if host == "" {
			host = connectHost // no Host header: fall back to the CONNECT target
		}
		// Reconstruct the absolute URL the forwarding core expects.
		r.URL.Scheme = "https"
		r.URL.Host = host
		// The origin filter already passed at CONNECT for this connection, so
		// every decrypted request on it is captured.
		s.forward(w, r, "https", true)
	})

	l := newSingleConnListener(conn)
	srv := &http.Server{
		Handler: handler,
		ConnState: func(_ net.Conn, state http.ConnState) {
			// Once the single connection is done, unblock Accept so Serve returns.
			if state == http.StateClosed || state == http.StateHijacked {
				l.Close()
			}
		},
	}
	_ = srv.Serve(l) // returns net.ErrClosed once the connection closes
}

// tunnel blind-copies bytes between the client and origin without inspecting
// them. When emit is true it records a single un-intercepted flow when the
// connection ends; the origin filter passes emit=false so a filtered-out
// connection stays invisible. note is non-empty only when the tunnel is a
// fallback from a failed interception.
func (s *Server) tunnel(w http.ResponseWriter, hostPort, note string, emit bool) {
	started := time.Now()
	client, err := hijack(w)
	if err != nil {
		http.Error(w, "proxysim: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer client.Close()

	upstream, err := net.DialTimeout("tcp", hostPort, 30*time.Second)
	if err != nil {
		// The client is waiting on our CONNECT reply; a 502 is the honest answer.
		io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		if emit {
			s.emitTunnel(hostPort, started, err.Error())
		}
		return
	}
	defer upstream.Close()

	if _, err := io.WriteString(client, connectEstablished); err != nil {
		return
	}

	// Copy both directions; the first to finish tears down the other so neither
	// half-open direction lingers.
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, client); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); done <- struct{}{} }()
	<-done

	if emit {
		s.emitTunnel(hostPort, started, note)
	}
}

// emitTunnel emits the flow for a tunnelled (un-inspected) connection.
func (s *Server) emitTunnel(hostPort string, started time.Time, note string) {
	s.sink.Emit(&flow.Flow{
		ID:          flow.NextID(),
		Started:     started,
		Duration:    time.Since(started),
		Scheme:      "https",
		Method:      http.MethodConnect,
		Host:        hostPort,
		Intercepted: false,
		Error:       note,
	})
}

// hijack takes over the raw connection and returns a conn that drains any bytes
// the server already buffered past the CONNECT line. A client that pipelines its
// TLS ClientHello leaves it in that buffer; reading the socket directly would
// lose it and hang the handshake.
func hijack(w http.ResponseWriter) (net.Conn, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("connection does not support hijacking")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("hijacking connection: %w", err)
	}
	return &bufConn{Conn: conn, r: brw.Reader}, nil
}

// bufConn reads through the hijack's buffered reader (buffered bytes first, then
// the socket) while writes and connection controls pass straight to the socket.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// hostname strips the port from a "host:port" CONNECT target.
func hostname(hostPort string) string {
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		return h
	}
	return hostPort
}

// singleConnListener serves exactly one pre-established connection to http.Serve,
// then blocks Accept until closed so Serve exits cleanly when the conn is done.
type singleConnListener struct {
	conn   net.Conn
	handed chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newSingleConnListener(conn net.Conn) *singleConnListener {
	l := &singleConnListener{
		conn:   conn,
		handed: make(chan net.Conn, 1),
		closed: make(chan struct{}),
	}
	l.handed <- conn
	return l
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.handed:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *singleConnListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
