package systemd

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNotify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	t.Setenv("NOTIFY_SOCKET", path)

	if ok, err := Notify("READY=1"); !ok || err != nil {
		t.Fatalf("Notify = %v, %v", ok, err)
	}
	buf := make([]byte, 64)
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "READY=1" {
		t.Fatalf("got %q, %v", buf[:n], err)
	}
}

func TestNotifyWithoutSystemd(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if ok, err := Notify("READY=1"); ok || err != nil {
		t.Fatalf("Notify = %v, %v", ok, err)
	}
}

func TestListenersNotActivated(t *testing.T) {
	t.Setenv("LISTEN_PID", "1")
	t.Setenv("LISTEN_FDS", "1")
	ls, err := Listeners()
	if err != nil || ls != nil {
		t.Fatalf("Listeners = %v, %v", ls, err)
	}
	if os.Getenv("LISTEN_FDS") != "" {
		t.Error("LISTEN_FDS should be unset")
	}
}

func TestWatchdogInterval(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "30000000")
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()))
	if d := WatchdogInterval(); d != 30*time.Second {
		t.Fatalf("interval = %v", d)
	}
	t.Setenv("WATCHDOG_PID", "1")
	if d := WatchdogInterval(); d != 0 {
		t.Fatalf("interval for other pid = %v", d)
	}
}

func TestNotifyErrors(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		t.Setenv("NOTIFY_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
		if ok, err := Notify("READY=1"); ok || err == nil {
			t.Fatalf("Notify = %v, %v; want a dial error", ok, err)
		}
	})
	t.Run("big", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "n.sock")
		conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		t.Setenv("NOTIFY_SOCKET", path)
		if ok, err := Notify(strings.Repeat("x", 8<<20)); ok || err == nil {
			t.Fatalf("Notify = %v, %v; want a write error", ok, err)
		}
	})
}

func TestWatchdogIntervalTable(t *testing.T) {
	pid := strconv.Itoa(os.Getpid())
	tests := []struct {
		name, usec, pid string
		want            time.Duration
	}{
		{"unset", "", "", 0},
		{"invalid usec", "soon", "", 0},
		{"zero usec", "0", "", 0},
		{"negative usec", "-5", "", 0},
		{"no pid applies to us", "1500", "", 1500 * time.Microsecond},
		{"our pid", "2000000", pid, 2 * time.Second},
		{"other pid", "2000000", "1", 0},
		{"invalid pid", "2000000", "me", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("WATCHDOG_USEC", tt.usec)
			t.Setenv("WATCHDOG_PID", tt.pid)
			if got := WatchdogInterval(); got != tt.want {
				t.Fatalf("WatchdogInterval = %v, want %v", got, tt.want)
			}
		})
	}
}
