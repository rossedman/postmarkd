package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/rossedman/postmarkd/internal/domaincheck"
)

// testCLI returns a cli reading stdin and capturing stdout and stderr. stdin
// counts as piped when it is non-empty.
func testCLI(stdin string) (c *cli, stdout, stderr *bytes.Buffer) {
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	c = &cli{
		ctx:        context.Background(),
		stdin:      strings.NewReader(stdin),
		stdout:     stdout,
		stderr:     stderr,
		stdinPiped: func() bool { return stdin != "" },
		checkDomain: func(context.Context, domaincheck.Options) []domaincheck.Check {
			panic("checkDomain not stubbed")
		},
	}
	return c, stdout, stderr
}

// request is what the fake daemon received.
type request struct {
	method, path string
	header       http.Header
	body         map[string]any
}

// fakeDaemon is a stand-in for a running postmarkd that records requests
// and answers with a canned response.
type fakeDaemon struct {
	*httptest.Server
	mu     sync.Mutex
	reqs   []request
	status int
	body   string
}

// newFakeDaemon starts a fake daemon answering status and body, and points
// POSTMARKD_ADDR at it.
func newFakeDaemon(t *testing.T, status int, body string) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{status: status, body: body}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		req := request{method: r.Method, path: r.URL.Path, header: r.Header}
		if len(b) > 0 {
			if err := json.Unmarshal(b, &req.body); err != nil {
				t.Errorf("request body is not JSON: %s", b)
			}
		}
		d.mu.Lock()
		d.reqs = append(d.reqs, req)
		d.mu.Unlock()
		w.WriteHeader(d.status)
		_, _ = io.WriteString(w, d.body)
	}))
	t.Cleanup(d.Close)
	t.Setenv("POSTMARKD_ADDR", d.URL)
	t.Setenv("POSTMARKD_TOKEN", "")
	return d
}

// last returns the most recent request, failing the test if there was none.
func (d *fakeDaemon) last(t *testing.T) request {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.reqs) == 0 {
		t.Fatal("no request reached the daemon")
	}
	return d.reqs[len(d.reqs)-1]
}

// unreachable points POSTMARKD_ADDR at a closed port.
func unreachable(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	t.Setenv("POSTMARKD_ADDR", srv.URL)
}

func TestRunDispatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		desc     string
		argv     []string
		code     int
		out, err string
	}{
		{"version", []string{"postmarkd", "version"}, 0, "postmarkd dev\n", ""},
		{"help", []string{"postmarkd", "help"}, 0, "Usage:", ""},
		{"unknown", []string{"postmarkd", "bogus"}, 2, "", `unknown command "bogus"`},
		{"flags go to serve", []string{"postmarkd", "-version"}, 0, "postmarkd dev\n", ""},
		{"serve", []string{"postmarkd", "serve", "-version"}, 0, "postmarkd dev\n", ""},
		{"send help", []string{"postmarkd", "send", "-h"}, 0, "", "Usage: postmarkd send"},
		{"sendmail command", []string{"postmarkd", "sendmail", "-bp"}, exUsage, "", "-bp is not supported"},
		{"invoked as sendmail", []string{"/usr/sbin/sendmail", "-bp"}, exUsage, "", "-bp is not supported"},
		{"check-domain", []string{"postmarkd", "check-domain"}, 2, "", "Usage: postmarkd check-domain"},
		{"healthcheck", []string{"postmarkd", "healthcheck", "-nope"}, 2, "", "flag provided but not defined"},
	}
	for _, tc := range cases {
		c, stdout, stderr := testCLI("")
		if code := c.run(tc.argv); code != tc.code {
			t.Errorf("%s: exit %d, want %d (stderr %q)", tc.desc, code, tc.code, stderr)
		}
		if !strings.Contains(stdout.String(), tc.out) {
			t.Errorf("%s: stdout %q, want it to contain %q", tc.desc, stdout, tc.out)
		}
		if !strings.Contains(stderr.String(), tc.err) {
			t.Errorf("%s: stderr %q, want it to contain %q", tc.desc, stderr, tc.err)
		}
	}
}

func TestParse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want int
	}{
		{nil, -1},
		{[]string{"-v"}, -1},
		{[]string{"-h"}, 0},
		{[]string{"-help"}, 0},
		{[]string{"-x"}, 2},
		{[]string{"-v=maybe"}, 2},
	} {
		fl := flag.NewFlagSet("t", flag.ContinueOnError)
		fl.SetOutput(io.Discard)
		fl.Bool("v", false, "")
		if got := parse(fl, tc.args); got != tc.want {
			t.Errorf("parse(%q) = %d, want %d", tc.args, got, tc.want)
		}
	}
}

// TestStdinPiped swaps os.Stdin, so it must not run in parallel with tests
// that read it.
func TestStdinPiped(t *testing.T) {
	old := os.Stdin
	t.Cleanup(func() { os.Stdin = old })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	os.Stdin = r
	if !stdinPiped() {
		t.Error("a pipe: stdinPiped() = false, want true")
	}

	_ = r.Close()
	if stdinPiped() {
		t.Error("a closed file: stdinPiped() = true, want false")
	}
}
