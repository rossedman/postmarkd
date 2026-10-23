package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossedman/postmarkd/internal/client"
	"github.com/rossedman/postmarkd/internal/config"
)

// shortDir returns a temporary directory with a path short enough for unix
// sockets (macOS allows 104 bytes; t.TempDir paths can exceed that).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// syncBuffer is a bytes.Buffer safe for concurrent writes and reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Write appends p.
func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// String returns everything written so far.
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// notifySocket listens where systemd would and collects every state sent.
type notifySocket struct {
	mu     sync.Mutex
	states []string
}

// listenNotify points NOTIFY_SOCKET at a new datagram socket.
func listenNotify(t *testing.T, dir string) *notifySocket {
	t.Helper()
	path := filepath.Join(dir, "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	t.Setenv("NOTIFY_SOCKET", path)
	n := &notifySocket{}
	go func() {
		buf := make([]byte, 4096)
		for {
			k, err := conn.Read(buf)
			if err != nil {
				return
			}
			n.mu.Lock()
			n.states = append(n.states, string(buf[:k]))
			n.mu.Unlock()
		}
	}()
	return n
}

// all returns the states received so far, joined by newlines.
func (n *notifySocket) all() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return strings.Join(n.states, "\n")
}

// unsetenv removes key from the environment for the rest of the test.
func unsetenv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "") // restores the old value afterwards
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls cond every 10ms for up to 5s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startServe runs serve with args in the background. It returns a function
// that stops it and returns its exit status.
func startServe(t *testing.T, args ...string) (stop func() int, stdout, stderr *syncBuffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr = &syncBuffer{}, &syncBuffer{}
	c := &cli{ctx: ctx, stdin: strings.NewReader(""), stdout: stdout, stderr: stderr}
	done := make(chan int, 1)
	go func() { done <- c.serve(args) }()
	stop = func() int {
		cancel()
		select {
		case code := <-done:
			return code
		case <-time.After(10 * time.Second):
			t.Fatal("serve did not stop")
			return -1
		}
	}
	t.Cleanup(func() { cancel() })
	return stop, stdout, stderr
}

func TestServeBlackhole(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "api.sock")
	smtpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	smtpAddr := smtpLn.Addr().String()
	_ = smtpLn.Close()

	t.Setenv("POSTMARKD_BLACKHOLE", "true")
	t.Setenv("POSTMARKD_LISTEN", sock)
	t.Setenv("POSTMARKD_SMTP_LISTEN", smtpAddr)
	t.Setenv("POSTMARKD_SMTP_HOSTNAME", "")
	t.Setenv("POSTMARKD_DEFAULT_FROM", "app@example.com")
	t.Setenv("POSTMARKD_LOG_FORMAT", "json")
	t.Setenv("JOURNAL_STREAM", "8:1234")
	t.Setenv("WATCHDOG_USEC", "20000")
	t.Setenv("WATCHDOG_PID", "")
	notified := listenNotify(t, dir)

	stop, _, stderr := startServe(t)
	api := client.New(sock, "")
	waitFor(t, "readiness", func() bool {
		res, err := api.Do(context.Background(), http.MethodGet, "/readyz", nil, nil)
		return err == nil && res.StatusCode == http.StatusOK
	})

	res, err := api.Do(context.Background(), http.MethodPost, "/email", []byte(`{"To":"a@example.com","Subject":"api","TextBody":"hi"}`), nil)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("send over the API: %v %+v", err, res)
	}
	if err := smtp.SendMail(smtpAddr, nil, "app@example.com", []string{"b@example.com"}, []byte("Subject: smtp\r\n\r\nhi\r\n")); err != nil {
		t.Fatalf("send over SMTP: %v", err)
	}
	res, err = api.Do(context.Background(), http.MethodGet, "/blackhole/messages", nil, nil)
	if err != nil || !strings.Contains(string(res.Body), `"Subject":"api"`) || !strings.Contains(string(res.Body), `"Subject":"smtp"`) {
		t.Fatalf("blackhole messages: %v %s", err, res.Body)
	}
	waitFor(t, "a watchdog ping", func() bool { return strings.Contains(notified.all(), "WATCHDOG=1") })

	if code := stop(); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	states := notified.all()
	for _, want := range []string{"READY=1", "STATUS=Accepting mail on " + sock + ", smtp " + smtpAddr + " (BLACKHOLE MODE)", "STOPPING=1"} {
		if !strings.Contains(states, want) {
			t.Errorf("notify states missing %q:\n%s", want, states)
		}
	}
	logs := stderr.String()
	for _, want := range []string{`"msg":"BLACKHOLE MODE`, `"msg":"postmarkd started"`, `"msg":"stopped"`, `"address":"unix:` + sock} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, `"time":`) {
		t.Errorf("logs under journald have timestamps:\n%s", logs)
	}
}

func TestServeUpstream(t *testing.T) {
	var got sync.Map
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store("token", r.Header.Get("X-Postmark-Server-Token"))
		got.Store("ua", r.Header.Get("User-Agent"))
		_, _ = io.WriteString(w, okResponse)
	}))
	t.Cleanup(upstream.Close)

	dir := shortDir(t)
	sock := filepath.Join(dir, "api.sock")
	t.Setenv("POSTMARKD_SERVER_TOKEN", "server-token")
	t.Setenv("POSTMARKD_API_URL", upstream.URL)
	t.Setenv("POSTMARKD_LISTEN", sock)
	t.Setenv("POSTMARKD_SMTP_LISTEN", "")
	t.Setenv("POSTMARKD_DEFAULT_FROM", "app@example.com")
	t.Setenv("JOURNAL_STREAM", "")
	t.Setenv("NOTIFY_SOCKET", filepath.Join(dir, "nobody-listening"))

	stop, _, stderr := startServe(t)
	api := client.New(sock, "")
	waitFor(t, "readiness", func() bool {
		res, err := api.Do(context.Background(), http.MethodGet, "/readyz", nil, nil)
		return err == nil && res.StatusCode == http.StatusOK
	})
	res, err := api.Do(context.Background(), http.MethodPost, "/email", []byte(`{"To":"a@example.com","TextBody":"hi"}`), nil)
	if err != nil || res.StatusCode != http.StatusOK || !strings.Contains(string(res.Body), "b7bc2f4a") {
		t.Fatalf("send: %v %+v", err, res)
	}
	if tok, _ := got.Load("token"); tok != "server-token" {
		t.Errorf("upstream token = %v", tok)
	}
	if ua, _ := got.Load("ua"); ua != "postmarkd/dev" {
		t.Errorf("upstream User-Agent = %v", ua)
	}
	res, err = api.Do(context.Background(), http.MethodGet, "/metrics", nil, nil)
	if err != nil || !strings.Contains(string(res.Body), "postmarkd_upstream_in_flight 0") || !strings.Contains(string(res.Body), "postmarkd_blackhole_mode 0") {
		t.Errorf("metrics: %v\n%s", err, res.Body)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	// A broken NOTIFY_SOCKET is logged, not fatal.
	if !strings.Contains(stderr.String(), "sd_notify failed") {
		t.Errorf("logs:\n%s", stderr)
	}
}

func TestServeStartupFailures(t *testing.T) {
	dir := shortDir(t)
	cfgFile := writeFile(t, dir, "postmarkd.conf", "POSTMARKD_BLACKHOLE=true\nPOSTMARKD_LISTEN="+filepath.Join(dir, "c.sock")+"\n")
	badCfg := writeFile(t, dir, "bad.conf", "POSTMARKD_NOPE=1\n")
	for _, tc := range []struct {
		desc           string
		env            map[string]string
		args           []string
		code           int
		stdout, stderr string
	}{
		{"version", nil, []string{"-version"}, 0, "postmarkd dev\n", ""},
		{"help", nil, []string{"-h"}, 0, "", "Daemon flags:"},
		{"bad flag", nil, []string{"-nope"}, 2, "", "flag provided but not defined"},
		{"check-config", map[string]string{"POSTMARKD_BLACKHOLE": "true"}, []string{"-check-config"}, 0, "configuration OK\n", ""},
		{"config file", nil, []string{"-config", cfgFile, "-check-config"}, 0, "configuration OK\n", ""},
		{"bad config file", nil, []string{"-config", badCfg}, 2, "", `unknown key "POSTMARKD_NOPE"`},
		{"invalid", map[string]string{"POSTMARKD_BLACKHOLE": "true", "POSTMARKD_MAX_RETRIES": "-1"}, nil, 2, "", "invalid configuration:\n  "},
		{"cannot listen", map[string]string{"POSTMARKD_BLACKHOLE": "true", "POSTMARKD_LISTEN": filepath.Join(dir, "missing", "s.sock")}, nil, 1, "", "cannot listen"},
		{"cannot listen for SMTP", map[string]string{
			"POSTMARKD_BLACKHOLE": "true", "POSTMARKD_LISTEN": filepath.Join(dir, "ok.sock"),
			"POSTMARKD_SMTP_LISTEN": filepath.Join(dir, "missing", "smtp.sock"),
		}, nil, 1, "", "cannot listen"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			for _, k := range []string{"POSTMARKD_BLACKHOLE", "POSTMARKD_SERVER_TOKEN", "POSTMARKD_LISTEN", "POSTMARKD_SMTP_LISTEN", "POSTMARKD_MAX_RETRIES"} {
				unsetenv(t, k)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c, stdout, stderr := testCLI("")
			if code := c.serve(tc.args); code != tc.code {
				t.Errorf("exit %d, want %d\n%s", code, tc.code, stderr)
			}
			if !strings.Contains(stdout.String(), tc.stdout) || !strings.Contains(stderr.String(), tc.stderr) {
				t.Errorf("stdout %q, stderr %q; want %q, %q", stdout, stderr, tc.stdout, tc.stderr)
			}
		})
	}
	// The HTTP socket bound before SMTP failed was closed again.
	if _, err := net.Dial("unix", filepath.Join(dir, "ok.sock")); err == nil {
		t.Error("HTTP listener left open after SMTP bind failed")
	}
}

func TestRemoveStaleSocket(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)

	if err := removeStaleSocket(filepath.Join(dir, "absent")); err != nil {
		t.Errorf("absent: %v", err)
	}

	file := writeFile(t, dir, "file", "x")
	if err := removeStaleSocket(file); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("regular file: %v", err)
	}
	if err := removeStaleSocket(filepath.Join(file, "under")); err == nil {
		t.Error("path under a file: want error")
	}

	live := filepath.Join(dir, "live.sock")
	l, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err := removeStaleSocket(live); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("live socket: %v", err)
	}

	stale := filepath.Join(dir, "stale.sock")
	ul, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ul.SetUnlinkOnClose(false)
	_ = ul.Close()
	if err := removeStaleSocket(stale); err != nil {
		t.Errorf("stale socket: %v", err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Errorf("stale socket still there: %v", err)
	}
}

func TestBind(t *testing.T) {
	t.Parallel()
	l, err := bind(config.Addr{Network: "tcp", Address: "127.0.0.1:0"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()

	dir := shortDir(t)
	sock := filepath.Join(dir, "s.sock")
	l, err = bind(config.Addr{Network: "unix", Address: sock}, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	fi, err := os.Stat(sock)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode: %v %v", fi.Mode(), err)
	}
	// A second bind finds the socket in use.
	if _, err := bind(config.Addr{Network: "unix", Address: sock}, 0o600); err == nil {
		t.Error("binding a live socket: want error")
	}
}

func TestNewLogger(t *testing.T) {
	t.Setenv("JOURNAL_STREAM", "")
	var buf bytes.Buffer
	log := newLogger(config.Config{LogLevel: slog.LevelWarn, LogFormat: "text"}, &buf)
	log.Info("hidden")
	log.Warn("shown", "k", "v")
	out := buf.String()
	if strings.Contains(out, "hidden") || !strings.Contains(out, "msg=shown k=v") || !strings.Contains(out, "time=") {
		t.Errorf("text log = %q", out)
	}
}

func TestIndent(t *testing.T) {
	t.Parallel()
	if got := indent("a\nb"); got != "  a\n  b" {
		t.Errorf("indent = %q", got)
	}
}
