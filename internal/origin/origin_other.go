//go:build !darwin

package origin

import (
	"fmt"
	"net"
)

// Resolve is unsupported off macOS: the simulator, libproc, and the whole feature
// are macOS-only. It always errors, which the proxy treats as "not ours" — so on
// other platforms an active filter would tunnel everything. main.go only wires
// the filter in when the user asks for it, so a portable build still serves
// normally without one.
func Resolve(local, remote net.Addr) (Process, error) {
	return Process{}, fmt.Errorf("origin: process resolution is only supported on macOS")
}
