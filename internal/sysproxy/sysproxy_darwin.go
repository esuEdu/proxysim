//go:build darwin

package sysproxy

// supported gates the real takeover. On macOS the whole feature applies: the
// Simulator inherits the host system proxy, so pointing it at proxysim is exactly
// how simulator traffic reaches us.
const supported = true
