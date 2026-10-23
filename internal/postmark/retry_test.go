package postmark

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewDefaults(t *testing.T) {
	t.Parallel()
	c := New(Options{MaxConcurrency: -3, Timeout: -time.Second})
	if c.base != DefaultBaseURL {
		t.Errorf("base = %q", c.base)
	}
	if cap(c.sem) != 1 {
		t.Errorf("concurrency = %d, want 1", cap(c.sem))
	}
	if c.timeout != 30*time.Second {
		t.Errorf("timeout = %v", c.timeout)
	}
	if c.InFlight() != 0 {
		t.Errorf("InFlight = %d", c.InFlight())
	}

	c = New(Options{BaseURL: "http://x", MaxConcurrency: 7, Timeout: time.Second, MaxRetries: 4})
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", c.http.Transport)
	}
	if c.base != "http://x" || cap(c.sem) != 7 || c.timeout != time.Second || c.maxRetries != 4 {
		t.Errorf("options not applied: %+v", c)
	}
	if tr.MaxConnsPerHost != 7 || tr.MaxIdleConnsPerHost != 7 || tr.MaxIdleConns != 7 {
		t.Errorf("transport pool not sized to concurrency: %+v", tr)
	}
}

func TestRequestAndResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method != http.MethodPost, r.URL.Path != "/email/batch":
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		case r.Header.Get("X-Postmark-Server-Token") != "tok":
			t.Errorf("token %q", r.Header.Get("X-Postmark-Server-Token"))
		case r.Header.Get("Accept") != "application/json", r.Header.Get("Content-Type") != "application/json":
			t.Errorf("headers %v", r.Header)
		case r.Header.Get("User-Agent") != "postmarkd/test":
			t.Errorf("user agent %q", r.Header.Get("User-Agent"))
		case string(body) != `[{"To":"a@b"}]`:
			t.Errorf("body %q", body)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"ErrorCode":300,"Message":"Invalid email request"}`))
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, ServerToken: "tok", UserAgent: "postmarkd/test", MaxRetries: 3})
	res, err := c.Post(t.Context(), "/email/batch", []byte(`[{"To":"a@b"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 422 || res.Attempts != 1 || res.ContentType != "application/json; charset=utf-8" ||
		string(res.Body) != `{"ErrorCode":300,"Message":"Invalid email request"}` {
		t.Fatalf("res = %+v (body %q)", res, res.Body)
	}
	if c.InFlight() != 0 {
		t.Fatalf("slot not released: InFlight = %d", c.InFlight())
	}
}

func TestDefaultUserAgentWhenUnset(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "Go-http-client/") {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
	}))
	defer srv.Close()
	if _, err := newTestClient(srv.URL, 0).Post(t.Context(), "/email", nil); err != nil {
		t.Fatal(err)
	}
}

func TestResponseBodyIsCapped(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxResponseBytes+100))
	}))
	defer srv.Close()
	res, err := newTestClient(srv.URL, 0).Post(t.Context(), "/email", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Body) != maxResponseBytes {
		t.Fatalf("body length %d, want %d", len(res.Body), maxResponseBytes)
	}
}

func TestRetryOnEveryRetryableStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			res, err := newTestClient(srv.URL, 1).Post(t.Context(), "/email", nil)
			if err != nil || res.StatusCode != 200 || res.Attempts != 2 || calls.Load() != 2 {
				t.Fatalf("err=%v res=%+v calls=%d", err, res, calls.Load())
			}
		})
	}
}

func TestNoRetriesConfigured(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	res, err := newTestClient(srv.URL, 0).Post(t.Context(), "/email", nil)
	if err != nil || res.StatusCode != 503 || res.Attempts != 1 || calls.Load() != 1 {
		t.Fatalf("err=%v res=%+v calls=%d", err, res, calls.Load())
	}
}

// TestRetryAfter checks how the Retry-After header stretches the wait
// between attempts. Backoff is 1ms, so a honoured Retry-After is the only
// way the wait can outlast the context deadline.
func TestRetryAfter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		header       string
		honoured     bool
		wantAttempts int
	}{
		{"30", true, 1},
		{"59", true, 1},
		{"60", false, 3}, // a minute or more is ignored
		{"3600", false, 3},
		{"0", false, 3},
		{"-5", false, 3},
		{"soon", false, 3},
		{"Wed, 21 Oct 2015 07:28:00 GMT", false, 3}, // HTTP-date form is not supported
		{"", false, 3},
	}
	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if tt.header != "" {
					w.Header().Set("Retry-After", tt.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer srv.Close()

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			start := time.Now()
			res, err := newTestClient(srv.URL, 2).Post(ctx, "/email", nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != 429 || res.Attempts != tt.wantAttempts || int(calls.Load()) != tt.wantAttempts {
				t.Fatalf("res=%+v calls=%d, want %d attempts", res, calls.Load(), tt.wantAttempts)
			}
			if elapsed := time.Since(start); tt.honoured != (elapsed >= 2*time.Second) {
				t.Fatalf("elapsed %v, honoured=%v", elapsed, tt.honoured)
			}
		})
	}
}

func TestRetryAfterExtendsShortBackoff(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	start := time.Now()
	res, err := newTestClient(srv.URL, 1).Post(t.Context(), "/email", nil)
	if err != nil || res.StatusCode != 200 || res.Attempts != 2 {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("retried after %v, before Retry-After: 1", elapsed)
	}
}

func TestBackoffLongerThanRetryAfterWins(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL, 1)
	ctx, cancel := context.WithCancel(t.Context())
	var waits []int
	c.backoff = func(attempt int) time.Duration {
		waits = append(waits, attempt)
		cancel() // observe the wait, then abandon it
		return time.Hour
	}
	res, err := c.Post(ctx, "/email", nil)
	if err != nil || res.StatusCode != 503 || res.Attempts != 1 || len(waits) != 1 || waits[0] != 0 {
		t.Fatalf("err=%v res=%+v waits=%v", err, res, waits)
	}
}

func TestCancelDuringBackoff(t *testing.T) {
	t.Parallel()
	t.Run("after a response returns it", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()
		c := newTestClient(srv.URL, 5)
		ctx, cancel := context.WithCancel(t.Context())
		c.backoff = func(int) time.Duration { cancel(); return time.Hour }
		res, err := c.Post(ctx, "/email", nil)
		if err != nil || res == nil || res.StatusCode != 502 || res.Attempts != 1 {
			t.Fatalf("err=%v res=%+v", err, res)
		}
	})
	t.Run("after a dial error returns the context error", func(t *testing.T) {
		t.Parallel()
		c := newTestClient(closedURL(t), 5)
		ctx, cancel := context.WithCancel(t.Context())
		c.backoff = func(int) time.Duration { cancel(); return time.Hour }
		res, err := c.Post(ctx, "/email", nil)
		if !errors.Is(err, context.Canceled) || res != nil {
			t.Fatalf("err=%v res=%+v", err, res)
		}
	})
}

func TestBackoffSchedule(t *testing.T) {
	t.Parallel()
	var attempts []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL, 3)
	c.backoff = func(a int) time.Duration { attempts = append(attempts, a); return 0 }
	res, err := c.Post(t.Context(), "/email", nil)
	if err != nil || res.Attempts != 4 {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	if len(attempts) != 3 || attempts[0] != 0 || attempts[1] != 1 || attempts[2] != 2 {
		t.Fatalf("backoff called with %v", attempts)
	}
}

func TestDefaultBackoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		attempt int
		max     time.Duration
	}{
		{0, 250 * time.Millisecond},
		{1, 500 * time.Millisecond},
		{2, time.Second},
		{3, 2 * time.Second},
		{4, 4 * time.Second},
		{5, 8 * time.Second},
		{6, 10 * time.Second},
		{7, 10 * time.Second},
		{63, 10 * time.Second},
		{1 << 30, 10 * time.Second},
	}
	for _, tt := range tests {
		for range 200 {
			d := defaultBackoff(tt.attempt)
			if d < tt.max/2 || d > tt.max {
				t.Fatalf("defaultBackoff(%d) = %v, want in [%v, %v]", tt.attempt, d, tt.max/2, tt.max)
			}
		}
	}
	if New(Options{}).backoff == nil {
		t.Fatal("New should install a backoff")
	}
}

func TestWaitForSlotHonoursContext(t *testing.T) {
	t.Parallel()
	arrived := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		arrived <- struct{}{}
		<-release
	}))
	defer srv.Close()
	c := newTestClient(srv.URL, 0) // MaxConcurrency 2

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := c.Post(context.Background(), "/email", nil)
			errs <- err
		}()
	}
	<-arrived
	<-arrived
	if c.InFlight() != 2 {
		t.Fatalf("InFlight = %d, want 2", c.InFlight())
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if res, err := c.Post(ctx, "/email", nil); !errors.Is(err, context.Canceled) || res != nil {
		t.Fatalf("third Post = %v, %v; want context.Canceled", res, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("server saw %d requests, want 2", calls.Load())
	}

	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if c.InFlight() != 0 {
		t.Fatalf("InFlight after completion = %d", c.InFlight())
	}
}

func TestErrors(t *testing.T) {
	t.Parallel()
	t.Run("invalid base URL", func(t *testing.T) {
		t.Parallel()
		_, err := newTestClient("http://bad host", 3).Post(t.Context(), "/email", nil)
		if err == nil || !strings.Contains(err.Error(), "postmark /email: ") || !strings.Contains(err.Error(), "(after 1 attempt(s))") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("truncated body is not retried", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("short"))
		}))
		defer srv.Close()
		_, err := newTestClient(srv.URL, 3).Post(t.Context(), "/email", nil)
		if err == nil || !strings.Contains(err.Error(), "reading response: ") || !errors.Is(err, io.ErrUnexpectedEOF) || calls.Load() != 1 {
			t.Fatalf("err=%v calls=%d", err, calls.Load())
		}
	})
	t.Run("connection dropped after request is not retried", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("cannot hijack")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		}))
		defer srv.Close()
		_, err := newTestClient(srv.URL, 3).Post(t.Context(), "/email", []byte("{}"))
		if err == nil || !strings.Contains(err.Error(), "(after 1 attempt(s))") || calls.Load() != 1 {
			t.Fatalf("err=%v calls=%d", err, calls.Load())
		}
	})
	t.Run("attempt timeout is not retried", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			<-r.Context().Done()
		}))
		defer srv.Close()
		c := New(Options{BaseURL: srv.URL, MaxRetries: 3, Timeout: 50 * time.Millisecond})
		c.backoff = func(int) time.Duration { return 0 }
		_, err := c.Post(t.Context(), "/email", nil)
		if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
			t.Fatalf("err=%v calls=%d", err, calls.Load())
		}
	})
}

func TestRetryableErr(t *testing.T) {
	t.Parallel()
	if retryableErr(nil) || retryableErr(errors.New("x")) || retryableErr(context.DeadlineExceeded) {
		t.Fatal("non-dial errors must not be retryable")
	}
	if retryableStatus(nil) || retryableStatus(&Response{StatusCode: 500}) || !retryableStatus(&Response{StatusCode: 429}) {
		t.Fatal("retryableStatus")
	}
}

// closedURL returns the URL of a server that has been shut down, so dialing
// it is refused.
func closedURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}
