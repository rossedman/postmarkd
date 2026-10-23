package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossedman/postmarkd/internal/blackhole"
	"github.com/rossedman/postmarkd/internal/config"
	"github.com/rossedman/postmarkd/internal/metrics"
	"github.com/rossedman/postmarkd/internal/postmark"
	"github.com/rossedman/postmarkd/internal/queue"
	"github.com/rossedman/postmarkd/internal/smtpd"
)

const validEmail = `{"From":"f@example.com","To":"a@example.com","TextBody":"x"}`

// stubSender is a Sender whose behaviour is a function.
type stubSender func(ctx context.Context, path string, body []byte) (*postmark.Response, error)

func (f stubSender) Post(ctx context.Context, path string, body []byte) (*postmark.Response, error) {
	return f(ctx, path, body)
}

func reply(status int, body string) stubSender {
	return func(context.Context, string, []byte) (*postmark.Response, error) {
		return &postmark.Response{StatusCode: status, ContentType: "application/json", Body: []byte(body), Attempts: 1}, nil
	}
}

// logBuf is a goroutine-safe log sink.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// stubServer builds a Server around sender with a debug-level text logger.
func stubServer(t *testing.T, sender Sender, mutate func(*config.Config)) (*Server, *metrics.Metrics, *logBuf) {
	t.Helper()
	cfg := config.Default()
	cfg.QueueSize = 10
	cfg.Workers = 1
	if mutate != nil {
		mutate(&cfg)
	}
	m := metrics.New()
	logs := &logBuf{}
	s := New(cfg, sender, m, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s, m, logs
}

// funcServer is stubServer for a sender written as a function literal.
func funcServer(t *testing.T, f stubSender, mutate func(*config.Config)) (*Server, *metrics.Metrics, *logBuf) {
	t.Helper()
	return stubServer(t, f, mutate)
}

func metricsText(m *metrics.Metrics) string {
	var b strings.Builder
	m.Write(&b)
	return b.String()
}

func TestAuthVariants(t *testing.T) {
	t.Parallel()
	s, m, _ := stubServer(t, reply(200, `{"ErrorCode":0}`), func(c *config.Config) { c.AuthTokens = []string{"t1", "t2"} })
	cases := []struct {
		name string
		hdr  []string
		want int
	}{
		{"none", nil, 401},
		{"bearer", []string{"Authorization", "Bearer t1"}, 200},
		{"bearer padded", []string{"Authorization", "Bearer  t2 "}, 200},
		{"bearer empty", []string{"Authorization", "Bearer "}, 401},
		{"basic scheme", []string{"Authorization", "Basic t1"}, 401},
		{"lowercase scheme", []string{"Authorization", "bearer t1"}, 401},
		{"prefix of token", []string{"Authorization", "Bearer t"}, 401},
		{"token with suffix", []string{"Authorization", "Bearer t12"}, 401},
		{"postmark header", []string{"X-Postmark-Server-Token", "t2"}, 200},
		{"postmark header wins over bearer", []string{"X-Postmark-Server-Token", "bad", "Authorization", "Bearer t1"}, 401},
	}
	for _, c := range cases {
		w := do(s, "POST", "/email", validEmail, c.hdr...)
		if w.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, w.Code, c.want)
			continue
		}
		if c.want == 401 {
			var e apiError
			mustUnmarshal(t, w.Body.Bytes(), &e)
			if e.ErrorCode != codeUnauthorized || w.Header().Get("WWW-Authenticate") != `Bearer realm="postmarkd"` {
				t.Errorf("%s: body=%s www-authenticate=%q", c.name, w.Body, w.Header().Get("WWW-Authenticate"))
			}
		}
	}
	if !strings.Contains(metricsText(m), `postmarkd_http_requests_total{endpoint="auth",code="401"} 7`) {
		t.Errorf("auth failures not counted:\n%s", metricsText(m))
	}
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		if w := do(s, "GET", path, ""); w.Code == 401 {
			t.Errorf("%s requires auth", path)
		}
	}
}

func TestBlackholeRequiresAuth(t *testing.T) {
	t.Parallel()
	bh := blackhole.New(5)
	s, _, _ := stubServer(t, bh, func(c *config.Config) { c.AuthTokens = []string{"tok"} })
	if w := do(s, "POST", "/email", validEmail, "Authorization", "Bearer tok"); w.Code != 200 {
		t.Fatalf("send: %d %s", w.Code, w.Body)
	}
	for _, method := range []string{"GET", "DELETE"} {
		if w := do(s, method, "/blackhole/messages", ""); w.Code != 401 {
			t.Errorf("%s without token: %d", method, w.Code)
		}
	}
	if len(bh.Messages()) != 1 {
		t.Fatal("unauthenticated DELETE cleared the inbox")
	}
	w := do(s, "GET", "/blackhole/messages", "", "Authorization", "Bearer tok")
	var inbox struct{ Messages []blackhole.Message }
	mustUnmarshal(t, w.Body.Bytes(), &inbox)
	if w.Code != 200 || len(inbox.Messages) != 1 || inbox.Messages[0].Path != "/email" {
		t.Errorf("inbox: %d %s", w.Code, w.Body)
	}
}

func TestBlackholeRoutesOnlyWithBlackhole(t *testing.T) {
	t.Parallel()
	s, _, _ := stubServer(t, reply(200, `{}`), nil)
	if w := do(s, "GET", "/blackhole/messages", ""); w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestSMTPAuth(t *testing.T) {
	t.Parallel()
	s, _, _ := stubServer(t, reply(200, `{}`), nil)
	if s.SMTPAuth() != nil {
		t.Fatal("SMTPAuth should be nil when no tokens are configured")
	}
	s, _, _ = stubServer(t, reply(200, `{}`), func(c *config.Config) { c.AuthTokens = []string{"t1", "t2"} })
	auth := s.SMTPAuth()
	if auth == nil {
		t.Fatal("SMTPAuth is nil with tokens configured")
	}
	cases := []struct {
		user, pass string
		want       bool
	}{
		{"anyone", "t1", true},
		{"", "t2", true},
		{"t1", "", false},
		{"user", "t3", false},
		{"user", "T1", false},
	}
	for _, c := range cases {
		if got := auth(c.user, c.pass); got != c.want {
			t.Errorf("auth(%q, %q) = %v, want %v", c.user, c.pass, got, c.want)
		}
	}
}

func TestSendResponses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		sender   stubSender
		wantCode int
		wantCT   string
		wantBody string
		sent     uint64
		failed   uint64
	}{
		{
			name: "content type defaulted",
			sender: func(context.Context, string, []byte) (*postmark.Response, error) {
				return &postmark.Response{StatusCode: 200, Body: []byte(`{"ErrorCode":0}`)}, nil
			},
			wantCode: 200, wantCT: "application/json", wantBody: `{"ErrorCode":0}`, sent: 1,
		},
		{
			name: "content type passed through",
			sender: func(context.Context, string, []byte) (*postmark.Response, error) {
				return &postmark.Response{StatusCode: 500, ContentType: "text/plain", Body: []byte("boom")}, nil
			},
			wantCode: 500, wantCT: "text/plain", wantBody: "boom", failed: 1,
		},
		{
			name:     "single rejected with 200",
			sender:   reply(200, `{"ErrorCode":406,"Message":"inactive"}`),
			wantCode: 200, wantCT: "application/json", wantBody: `{"ErrorCode":406,"Message":"inactive"}`, failed: 1,
		},
		{
			name: "upstream error",
			sender: func(context.Context, string, []byte) (*postmark.Response, error) {
				return nil, errors.New("dial tcp: refused")
			},
			wantCode: 502, wantCT: "application/json", failed: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, m, _ := stubServer(t, c.sender, nil)
			w := do(s, "POST", "/email", validEmail)
			if w.Code != c.wantCode || w.Header().Get("Content-Type") != c.wantCT {
				t.Fatalf("status=%d ct=%q body=%s", w.Code, w.Header().Get("Content-Type"), w.Body)
			}
			if c.wantBody != "" && w.Body.String() != c.wantBody {
				t.Errorf("body = %q, want %q", w.Body, c.wantBody)
			}
			if c.wantCode == 502 {
				var e apiError
				mustUnmarshal(t, w.Body.Bytes(), &e)
				if e.ErrorCode != codeUpstream || e.Message != "could not reach Postmark: dial tcp: refused" {
					t.Errorf("error = %+v", e)
				}
			}
			if m.MessagesSent.Load() != c.sent || m.MessagesFailed.Load() != c.failed {
				t.Errorf("sent=%d failed=%d, want %d/%d", m.MessagesSent.Load(), m.MessagesFailed.Load(), c.sent, c.failed)
			}
			if w.Header().Get("X-Request-Id") == "" {
				t.Error("no X-Request-Id")
			}
		})
	}
}

func TestRetriesCounted(t *testing.T) {
	t.Parallel()
	s, m, _ := funcServer(t, func(context.Context, string, []byte) (*postmark.Response, error) {
		return &postmark.Response{StatusCode: 200, Body: []byte(`{}`), Attempts: 3}, nil
	}, nil)
	if w := do(s, "POST", "/email", validEmail); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if m.Retries.Load() != 2 {
		t.Errorf("retries = %d, want 2", m.Retries.Load())
	}
}

func TestRequestID(t *testing.T) {
	t.Parallel()
	s, _, _ := stubServer(t, reply(200, `{}`), nil)
	cases := []struct {
		in   string
		keep bool
	}{
		{"client-id", true},
		{strings.Repeat("b", 128), true},
		{strings.Repeat("a", 129), false},
		{"", false},
	}
	for _, c := range cases {
		w := do(s, "POST", "/email", validEmail, "X-Request-Id", c.in)
		got := w.Header().Get("X-Request-Id")
		if c.keep && got != c.in {
			t.Errorf("id %q replaced by %q", c.in, got)
		}
		if !c.keep && (got == c.in || len(got) != 24) {
			t.Errorf("id %q: got %q, want a fresh 24-char id", c.in, got)
		}
	}
	if a, b := newID(), newID(); a == b {
		t.Errorf("newID repeated %q", a)
	}
}

func TestStreamHeaderOverridesQuery(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var got []byte
	s, _, _ := funcServer(t, func(_ context.Context, _ string, body []byte) (*postmark.Response, error) {
		mu.Lock()
		got = body
		mu.Unlock()
		return &postmark.Response{StatusCode: 200, Body: []byte(`{}`)}, nil
	}, func(c *config.Config) { c.DefaultMessageStream = "outbound" })
	cases := []struct {
		path string
		hdr  []string
		want string
	}{
		{"/email", nil, "outbound"},
		{"/email?stream=q", nil, "q"},
		{"/email?stream=q", []string{"X-Postmarkd-Stream", "h"}, "h"},
	}
	for _, c := range cases {
		if w := do(s, "POST", c.path, validEmail, c.hdr...); w.Code != 200 {
			t.Fatalf("%s: %d", c.path, w.Code)
		}
		var sent map[string]any
		mu.Lock()
		mustUnmarshal(t, got, &sent)
		mu.Unlock()
		if sent["MessageStream"] != c.want {
			t.Errorf("%s %v: stream = %v, want %s", c.path, c.hdr, sent["MessageStream"], c.want)
		}
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestBodyReadError(t *testing.T) {
	t.Parallel()
	s, _, _ := stubServer(t, reply(200, `{}`), nil)
	r := httptest.NewRequest(http.MethodPost, "/email", errReader{})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var e apiError
	mustUnmarshal(t, w.Body.Bytes(), &e)
	if w.Code != 400 || e.ErrorCode != codeInvalidRequest || e.Message != "error reading request body" {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
}

func TestBodyLimitBoundary(t *testing.T) {
	t.Parallel()
	s, m, _ := stubServer(t, reply(200, `{}`), func(c *config.Config) { c.MaxBodyBytes = 64 })
	w := do(s, "POST", "/email", strings.Repeat(" ", 65))
	var e apiError
	mustUnmarshal(t, w.Body.Bytes(), &e)
	if w.Code != 413 || e.ErrorCode != codeInvalidRequest || e.Message != "request body exceeds 64 bytes" {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	exact := validEmail[:len(validEmail)-1] + strings.Repeat(" ", 64-len(validEmail)) + "}"
	if len(exact) != 64 {
		t.Fatalf("test body is %d bytes", len(exact))
	}
	if w := do(s, "POST", "/email", exact); w.Code != 200 {
		t.Errorf("body of exactly the limit: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(metricsText(m), `postmarkd_http_requests_total{endpoint="email",code="413"} 1`) {
		t.Errorf("413 not counted")
	}
}

func TestClientGone(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	s, m, _ := funcServer(t, func(ctx context.Context, _ string, _ []byte) (*postmark.Response, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}, nil)
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/email", strings.NewReader(validEmail))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Body.Len() != 0 {
		t.Errorf("wrote a body to a client that went away: %s", w.Body)
	}
	if !strings.Contains(metricsText(m), `postmarkd_http_requests_total{endpoint="email",code="499"} 1`) {
		t.Errorf("499 not recorded:\n%s", metricsText(m))
	}
}

func TestRecover(t *testing.T) {
	t.Parallel()
	s, _, logs := funcServer(t, func(context.Context, string, []byte) (*postmark.Response, error) {
		panic("boom")
	}, nil)
	w := do(s, "POST", "/email", validEmail)
	if w.Code != 500 || !strings.Contains(w.Body.String(), "internal error") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if !strings.Contains(logs.String(), "panic serving request") || !strings.Contains(logs.String(), "boom") {
		t.Errorf("panic not logged: %s", logs)
	}
}

func TestRecoverRepanicsAbort(t *testing.T) {
	t.Parallel()
	s, _, _ := funcServer(t, func(context.Context, string, []byte) (*postmark.Response, error) {
		panic(http.ErrAbortHandler)
	}, nil)
	defer func() {
		if v, _ := recover().(error); !errors.Is(v, http.ErrAbortHandler) {
			t.Errorf("recovered %v, want http.ErrAbortHandler", v)
		}
	}()
	do(s, "POST", "/email", validEmail)
	t.Error("ErrAbortHandler was swallowed")
}

func TestAsyncQueueFull(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	s, m, _ := funcServer(t, func(context.Context, string, []byte) (*postmark.Response, error) {
		started <- struct{}{}
		<-release
		return &postmark.Response{StatusCode: 200, Body: []byte(`{}`)}, nil
	}, func(c *config.Config) { c.QueueSize = 1; c.Workers = 1 })

	if w := do(s, "POST", "/email", validEmail, "X-Postmarkd-Async", "true"); w.Code != 202 {
		t.Fatalf("first: %d", w.Code)
	}
	<-started // the worker is busy with the first job
	if w := do(s, "POST", "/email", validEmail, "X-Postmarkd-Async", "true"); w.Code != 202 {
		t.Fatalf("second: %d", w.Code)
	}
	w := do(s, "POST", "/email", validEmail, "X-Postmarkd-Async", "true")
	var e apiError
	mustUnmarshal(t, w.Body.Bytes(), &e)
	if w.Code != 503 || e.ErrorCode != codeQueueFull || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("third: %d %s retry-after=%q", w.Code, w.Body, w.Header().Get("Retry-After"))
	}
	if !strings.Contains(metricsText(m), "postmarkd_async_queue_depth 1") {
		t.Errorf("queue depth gauge:\n%s", metricsText(m))
	}
	close(release)
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.AsyncEnqueued.Load() != 2 || m.AsyncRejected.Load() != 1 || m.MessagesSent.Load() != 2 {
		t.Errorf("enqueued=%d rejected=%d sent=%d", m.AsyncEnqueued.Load(), m.AsyncRejected.Load(), m.MessagesSent.Load())
	}
}

func TestAsyncFlagParsing(t *testing.T) {
	t.Parallel()
	s, _, _ := stubServer(t, reply(200, `{"ErrorCode":0}`), nil)
	cases := []struct {
		path string
		hdr  []string
		want int
	}{
		{"/email?async=1", nil, 202},
		{"/email?async=TRUE", nil, 202},
		{"/email?async=0", nil, 200},
		{"/email?async=yes", nil, 200},
		{"/email?async=true", []string{"X-Postmarkd-Async", "false"}, 200},
		{"/email", []string{"X-Postmarkd-Async", "t"}, 202},
	}
	for _, c := range cases {
		if w := do(s, "POST", c.path, validEmail, c.hdr...); w.Code != c.want {
			t.Errorf("%s %v: status = %d, want %d", c.path, c.hdr, w.Code, c.want)
		}
	}
}

func TestAsyncInvalidNotQueued(t *testing.T) {
	t.Parallel()
	s, m, _ := stubServer(t, reply(200, `{}`), nil)
	if w := do(s, "POST", "/email?async=1", `{"To":"a@example.com"}`); w.Code != 422 {
		t.Fatalf("status = %d", w.Code)
	}
	if m.AsyncEnqueued.Load() != 0 {
		t.Error("invalid message queued")
	}
}

// TestShutdownDropsPending checks that jobs still queued when the shutdown
// deadline passes are counted as dropped rather than silently lost.
func TestShutdownDropsPending(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 2)
	s, m, logs := funcServer(t, func(ctx context.Context, _ string, _ []byte) (*postmark.Response, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}, func(c *config.Config) { c.QueueSize = 5; c.Workers = 1 })

	if w := do(s, "POST", "/email/batch?async=1", `[`+validEmail+`,`+validEmail+`]`); w.Code != 202 || !strings.Contains(w.Body.String(), `"Messages":2`) {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	<-started
	if w := do(s, "POST", "/email?async=1", validEmail); w.Code != 202 {
		t.Fatalf("second: %d", w.Code)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown = %v, want context.Canceled", err)
	}
	// The in-flight batch fails (2) and the queued email is dropped (1).
	if m.AsyncDropped.Load() != 1 || m.MessagesFailed.Load() != 3 {
		t.Errorf("dropped=%d failed=%d, want 1/3", m.AsyncDropped.Load(), m.MessagesFailed.Load())
	}
	for _, want := range []string{
		"draining async queue",
		`msg="dropping queued request at shutdown" request_id=`,
		"endpoint=email async=true messages=1",
		`msg="postmark request failed" request_id=`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log missing %q:\n%s", want, logs)
		}
	}
	if w := do(s, "POST", "/email?async=1", validEmail); w.Code != 503 {
		t.Errorf("enqueue after shutdown: %d", w.Code)
	}
	if w := do(s, "GET", "/readyz", ""); w.Code != 503 {
		t.Errorf("ready after shutdown: %d", w.Code)
	}
}

func TestShutdownWithoutQueue(t *testing.T) {
	t.Parallel()
	s, _, _ := stubServer(t, reply(200, `{}`), func(c *config.Config) { c.QueueSize = 0 })
	s.SetReady(true)
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if w := do(s, "GET", "/readyz", ""); w.Code != 503 {
		t.Errorf("readyz after shutdown = %d", w.Code)
	}
	if w := do(s, "GET", "/metrics", ""); strings.Contains(w.Body.String(), "postmarkd_async_queue") {
		t.Errorf("queue gauges registered without a queue")
	}
}

func TestDeliverUnknownEndpoint(t *testing.T) {
	t.Parallel()
	var called bool
	s, m, _ := funcServer(t, func(context.Context, string, []byte) (*postmark.Response, error) {
		called = true
		return nil, errors.New("unexpected")
	}, nil)
	s.deliver(t.Context(), queue.Job{ID: "x", Path: "/nope", Endpoint: "nope", Body: []byte(validEmail), Messages: 1, Enqueued: time.Now()})
	if called || m.MessagesFailed.Load() != 0 {
		t.Errorf("unknown endpoint was forwarded")
	}
}

func TestDebugLogging(t *testing.T) {
	t.Parallel()
	s, _, logs := stubServer(t, reply(200, `{"ErrorCode":0}`), nil)
	do(s, "POST", "/email", `{}`, "X-Request-Id", "rid-1")
	do(s, "POST", "/email", validEmail, "X-Request-Id", "rid-2")
	do(s, "POST", "/email?async=1", validEmail, "X-Request-Id", "rid-3")
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	for _, want := range []string{
		`msg="request rejected" request_id=rid-1 endpoint=email status=422`,
		`msg=sent request_id=rid-2 endpoint=email messages=1`,
		`msg=queued request_id=rid-3 endpoint=email messages=1`,
		`msg=sent request_id=rid-3 endpoint=email async=true messages=1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
}

func TestQuietLogging(t *testing.T) {
	t.Parallel()
	logs := &logBuf{}
	s := New(config.Default(), reply(200, `{"ErrorCode":0}`), metrics.New(), slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelError})))
	do(s, "POST", "/email", `{}`)
	do(s, "POST", "/email", validEmail)
	do(s, "POST", "/email/batch", `[`+validEmail+`]`) // reply is not an array: assumed sent
	rejecting := New(config.Default(), reply(422, `{"ErrorCode":300,"Message":"bad"}`), metrics.New(), slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelError})))
	if w := do(rejecting, "POST", "/email", validEmail); w.Code != 422 {
		t.Fatalf("status = %d", w.Code)
	}
	if logs.String() != "" {
		t.Errorf("logged below the configured level: %s", logs)
	}
}

func TestHandleSMTPOutcomes(t *testing.T) {
	t.Parallel()
	const raw = "From: f@example.com\r\nTo: a@example.com\r\nSubject: Hi\r\n\r\nhello\r\n"
	cases := []struct {
		name   string
		sender stubSender
		data   string
		wantID string
		code   int
		msg    string
		metric string
		sent   uint64
	}{
		{"ok", reply(200, `{"ErrorCode":0,"MessageID":"m-1"}`), raw, "m-1", 0, "", `code="200"`, 1},
		{"unparseable", reply(200, `{}`), "no header line\r\n", "", 554, "5.6.0 ", `code="422"`, 0},
		{"no body", reply(200, `{}`), "From: f@example.com\r\nTo: a@example.com\r\n\r\n", "", 554, "5.6.0 'HtmlBody' or 'TextBody' is required", `code="422"`, 0},
		{"unreachable", func(context.Context, string, []byte) (*postmark.Response, error) { return nil, errors.New("refused") }, raw, "", 451, "4.4.1 Postmark unreachable, try again later", `code="502"`, 0},
		{"rate limited", reply(429, `{"ErrorCode":429,"Message":"slow down"}`), raw, "", 451, "4.3.0 Postmark error 429: slow down", `code="429"`, 0},
		{"server error not json", reply(503, `<html>`), raw, "", 451, "4.3.0 Postmark error 0: ", `code="503"`, 0},
		{"rejected with 200", reply(200, `{"ErrorCode":406,"Message":"inactive"}`), raw, "", 554, "5.7.1 Postmark error 406: inactive", `code="200"`, 0},
		{"rejected 422", reply(422, `{"ErrorCode":300,"Message":"bad"}`), raw, "", 554, "5.7.1 Postmark error 300: bad", `code="422"`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, m, _ := stubServer(t, c.sender, nil)
			id, err := s.HandleSMTP(t.Context(), smtpd.Envelope{To: []string{"a@example.com"}}, []byte(c.data))
			if c.code == 0 {
				if err != nil || id != c.wantID {
					t.Fatalf("id=%q err=%v", id, err)
				}
			} else {
				var se *smtpd.Error
				if !errors.As(err, &se) || se.Code != c.code || !strings.HasPrefix(se.Message, c.msg) {
					t.Fatalf("err = %v, want %d %q", err, c.code, c.msg)
				}
				if id != "" {
					t.Errorf("id = %q on failure", id)
				}
			}
			if want := `postmarkd_http_requests_total{endpoint="smtp",` + c.metric + `} 1`; !strings.Contains(metricsText(m), want) {
				t.Errorf("metrics missing %s:\n%s", want, metricsText(m))
			}
			if m.MessagesSent.Load() != c.sent {
				t.Errorf("sent = %d, want %d", m.MessagesSent.Load(), c.sent)
			}
		})
	}
}

func TestHandleSMTPEnvelopeFrom(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var got []byte
	s, _, _ := funcServer(t, func(_ context.Context, path string, body []byte) (*postmark.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if path != "/email" {
			t.Errorf("path = %q", path)
		}
		got = body
		return &postmark.Response{StatusCode: 200, Body: []byte(`{"MessageID":"x"}`)}, nil
	}, nil)
	_, err := s.HandleSMTP(t.Context(), smtpd.Envelope{From: "env@example.com", To: []string{"a@example.com"}}, []byte("To: a@example.com\r\n\r\nhi\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	mu.Lock()
	mustUnmarshal(t, got, &sent)
	mu.Unlock()
	if sent["From"] != "env@example.com" {
		t.Errorf("From = %v", sent["From"])
	}
	_, err = s.HandleSMTP(t.Context(), smtpd.Envelope{To: []string{"a@example.com"}}, []byte("To: a@example.com\r\n\r\nhi\r\n"))
	var se *smtpd.Error
	if !errors.As(err, &se) || se.Code != 554 || !strings.Contains(se.Message, "'From' is required") {
		t.Errorf("no From anywhere: err = %v", err)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	t.Parallel()
	s, _, _ := stubServer(t, reply(200, `{"ErrorCode":0}`), nil)
	do(s, "POST", "/email", validEmail)
	w := do(s, "GET", "/metrics", "")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("status=%d ct=%q", w.Code, w.Header().Get("Content-Type"))
	}
	for _, want := range []string{
		`postmarkd_http_requests_total{endpoint="email",code="200"} 1`,
		"postmarkd_async_queue_capacity 10",
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if w := do(s, "GET", "/healthz", ""); w.Code != 200 || w.Body.String() != "ok\n" {
		t.Errorf("healthz = %d %q", w.Code, w.Body)
	}
}
