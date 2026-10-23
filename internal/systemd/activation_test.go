package systemd

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func checkStaged(t *testing.T, a activation) {
	t.Helper()
	if a.setupErr != nil {
		t.Fatalf("staging socket activation in TestMain failed: %v", a.setupErr)
	}
	if len(a.env) != 0 {
		t.Errorf("LISTEN_* left in the environment: %q", a.env)
	}
}

// acceptOne dials addr over network and checks l accepts the connection.
func acceptOne(t *testing.T, l net.Listener, network, addr string) {
	t.Helper()
	conn, err := net.Dial(network, addr)
	if err != nil {
		t.Fatalf("dial %s %s: %v", network, addr, err)
	}
	defer func() { _ = conn.Close() }()
	got, err := l.Accept()
	if err != nil {
		t.Fatalf("accept on %s: %v", addr, err)
	}
	_ = got.Close()
}

func TestListenersSocketActivation(t *testing.T) {
	a := activated
	checkStaged(t, a)
	if a.err != nil {
		t.Fatal(a.err)
	}
	if len(a.listeners) != 2 {
		t.Fatalf("got %d listeners, want 2", len(a.listeners))
	}
	tcp, unix := a.listeners[0], a.listeners[1]
	if tcp.Name != "http" {
		t.Errorf("first listener name = %q, want the LISTEN_FDNAMES entry", tcp.Name)
	}
	if unix.Name != "LISTEN_FD_4" {
		t.Errorf("unnamed listener name = %q, want LISTEN_FD_4", unix.Name)
	}
	if tcp.Addr().String() != a.addrs[0] || tcp.Addr().Network() != "tcp" {
		t.Errorf("tcp listener addr = %v, want %s", tcp.Addr(), a.addrs[0])
	}
	if unix.Addr().Network() != "unix" || unix.Addr().String() != a.addrs[1] {
		t.Errorf("unix listener addr = %v, want %s", unix.Addr(), a.addrs[1])
	}
	acceptOne(t, tcp, "tcp", a.addrs[0])
	acceptOne(t, unix, "unix", a.addrs[1])
}

func TestListenersEmptyNameFallsBack(t *testing.T) {
	a := activatedDefaultName
	checkStaged(t, a)
	if a.err != nil {
		t.Fatal(a.err)
	}
	if len(a.listeners) != 1 || a.listeners[0].Name != "LISTEN_FD_3" {
		t.Fatalf("listeners = %+v", a.listeners)
	}
	// TestMain closed it to free fd 3 for the next scenario.
	if _, err := net.Dial("tcp", a.addrs[0]); err == nil {
		t.Error("closed activated listener still accepts connections")
	}
}

func TestListenersNonSocketFails(t *testing.T) {
	a := activatedFailure
	checkStaged(t, a)
	if a.listeners != nil {
		t.Fatalf("listeners = %+v, want nil on error", a.listeners)
	}
	if a.err == nil || !strings.Contains(a.err.Error(), "socket activation: fd 4 (LISTEN_FD_4): ") {
		t.Fatalf("err = %v", a.err)
	}
	// The listener built from fd 3 before the failure must be closed.
	if _, err := net.Dial("tcp", a.addrs[0]); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dial to the partially activated listener: %v, want connection refused", err)
	}
}

func TestListenersIgnoredEnvironment(t *testing.T) {
	pid := strconv.Itoa(os.Getpid())
	tests := []struct {
		name, pid, fds string
	}{
		{"no pid", "", "1"},
		{"bad pid", "abc", "1"},
		{"other pid", "1", "1"},
		{"no fds", pid, ""},
		{"bad fds", pid, "two"},
		{"zero fds", pid, "0"},
		{"negative fds", pid, "-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("LISTEN_PID", tt.pid)
			t.Setenv("LISTEN_FDS", tt.fds)
			t.Setenv("LISTEN_FDNAMES", "http")
			ls, err := Listeners()
			if err != nil || ls != nil {
				t.Fatalf("Listeners = %v, %v", ls, err)
			}
			for _, k := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
				if _, ok := os.LookupEnv(k); ok {
					t.Errorf("%s should be unset", k)
				}
			}
		})
	}
}
