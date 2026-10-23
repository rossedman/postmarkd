package systemd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// Socket activation hands sockets to the process at fixed descriptors
// starting at 3. Inside a running test those numbers already belong to the
// runtime (the netpoller, the test log), so the activation scenarios run in
// TestMain, before anything else opens a file, using raw sockets that do not
// touch the netpoller and so take the lowest free descriptors. The Test
// functions in activation_test.go assert on the outcomes.

// activation records one call to Listeners.
type activation struct {
	setupErr  error      // the scenario could not be staged
	listeners []Listener // returned by Listeners
	err       error      // returned by Listeners
	env       []string   // LISTEN_* values left in the environment afterwards
	addrs     []string   // addresses of the staged sockets, in fd order
}

var (
	// activatedFailure: fd 3 is a TCP socket, fd 4 is /dev/null.
	activatedFailure activation
	// activatedDefaultName: one TCP socket with an empty name.
	activatedDefaultName activation
	// activated: a TCP and a unix socket, with fewer names than sockets.
	activated activation
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sd")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	// Order matters: the first call initializes the netpoller while fds 3
	// and 4 are occupied, so its descriptors land above them, and each
	// scenario except the last closes its listeners to free 3.. again.
	activatedFailure = activate([]stage{tcpSocket, devNull}, "")
	activatedDefaultName = activate([]stage{tcpSocket}, ":ignored")
	closeAll(activatedDefaultName.listeners)
	activated = activate([]stage{tcpSocket, unixSocket(filepath.Join(dir, "s"))}, "http")

	code := m.Run()
	closeAll(activated.listeners)
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// stage creates a descriptor without registering it with the netpoller and
// returns it along with a printable address.
type stage func() (fd int, addr string, err error)

func tcpSocket() (int, string, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return -1, "", err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		_ = syscall.Close(fd)
		return -1, "", err
	}
	if err := syscall.Listen(fd, 8); err != nil {
		_ = syscall.Close(fd)
		return -1, "", err
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		_ = syscall.Close(fd)
		return -1, "", err
	}
	return fd, "127.0.0.1:" + strconv.Itoa(sa.(*syscall.SockaddrInet4).Port), nil
}

func unixSocket(path string) stage {
	return func() (int, string, error) {
		fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		if err != nil {
			return -1, "", err
		}
		if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
			_ = syscall.Close(fd)
			return -1, "", err
		}
		if err := syscall.Listen(fd, 8); err != nil {
			_ = syscall.Close(fd)
			return -1, "", err
		}
		return fd, path, nil
	}
}

func devNull() (int, string, error) {
	fd, err := syscall.Open(os.DevNull, syscall.O_RDONLY, 0)
	return fd, os.DevNull, err
}

// activate stages the descriptors at 3.., sets LISTEN_* as systemd would and
// calls Listeners.
func activate(stages []stage, names string) activation {
	var a activation
	for i, s := range stages {
		target := listenFDsStart + i
		var st syscall.Stat_t
		if err := syscall.Fstat(target, &st); !errors.Is(err, syscall.EBADF) {
			a.setupErr = fmt.Errorf("fd %d is already in use, cannot stage socket activation", target)
			return a
		}
		fd, addr, err := s()
		if err != nil {
			a.setupErr = err
			return a
		}
		// POSIX allocates the lowest free descriptor, so with 3..target-1
		// staged and target free the new descriptor is target.
		if fd != target {
			_ = syscall.Close(fd)
			a.setupErr = fmt.Errorf("staged descriptor is %d, want %d", fd, target)
			return a
		}
		a.addrs = append(a.addrs, addr)
	}
	for k, v := range map[string]string{
		"LISTEN_PID":     strconv.Itoa(os.Getpid()),
		"LISTEN_FDS":     strconv.Itoa(len(stages)),
		"LISTEN_FDNAMES": names,
	} {
		if err := os.Setenv(k, v); err != nil {
			a.setupErr = err
			return a
		}
	}
	a.listeners, a.err = Listeners()
	for _, k := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		if v, ok := os.LookupEnv(k); ok {
			a.env = append(a.env, k+"="+v)
		}
	}
	return a
}

func closeAll(ls []Listener) {
	for _, l := range ls {
		_ = l.Close()
	}
}
