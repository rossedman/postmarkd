// Package systemd implements the small subset of the systemd service
// protocol postmarkd needs: socket activation (sd_listen_fds), readiness and
// status notification (sd_notify) and the service watchdog. It has no
// dependencies and is a no-op when not running under systemd.
package systemd

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// listenFDsStart is SD_LISTEN_FDS_START, the first passed file descriptor.
const listenFDsStart = 3

// Listener is a socket passed by systemd with its FileDescriptorName=
// (by default the name of the .socket unit).
type Listener struct {
	net.Listener
	Name string
}

// Listeners returns the sockets passed by systemd socket activation, or nil
// if the process was not socket activated. The LISTEN_* variables are removed
// from the environment so they are not inherited by child processes.
func Listeners() ([]Listener, error) {
	defer func() {
		os.Unsetenv("LISTEN_PID")
		os.Unsetenv("LISTEN_FDS")
		os.Unsetenv("LISTEN_FDNAMES")
	}()

	pid, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
	if err != nil || pid != os.Getpid() {
		return nil, nil
	}
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || n <= 0 {
		return nil, nil
	}
	names := strings.Split(os.Getenv("LISTEN_FDNAMES"), ":")

	listeners := make([]Listener, 0, n)
	for i := 0; i < n; i++ {
		fd := listenFDsStart + i
		syscall.CloseOnExec(fd)
		name := "LISTEN_FD_" + strconv.Itoa(fd)
		if i < len(names) && names[i] != "" {
			name = names[i]
		}
		f := os.NewFile(uintptr(fd), name)
		l, err := net.FileListener(f)
		f.Close() // FileListener dups the descriptor
		if err != nil {
			for _, l := range listeners {
				l.Close()
			}
			return nil, fmt.Errorf("socket activation: fd %d (%s): %w", fd, name, err)
		}
		listeners = append(listeners, Listener{l, name})
	}
	return listeners, nil
}

// Notify sends a state string such as "READY=1" to the service manager. It
// returns false without error when NOTIFY_SOCKET is not set.
func Notify(state string) (bool, error) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return false, nil
	}
	// Go maps a leading '@' to the Linux abstract socket namespace.
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(state)); err != nil {
		return false, err
	}
	return true, nil
}

// WatchdogInterval returns the watchdog timeout configured with WatchdogSec=,
// or 0 if the watchdog is not enabled for this process. Callers should send
// "WATCHDOG=1" at roughly half this interval.
func WatchdogInterval() time.Duration {
	usec, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec <= 0 {
		return 0
	}
	if p := os.Getenv("WATCHDOG_PID"); p != "" {
		if pid, err := strconv.Atoi(p); err != nil || pid != os.Getpid() {
			return 0
		}
	}
	return time.Duration(usec) * time.Microsecond
}
