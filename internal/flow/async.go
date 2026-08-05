package flow

import "sync/atomic"

// AsyncSink wraps a sink behind a buffered channel drained by one background
// goroutine, decoupling a slow consumer (a future UI) from the request path.
// When the buffer is full Emit drops the flow rather than blocking, and counts
// the drop — the spec's required degradation: lose flows, never stall traffic.
type AsyncSink struct {
	ch      chan *Flow
	wrapped Sink
	dropped atomic.Uint64
	done    chan struct{}
}

// NewAsyncSink starts draining wrapped in the background, buffering up to buffer
// flows. Call Close to stop the goroutine when done.
func NewAsyncSink(wrapped Sink, buffer int) *AsyncSink {
	a := &AsyncSink{
		ch:      make(chan *Flow, buffer),
		wrapped: wrapped,
		done:    make(chan struct{}),
	}
	go a.run()
	return a
}

func (a *AsyncSink) run() {
	defer close(a.done)
	for f := range a.ch {
		a.wrapped.Emit(f)
	}
}

// Emit hands f to the background drainer without blocking. If the buffer is
// full the flow is dropped and the drop counter incremented.
func (a *AsyncSink) Emit(f *Flow) {
	select {
	case a.ch <- f:
	default:
		a.dropped.Add(1)
	}
}

// Dropped reports how many flows have been dropped because the buffer was full.
func (a *AsyncSink) Dropped() uint64 {
	return a.dropped.Load()
}

// Close stops the background goroutine after the buffer drains. It must not be
// called concurrently with Emit.
func (a *AsyncSink) Close() {
	close(a.ch)
	<-a.done
}
