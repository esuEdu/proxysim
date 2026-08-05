package flow

// Sink consumes completed flows. Emit is called from the proxy request path, so
// implementations must return promptly and must not block: a slow or wedged
// sink has to degrade to dropping flows, never to stalling traffic. The console
// sink meets this by being synchronous and fast; anything slower belongs behind
// an AsyncSink.
type Sink interface {
	Emit(*Flow)
}

// MultiSink fans one flow out to several sinks, so console and file capture can
// run together without the proxy knowing there is more than one consumer. It is
// only as non-blocking as its slowest member; wrap slow members in an AsyncSink.
type MultiSink []Sink

// Emit delivers f to every sink in order.
func (m MultiSink) Emit(f *Flow) {
	for _, s := range m {
		s.Emit(f)
	}
}
