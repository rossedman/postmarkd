package client

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestResolveAddr(t *testing.T) {
	cases := []struct {
		name, explicit, addr, listen, want string
	}{
		{"explicit wins", "/tmp/x.sock", "1.2.3.4:5", "127.0.0.1:9", "/tmp/x.sock"},
		{"env addr", "", "1.2.3.4:5", "127.0.0.1:9", "1.2.3.4:5"},
		{"listen unix prefix", "", "", "unix:/run/a.sock,127.0.0.1:9", "/run/a.sock"},
		{"listen absolute path", "", "", " /run/b.sock ", "/run/b.sock"},
		{"listen tcp host kept", "", "", "10.0.0.1:8025", "10.0.0.1:8025"},
		{"listen empty host", "", "", ":8025", "127.0.0.1:8025"},
		{"listen ipv4 wildcard", "", "", "0.0.0.0:8025", "127.0.0.1:8025"},
		{"listen ipv6 wildcard", "", "", "[::]:8025", "127.0.0.1:8025"},
		{"listen ipv6 host kept", "", "", "[::1]:8025", "[::1]:8025"},
		{"listen first of many", "", "", ",,127.0.0.2:1,/x.sock", "127.0.0.2:1"},
		{"listen blank falls back", "", "", " , ", DefaultSocket},
		{"default", "", "", "", DefaultSocket},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("POSTMARKD_ADDR", c.addr)
			t.Setenv("POSTMARKD_LISTEN", c.listen)
			if got := ResolveAddr(c.explicit); got != c.want {
				t.Errorf("ResolveAddr(%q) = %q, want %q", c.explicit, got, c.want)
			}
		})
	}
}

func TestNewBase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		addr, base string
		unix       bool
	}{
		{"/run/postmarkd.sock", "http://postmarkd", true},
		{"unix:/run/postmarkd.sock", "http://postmarkd", true},
		{"http://example.com:8025/", "http://example.com:8025", false},
		{"https://example.com", "https://example.com", false},
		{"127.0.0.1:8025", "http://127.0.0.1:8025", false},
	}
	for _, c := range cases {
		cl := New(c.addr, "")
		if cl.base != c.base {
			t.Errorf("New(%q).base = %q, want %q", c.addr, cl.base, c.base)
		}
		tr, ok := cl.http.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("transport is %T", cl.http.Transport)
		}
		if (tr.DialContext != nil) != c.unix {
			t.Errorf("New(%q): custom dialer = %v, want %v", c.addr, tr.DialContext != nil, c.unix)
		}
		if cl.http.Timeout <= 0 {
			t.Errorf("New(%q): no client timeout", c.addr)
		}
	}
}

// seen is what the test server observed about a request.
type seen struct {
	method, path, query string
	header              http.Header
	body                string
}

func recorder(t *testing.T, status int, reply string) (http.Handler, func() seen) {
	t.Helper()
	var mu sync.Mutex
	var last seen
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		mu.Lock()
		last = seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(b)}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	})
	return h, func() seen {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func TestDoTCP(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		token   string
		method  string
		path    string
		body    []byte
		header  http.Header
		status  int
		reply   string
		wantCT  string
		wantAut string
	}{
		{
			name: "send with token", token: "secret", method: "POST", path: "/email?async=1",
			body:   []byte(`{"From":"a@example.com"}`),
			header: http.Header{"X-Postmarkd-Stream": {"alerts"}},
			status: 200, reply: `{"ErrorCode":0}`,
			wantCT: "application/json", wantAut: "Bearer secret",
		},
		{
			name: "get without token or body", method: "GET", path: "/readyz",
			status: 503, reply: "not ready\n",
		},
		{
			name: "error response passed through", method: "POST", path: "/email/batch",
			body: []byte(`[]`), status: 422, reply: `{"ErrorCode":300,"Message":"batch contains no messages"}`,
			wantCT: "application/json",
		},
		{
			name: "caller cannot override token", token: "secret", method: "DELETE", path: "/blackhole/messages",
			header: http.Header{"Authorization": {"Bearer other"}},
			status: 204, wantAut: "Bearer secret",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, last := recorder(t, c.status, c.reply)
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)

			for _, addr := range []string{srv.URL, srv.URL + "/", strings.TrimPrefix(srv.URL, "http://")} {
				res, err := New(addr, c.token).Do(t.Context(), c.method, c.path, c.body, c.header)
				if err != nil {
					t.Fatalf("%s: %v", addr, err)
				}
				if res.StatusCode != c.status || string(res.Body) != c.reply {
					t.Errorf("%s: response = %d %q, want %d %q", addr, res.StatusCode, res.Body, c.status, c.reply)
				}
				got := last()
				wantPath, wantQuery, _ := strings.Cut(c.path, "?")
				if got.method != c.method || got.path != wantPath || got.query != wantQuery {
					t.Errorf("%s: request = %s %s?%s", addr, got.method, got.path, got.query)
				}
				if got.body != string(c.body) {
					t.Errorf("%s: body = %q, want %q", addr, got.body, c.body)
				}
				if ct := got.header.Get("Content-Type"); ct != c.wantCT {
					t.Errorf("%s: Content-Type = %q, want %q", addr, ct, c.wantCT)
				}
				if a := got.header.Get("Accept"); a != "application/json" {
					t.Errorf("%s: Accept = %q", addr, a)
				}
				if a := got.header.Get("Authorization"); a != c.wantAut {
					t.Errorf("%s: Authorization = %q, want %q", addr, a, c.wantAut)
				}
				for k, v := range c.header {
					if k == "Authorization" {
						continue
					}
					if g := got.header.Get(k); g != v[0] {
						t.Errorf("%s: header %s = %q, want %q", addr, k, g, v[0])
					}
				}
			}
		})
	}
}

// TestDoHeaderWithoutToken checks caller headers, including Authorization,
// are forwarded untouched when the client has no token of its own.
func TestDoHeaderWithoutToken(t *testing.T) {
	t.Parallel()
	h, last := recorder(t, 200, "ok")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	hdr := http.Header{"Authorization": {"Bearer from-caller"}, "X-Request-Id": {"abc"}}
	if _, err := New(srv.URL, "").Do(t.Context(), "GET", "/healthz", nil, hdr); err != nil {
		t.Fatal(err)
	}
	got := last().header
	if got.Get("Authorization") != "Bearer from-caller" || got.Get("X-Request-Id") != "abc" {
		t.Errorf("headers = %v", got)
	}
}

// shortSocketPath returns a socket path short enough for sun_path (104
// bytes on macOS); t.TempDir paths can exceed it.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pmc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func TestDoUnixSocket(t *testing.T) {
	t.Parallel()
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	h, last := recorder(t, http.StatusAccepted, `{"Message":"Queued"}`)
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	for _, addr := range []string{path, "unix:" + path} {
		res, err := New(addr, "tok").Do(t.Context(), "POST", "/email", []byte(`{}`), nil)
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		if res.StatusCode != http.StatusAccepted || string(res.Body) != `{"Message":"Queued"}` {
			t.Errorf("%s: response = %d %s", addr, res.StatusCode, res.Body)
		}
		got := last()
		if got.path != "/email" || got.body != "{}" || got.header.Get("Authorization") != "Bearer tok" {
			t.Errorf("%s: request = %+v", addr, got)
		}
	}
}

func TestDoUnreachable(t *testing.T) {
	t.Parallel()
	path := shortSocketPath(t) // never listened on
	_, err := New(path, "").Do(t.Context(), "GET", "/healthz", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot reach postmarkd") {
		t.Fatalf("err = %v", err)
	}
	var op *net.OpError
	if !errors.As(err, &op) {
		t.Errorf("underlying dial error not wrapped: %v", err)
	}
}

func TestDoCanceledContext(t *testing.T) {
	t.Parallel()
	h, _ := recorder(t, 200, "ok")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := New(srv.URL, "").Do(ctx, "GET", "/healthz", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestDoBadRequest(t *testing.T) {
	t.Parallel()
	if _, err := New("127.0.0.1:1", "").Do(t.Context(), "BAD METHOD", "/", nil, nil); err == nil {
		t.Fatal("invalid method accepted")
	}
	if _, err := New("127.0.0.1:1", "").Do(t.Context(), "GET", "/\x7f", nil, nil); err == nil {
		t.Fatal("invalid URL accepted")
	}
}

// TestDoTruncatedBody checks that a response cut short is an error rather
// than a silently truncated body.
func TestDoTruncatedBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "short")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	res, err := New(srv.URL, "").Do(t.Context(), "GET", "/", nil, nil)
	if err == nil {
		t.Fatalf("truncated body accepted: %q", res.Body)
	}
}
