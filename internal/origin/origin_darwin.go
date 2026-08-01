//go:build darwin

package origin

/*
#include <libproc.h>
#include <sys/proc_info.h>
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"net"
	"unsafe"
)

// Resolve returns the process that owns the client end of a loopback connection,
// found by matching its TCP socket 4-tuple through libproc. It must be called
// while the connection is open: the originating socket is ephemeral, and a lookup
// after it closes finds nothing. Errors when the endpoints are not TCP or no live
// socket matches (the caller treats an error as "not ours" and tunnels).
func Resolve(local, remote net.Addr) (Process, error) {
	lt, ok1 := local.(*net.TCPAddr)
	rt, ok2 := remote.(*net.TCPAddr)
	if !ok1 || !ok2 {
		return Process{}, fmt.Errorf("origin: need TCP endpoints, got local %T remote %T", local, remote)
	}

	// From the proxy's view, remote is the client's ephemeral (its socket's local
	// port) and local is our listen port (its socket's foreign port). We look for
	// the process whose TCP socket has exactly that local/foreign pair.
	pid, path, err := findSocketOwner(rt.Port, lt.Port)
	if err != nil {
		return Process{}, err
	}
	return classify(pid, path), nil
}

// findSocketOwner scans every process for a TCP socket with the given local and
// foreign ports and returns its PID and executable path.
func findSocketOwner(localPort, foreignPort int) (int, string, error) {
	pids, err := listPIDs()
	if err != nil {
		return 0, "", err
	}
	for _, pid := range pids {
		if pid == 0 {
			continue
		}
		if pidOwnsSocket(pid, localPort, foreignPort) {
			return int(pid), pidPath(pid), nil
		}
	}
	return 0, "", fmt.Errorf("origin: no process owns loopback socket :%d->:%d (already closed?)", localPort, foreignPort)
}

// listPIDs returns all live process IDs.
func listPIDs() ([]C.int, error) {
	need := C.proc_listpids(C.PROC_ALL_PIDS, 0, nil, 0)
	if need <= 0 {
		return nil, fmt.Errorf("origin: proc_listpids sizing failed")
	}
	stride := C.int(unsafe.Sizeof(C.int(0)))
	// Over-allocate: the set can grow between the sizing and the fetch call.
	buf := make([]C.int, need/stride+64)
	got := C.proc_listpids(C.PROC_ALL_PIDS, 0, unsafe.Pointer(&buf[0]), C.int(len(buf))*stride)
	if got <= 0 {
		return nil, fmt.Errorf("origin: proc_listpids failed")
	}
	return buf[:int(got)/int(stride)], nil
}

// pidOwnsSocket reports whether pid holds a TCP socket with the given local and
// foreign ports.
func pidOwnsSocket(pid C.int, localPort, foreignPort int) bool {
	fdStride := C.int(unsafe.Sizeof(C.struct_proc_fdinfo{}))
	need := C.proc_pidinfo(pid, C.PROC_PIDLISTFDS, 0, nil, 0)
	if need <= 0 {
		return false
	}
	fds := make([]C.struct_proc_fdinfo, int(need)/int(fdStride)+16)
	got := C.proc_pidinfo(pid, C.PROC_PIDLISTFDS, 0, unsafe.Pointer(&fds[0]), C.int(len(fds))*fdStride)
	if got <= 0 {
		return false
	}
	fds = fds[:int(got)/int(fdStride)]

	for i := range fds {
		if uint32(fds[i].proc_fdtype) != uint32(C.PROX_FDTYPE_SOCKET) {
			continue
		}
		var si C.struct_socket_fdinfo
		n := C.proc_pidfdinfo(pid, fds[i].proc_fd, C.PROC_PIDFDSOCKETINFO, unsafe.Pointer(&si), C.int(unsafe.Sizeof(si)))
		if int(n) < int(unsafe.Sizeof(si)) {
			continue
		}
		if int(si.psi.soi_kind) != int(C.SOCKINFO_TCP) {
			continue
		}
		// soi_proto is a C union; reinterpret it as the TCP variant to reach the
		// in_sockinfo holding the ports (stored in network byte order).
		tcp := (*C.struct_tcp_sockinfo)(unsafe.Pointer(&si.psi.soi_proto))
		ini := tcp.tcpsi_ini
		if ntohs(uint16(ini.insi_lport)) == uint16(localPort) &&
			ntohs(uint16(ini.insi_fport)) == uint16(foreignPort) {
			return true
		}
	}
	return false
}

// pidPath returns pid's executable path, or "" if it cannot be read.
func pidPath(pid C.int) string {
	buf := make([]byte, C.PROC_PIDPATHINFO_MAXSIZE)
	n := C.proc_pidpath(pid, unsafe.Pointer(&buf[0]), C.uint32_t(len(buf)))
	if n <= 0 {
		return ""
	}
	return string(buf[:int(n)])
}

// ntohs swaps a 16-bit port from network to host byte order. macOS runs only on
// little-endian architectures, so the swap is unconditional.
func ntohs(n uint16) uint16 {
	return n<<8 | n>>8
}
