//go:build darwin

package origin

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// Criterion 8: the real libproc resolver attributes a live loopback connection to
// the process that owns its client end — here the test binary itself. Needs no
// simulator; it is the one test exercising the cgo path end to end.
func TestResolveFindsThisProcess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	server := <-accepted
	defer server.Close()

	// The proxy resolves from (accept-addr, client-addr). From the server side
	// that is (LocalAddr, RemoteAddr) of the accepted connection.
	p, err := Resolve(server.LocalAddr(), server.RemoteAddr())
	if err != nil {
		t.Fatalf("Resolve of a live loopback connection: %v", err)
	}
	if p.PID != os.Getpid() {
		t.Errorf("resolved PID = %d, want this test process %d", p.PID, os.Getpid())
	}
	// proc_pidpath returns the canonical path (/private/var/…); os.Executable may
	// return a symlinked form (/var/…), so compare after resolving symlinks.
	exe, _ := os.Executable()
	wantPath, _ := filepath.EvalSymlinks(exe)
	if gotPath, _ := filepath.EvalSymlinks(p.Path); gotPath != wantPath {
		t.Errorf("resolved path = %q, want %q", p.Path, exe)
	}
	if p.Simulator {
		t.Errorf("test binary must not classify as a simulator process: %+v", p)
	}
}

// A connection that has already closed cannot be attributed; Resolve errors
// rather than mis-attributing, and the proxy treats that as "not ours".
func TestResolveClosedConnectionErrors(t *testing.T) {
	local := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2} // no such live socket
	if _, err := Resolve(local, remote); err == nil {
		t.Error("Resolve should error when no live socket matches")
	}
}
