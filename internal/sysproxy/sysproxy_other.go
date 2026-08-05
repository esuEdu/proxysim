//go:build !darwin

package sysproxy

// supported is false off macOS: there is no Simulator and no networksetup, so the
// takeover is a no-op and Apply returns a do-nothing restore. main only wires the
// Manager in when the user asks for it, so a portable build still compiles and
// serves normally without one.
const supported = false
