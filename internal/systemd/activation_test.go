package systemd

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// rawFD returns a descriptor for l that nothing else owns, as systemd
// would pass it, and closes l.
func rawFD(t *testing.T, l net.Listener) int {
	t.Helper()
	f, err := l.(interface{ File() (*os.File, error) }).File()
	if err != nil {
		t.Fatal(err)
	}
	// Hold ForkLock so a parallel test starting a process can't inherit
	// the descriptor before it is marked close-on-exec.
	syscall.ForkLock.RLock()
	fd, err := syscall.Dup(int(f.Fd()))
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	_ = l.Close()
	return fd
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

// shortDir returns a temporary directory short enough for a unix socket.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestFromFDs(t *testing.T) {
	t.Parallel()
	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpAddr := tcpL.Addr().String()
	sock := filepath.Join(shortDir(t), "s")
	unixL, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	unixL.(*net.UnixListener).SetUnlinkOnClose(false)
	fds := []int{rawFD(t, tcpL), rawFD(t, unixL)}

	// Fewer names than sockets: the second falls back to LISTEN_FD_<fd>.
	ls, err := fromFDs(fds, []string{"http"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, l := range ls {
			_ = l.Close()
		}
	}()
	if len(ls) != 2 {
		t.Fatalf("got %d listeners, want 2", len(ls))
	}
	if ls[0].Name != "http" {
		t.Errorf("first name = %q, want http", ls[0].Name)
	}
	if want := "LISTEN_FD_" + strconv.Itoa(fds[1]); ls[1].Name != want {
		t.Errorf("unnamed listener = %q, want %s", ls[1].Name, want)
	}
	if ls[0].Addr().Network() != "tcp" || ls[0].Addr().String() != tcpAddr {
		t.Errorf("tcp listener addr = %v, want %s", ls[0].Addr(), tcpAddr)
	}
	if ls[1].Addr().Network() != "unix" || ls[1].Addr().String() != sock {
		t.Errorf("unix listener addr = %v, want %s", ls[1].Addr(), sock)
	}
	acceptOne(t, ls[0], "tcp", tcpAddr)
	acceptOne(t, ls[1], "unix", sock)
}

func TestFromFDsEmptyNameFallsBack(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fd := rawFD(t, l)
	ls, err := fromFDs([]int{fd}, []string{""})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ls[0].Close() }()
	if want := "LISTEN_FD_" + strconv.Itoa(fd); ls[0].Name != want {
		t.Errorf("name = %q, want %s", ls[0].Name, want)
	}
}

func TestFromFDsNonSocketFails(t *testing.T) {
	t.Parallel()
	// A unix socket in a private directory, unlike a TCP port, can't be
	// taken over by a parallel test once it's closed, so a refused dial
	// proves fromFDs closed it.
	sock := filepath.Join(shortDir(t), "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	sockFD := rawFD(t, l)
	nullFD, err := syscall.Open(os.DevNull, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}

	ls, err := fromFDs([]int{sockFD, nullFD}, nil)
	if ls != nil {
		t.Fatalf("listeners = %+v, want nil on error", ls)
	}
	want := "socket activation: fd " + strconv.Itoa(nullFD) + " (LISTEN_FD_" + strconv.Itoa(nullFD) + "): "
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want it to contain %q", err, want)
	}
	// The listener built from the first descriptor must be closed.
	if _, err := net.Dial("unix", sock); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dial to the partially activated listener: %v, want connection refused", err)
	}
}

// helperEnv makes the test binary act as a socket-activated service.
const helperEnv = "POSTMARKD_SYSTEMD_TEST_HELPER"

// TestListenersActivated runs this test binary again as systemd would run
// a socket-activated service: the socket at fd 3, LISTEN_FDS and
// LISTEN_FDNAMES set, and LISTEN_PID naming the process. exec places
// ExtraFiles at fd 3 onwards, which a test cannot arrange in-process.
func TestListenersActivated(t *testing.T) {
	if os.Getenv(helperEnv) == "1" {
		activatedService(t)
		return
	}
	// Not parallel: forking briefly copies every descriptor in the
	// process, so a socket another test has just closed could stay open
	// in the child until it execs. Sequential tests run while parallel
	// ones are paused.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f, err := l.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	addr := l.Addr().String()
	_ = l.Close() // f keeps the socket open

	cmd := exec.Command(os.Args[0], "-test.run=^TestListenersActivated$", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=1", "LISTEN_FDS=1", "LISTEN_FDNAMES=http", "WANT_ADDR="+addr)
	cmd.ExtraFiles = []*os.File{f}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("activated process failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestListenersActivated") {
		t.Fatalf("activated process did not run the check:\n%s", out)
	}
}

// activatedService is the child side of TestListenersActivated.
func activatedService(t *testing.T) {
	// systemd sets LISTEN_PID to the service's pid, which the parent
	// cannot know before exec.
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	ls, err := Listeners()
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 1 || ls[0].Name != "http" {
		t.Fatalf("listeners = %+v", ls)
	}
	defer func() { _ = ls[0].Close() }()
	if got, want := ls[0].Addr().String(), os.Getenv("WANT_ADDR"); got != want {
		t.Fatalf("addr = %s, want %s", got, want)
	}
	acceptOne(t, ls[0], "tcp", os.Getenv("WANT_ADDR"))
	for _, k := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("%s should be unset", k)
		}
	}
}

// TestListenersInProcess runs Listeners against a socket the test owns, by
// moving the first passed descriptor to it. It must not run in parallel.
func TestListenersInProcess(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	old := listenFDsStart
	listenFDsStart = rawFD(t, l)
	t.Cleanup(func() { listenFDsStart = old })

	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("LISTEN_FDS", "1")
	t.Setenv("LISTEN_FDNAMES", "http:extra")
	ls, err := Listeners()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ls[0].Close() }()
	if len(ls) != 1 || ls[0].Name != "http" || ls[0].Addr().String() != addr {
		t.Fatalf("listeners = %+v", ls)
	}
	acceptOne(t, ls[0], "tcp", addr)
	for _, k := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("%s should be unset", k)
		}
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
