package proxy

import "sync/atomic"

// Stats is a point-in-time snapshot of the server's counters, served by the
// control API (spec 013). It is a plain struct rather than a view onto the live
// atomics so a caller rendering several fields cannot show numbers from different
// instants and have them contradict each other.
//
// The JSON tags are the control API's /stats contract, which is explicitly
// unstable: adding a field here must never require a client change, which is why
// /status is a separate, small, stable shape.
type Stats struct {
	// ActiveRequests counts plain-HTTP forwards in flight right now.
	ActiveRequests int64 `json:"activeRequests"`
	// ActiveTunnels counts open CONNECT tunnels, intercepted or blind.
	ActiveTunnels int64 `json:"activeTunnels"`

	TotalRequests int64 `json:"totalRequests"`
	TotalTunnels  int64 `json:"totalTunnels"`

	// InterceptedFlows and TunnelledFlows split the CONNECT total by outcome, which
	// is the number that answers "is this host pinned?" without reading the log.
	InterceptedFlows int64 `json:"interceptedFlows"`
	TunnelledFlows   int64 `json:"tunnelledFlows"`
}

// Active is what a host app shows as "connections": everything currently open,
// without making the caller know that proxysim tracks two kinds.
func (s Stats) Active() int64 {
	return s.ActiveRequests + s.ActiveTunnels
}

// counters holds the live atomics. Separate from Stats so the snapshot type stays
// copyable — atomic.Int64 must not be copied, and a Stats that could not be
// returned by value would be a trap for every caller.
//
// Everything here is reachable from the request path, so it is atomic rather than
// mutex-guarded: these are increments on a hot path, and `-race` is not optional
// in this repo.
type counters struct {
	activeRequests atomic.Int64
	activeTunnels  atomic.Int64

	totalRequests atomic.Int64
	totalTunnels  atomic.Int64

	interceptedFlows atomic.Int64
	tunnelledFlows   atomic.Int64
}

func (c *counters) snapshot() Stats {
	return Stats{
		ActiveRequests:   c.activeRequests.Load(),
		ActiveTunnels:    c.activeTunnels.Load(),
		TotalRequests:    c.totalRequests.Load(),
		TotalTunnels:     c.totalTunnels.Load(),
		InterceptedFlows: c.interceptedFlows.Load(),
		TunnelledFlows:   c.tunnelledFlows.Load(),
	}
}

// enterRequest records a plain-HTTP forward starting and returns the function that
// records it finishing.
//
// Returning the exit function — to be used with defer at the call site — is what
// makes the active count honest: an early return, or a panic recovered further out,
// still decrements. A counter that only grows is worse than no counter, because it
// looks like data.
func (c *counters) enterRequest() func() {
	c.activeRequests.Add(1)
	c.totalRequests.Add(1)
	return func() { c.activeRequests.Add(-1) }
}

// enterTunnel is the CONNECT equivalent. intercepted distinguishes a terminated
// TLS connection from a blind pass-through, counted once on entry so the split is
// recorded even for a tunnel that never closes cleanly.
func (c *counters) enterTunnel(intercepted bool) func() {
	c.activeTunnels.Add(1)
	c.totalTunnels.Add(1)
	if intercepted {
		c.interceptedFlows.Add(1)
	} else {
		c.tunnelledFlows.Add(1)
	}
	return func() { c.activeTunnels.Add(-1) }
}

// Stats returns a snapshot of the server's counters. Safe to call concurrently
// with serving, which is the only way it is ever called.
func (s *Server) Stats() Stats {
	return s.counters.snapshot()
}
