package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossedman/postmarkd/internal/blackhole"
	"github.com/rossedman/postmarkd/internal/config"
	"github.com/rossedman/postmarkd/internal/metrics"
	"github.com/rossedman/postmarkd/internal/postmark"
	"github.com/rossedman/postmarkd/internal/smtpd"
)

// fakePostmark records requests and answers like the Postmark API.
type fakePostmark struct {
	mu       sync.Mutex
	requests []fakeRequest
	status   int
	calls    atomic.Int64
}

type fakeRequest struct {
	path, token string
	body        []byte
}

func (f *fakePostmark) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{r.URL.Path, r.Header.Get("X-Postmark-Server-Token"), b})
	status := f.status
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if status != 0 && status != 200 {
		w.WriteHeader(status)
		io.WriteString(w, `{"ErrorCode":406,"Message":"You tried to send to a recipient that has been marked as inactive."}`)
		return
	}
	if strings.Contains(r.URL.Path, "batch") {
		io.WriteString(w, `[{"ErrorCode":0,"Message":"OK","To":"a@example.com"},{"ErrorCode":300,"Message":"Invalid 'To' address"}]`)
		return
	}
	io.WriteString(w, `{"ErrorCode":0,"Message":"OK","MessageID":"b7bc2f4a","To":"a@example.com"}`)
}

func (f *fakePostmark) last(t *testing.T) fakeRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no upstream request")
	}
	return f.requests[len(f.requests)-1]
}

func setup(t *testing.T, mutate func(*config.Config)) (*Server, *fakePostmark, *metrics.Metrics) {
	t.Helper()
	fake := &fakePostmark{}
	up := httptest.NewServer(fake)
	t.Cleanup(up.Close)

	cfg := config.Default()
	cfg.ServerToken = "server-token"
	cfg.APIURL = up.URL
	cfg.QueueSize = 10
	cfg.Workers = 2
	if mutate != nil {
		mutate(&cfg)
	}
	m := metrics.New()
	client := postmark.New(postmark.Options{BaseURL: cfg.APIURL, ServerToken: cfg.ServerToken, MaxConcurrency: 4, MaxRetries: 0})
	s := New(cfg, client, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { s.Shutdown(context.Background()) })
	return s, fake, m
}

func do(s http.Handler, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestSendEmail(t *testing.T) {
	s, fake, _ := setup(t, func(c *config.Config) {
		c.DefaultFrom = "noreply@example.com"
		c.DefaultMessageStream = "outbound"
	})
	w := do(s, "POST", "/email", `{"To":"a@example.com","Subject":"hi","TextBody":"hello","CustomFutureField":{"x":1}}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "b7bc2f4a") {
		t.Errorf("upstream body not passed through: %s", w.Body)
	}
	req := fake.last(t)
	if req.path != "/email" || req.token != "server-token" {
		t.Errorf("upstream path=%q token=%q", req.path, req.token)
	}
	var sent map[string]any
	json.Unmarshal(req.body, &sent)
	if sent["From"] != "noreply@example.com" || sent["MessageStream"] != "outbound" {
		t.Errorf("defaults not applied: %v", sent)
	}
	if sent["CustomFutureField"] == nil {
		t.Errorf("unknown field dropped: %v", sent)
	}
}

func TestExplicitFromWins(t *testing.T) {
	s, fake, _ := setup(t, func(c *config.Config) { c.DefaultFrom = "default@example.com" })
	w := do(s, "POST", "/email", `{"from":"me@example.com","to":"a@example.com","TextBody":"x"}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	var sent map[string]any
	json.Unmarshal(fake.last(t).body, &sent)
	if sent["from"] != "me@example.com" || sent["From"] != nil {
		t.Errorf("explicit lowercase from was overridden: %v", sent)
	}
}

func TestValidation(t *testing.T) {
	s, fake, _ := setup(t, nil)
	cases := []struct {
		path, body string
		code       int
	}{
		{"/email", `not json`, codeInvalidJSON},
		{"/email", `[]`, codeInvalidJSON},
		{"/email", `{"To":"a@example.com","TextBody":"x"}`, codeInvalidRequest},                      // no From
		{"/email", `{"From":"f@example.com","TextBody":"x"}`, codeInvalidRequest},                    // no recipient
		{"/email", `{"From":"f@example.com","To":"a@example.com"}`, codeInvalidRequest},              // no body
		{"/email/withTemplate", `{"From":"f@example.com","To":"a@example.com"}`, codeInvalidRequest}, // no template
		{"/email/batch", `[]`, codeInvalidRequest},
		{"/email/batchWithTemplates", `{"Messages":[{"From":"f@example.com","To":"a@example.com"}]}`, codeInvalidRequest},
	}
	for _, c := range cases {
		w := do(s, "POST", c.path, c.body)
		var e apiError
		json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != 422 || e.ErrorCode != c.code {
			t.Errorf("%s %s: status=%d code=%d, want 422/%d (%s)", c.path, c.body, w.Code, e.ErrorCode, c.code, e.Message)
		}
	}
	if n := fake.calls.Load(); n != 0 {
		t.Errorf("invalid requests reached upstream %d times", n)
	}
}

func TestBatchLimit(t *testing.T) {
	s, _, _ := setup(t, nil)
	msgs := make([]map[string]string, maxBatch+1)
	for i := range msgs {
		msgs[i] = map[string]string{"From": "f@example.com", "To": "a@example.com", "TextBody": "x"}
	}
	b, _ := json.Marshal(msgs)
	if w := do(s, "POST", "/email/batch", string(b)); w.Code != 422 {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestBatchMetrics(t *testing.T) {
	s, _, m := setup(t, nil)
	w := do(s, "POST", "/email/batch", `[{"From":"f@example.com","To":"a@example.com","TextBody":"x"},{"From":"f@example.com","To":"bad","TextBody":"x"}]`)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if m.MessagesSent.Load() != 1 || m.MessagesFailed.Load() != 1 {
		t.Errorf("sent=%d failed=%d, want 1/1", m.MessagesSent.Load(), m.MessagesFailed.Load())
	}
	var out strings.Builder
	m.Write(&out)
	for _, want := range []string{
		`postmarkd_http_requests_total{endpoint="batch",code="200"} 1`,
		`postmarkd_messages_sent_total 1`,
		`postmarkd_upstream_request_duration_seconds_count{endpoint="batch"} 1`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestUpstreamErrorPassedThrough(t *testing.T) {
	s, fake, _ := setup(t, nil)
	fake.status = 422
	w := do(s, "POST", "/email", `{"From":"f@example.com","To":"a@example.com","TextBody":"x"}`)
	if w.Code != 422 || !strings.Contains(w.Body.String(), `"ErrorCode":406`) {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
}

func TestUpstreamUnreachable(t *testing.T) {
	s, _, _ := setup(t, func(c *config.Config) { c.APIURL = "http://127.0.0.1:1" })
	w := do(s, "POST", "/email", `{"From":"f@example.com","To":"a@example.com","TextBody":"x"}`)
	if w.Code != 502 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
}

func TestAuth(t *testing.T) {
	s, _, _ := setup(t, func(c *config.Config) { c.AuthTokens = []string{"t1", "t2"} })
	body := `{"From":"f@example.com","To":"a@example.com","TextBody":"x"}`
	if w := do(s, "POST", "/email", body); w.Code != 401 {
		t.Errorf("no token: status = %d", w.Code)
	}
	if w := do(s, "POST", "/email", body, "Authorization", "Bearer nope"); w.Code != 401 {
		t.Errorf("bad token: status = %d", w.Code)
	}
	if w := do(s, "POST", "/email", body, "Authorization", "Bearer t2"); w.Code != 200 {
		t.Errorf("bearer token: status = %d", w.Code)
	}
	if w := do(s, "POST", "/email", body, "X-Postmark-Server-Token", "t1"); w.Code != 200 {
		t.Errorf("postmark header token: status = %d", w.Code)
	}
	if w := do(s, "GET", "/healthz", ""); w.Code != 200 {
		t.Errorf("healthz should not require auth: %d", w.Code)
	}
}

func TestAsync(t *testing.T) {
	s, fake, m := setup(t, nil)
	w := do(s, "POST", "/email?async=true", `{"From":"f@example.com","To":"a@example.com","TextBody":"x"}`, "X-Request-Id", "abc")
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"RequestID":"abc"`) {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.calls.Load() != 1 || m.MessagesSent.Load() != 1 {
		t.Errorf("calls=%d sent=%d", fake.calls.Load(), m.MessagesSent.Load())
	}
}

func TestAsyncDisabled(t *testing.T) {
	s, _, _ := setup(t, func(c *config.Config) { c.QueueSize = 0 })
	w := do(s, "POST", "/email", `{"From":"f@example.com","To":"a@example.com","TextBody":"x"}`, "X-Postmarkd-Async", "1")
	if w.Code != 400 {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestBodyLimit(t *testing.T) {
	s, _, _ := setup(t, func(c *config.Config) { c.MaxBodyBytes = 1024 })
	w := do(s, "POST", "/email", `{"TextBody":"`+strings.Repeat("x", 2048)+`"}`)
	if w.Code != 413 {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestReadyz(t *testing.T) {
	s, _, _ := setup(t, nil)
	if w := do(s, "GET", "/readyz", ""); w.Code != 503 {
		t.Errorf("before ready: %d", w.Code)
	}
	s.SetReady(true)
	if w := do(s, "GET", "/readyz", ""); w.Code != 200 {
		t.Errorf("after ready: %d", w.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	s, _, _ := setup(t, nil)
	if w := do(s, "GET", "/email", ""); w.Code != 405 {
		t.Errorf("status = %d", w.Code)
	}
}

func TestStreamOverride(t *testing.T) {
	s, fake, _ := setup(t, func(c *config.Config) { c.DefaultMessageStream = "outbound" })
	do(s, "POST", "/email/batch?stream=broadcast", `[{"From":"f@example.com","To":"a@example.com","TextBody":"x"},{"From":"f@example.com","To":"b@example.com","TextBody":"x","MessageStream":"alerts"}]`)
	var sent []map[string]any
	json.Unmarshal(fake.last(t).body, &sent)
	if sent[0]["MessageStream"] != "broadcast" || sent[1]["MessageStream"] != "alerts" {
		t.Errorf("streams: %v", sent)
	}
}

func TestBlackholeInbox(t *testing.T) {
	cfg := config.Default()
	cfg.Blackhole = true
	bh := blackhole.New(10)
	s := New(cfg, bh, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	w := do(s, "POST", "/email", `{"From":"f@example.com","To":"a@example.com","TextBody":"x"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"MessageID"`) {
		t.Fatalf("send: %d %s", w.Code, w.Body)
	}
	w = do(s, "GET", "/blackhole/messages", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `a@example.com`) {
		t.Fatalf("inbox: %d %s", w.Code, w.Body)
	}
	if w = do(s, "DELETE", "/blackhole/messages", ""); w.Code != 204 || len(bh.Messages()) != 0 {
		t.Fatalf("reset: %d", w.Code)
	}
}

func TestHandleSMTP(t *testing.T) {
	s, fake, m := setup(t, func(c *config.Config) { c.DefaultFrom = "noreply@example.com" })
	raw := "To: a@example.com\r\nSubject: Hi\r\nX-PM-Message-Stream: alerts\r\n\r\nhello\r\n"
	id, err := s.HandleSMTP(context.Background(), smtpd.Envelope{To: []string{"a@example.com"}}, []byte(raw))
	if err != nil || id != "b7bc2f4a" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	var sent map[string]any
	json.Unmarshal(fake.last(t).body, &sent)
	if sent["From"] != "noreply@example.com" || sent["MessageStream"] != "alerts" || sent["TextBody"] != "hello\r\n" {
		t.Errorf("sent %v", sent)
	}
	if m.MessagesSent.Load() != 1 {
		t.Error("not counted")
	}

	fake.status = 422
	_, err = s.HandleSMTP(context.Background(), smtpd.Envelope{To: []string{"a@example.com"}}, []byte(raw))
	var se *smtpd.Error
	if !errors.As(err, &se) || se.Code != 554 || !strings.Contains(se.Message, "inactive") {
		t.Errorf("err = %v", err)
	}

	_, err = s.HandleSMTP(context.Background(), smtpd.Envelope{To: []string{"a@example.com"}}, []byte("To: a@example.com\r\nSubject: x\r\n\r\n"))
	if !errors.As(err, &se) || se.Code != 554 {
		t.Errorf("empty body: err = %v", err)
	}
}
