package postmark

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(url string, retries int) *Client {
	c := New(Options{BaseURL: url, ServerToken: "tok", MaxConcurrency: 2, MaxRetries: retries, Timeout: time.Second})
	c.backoff = func(int) time.Duration { return time.Millisecond }
	return c
}

func TestRetriesRetryableStatus(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"ErrorCode":0}`))
	}))
	defer srv.Close()

	res, err := newTestClient(srv.URL, 3).Post(context.Background(), "/email", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || res.Attempts != 3 {
		t.Fatalf("status=%d attempts=%d", res.StatusCode, res.Attempts)
	}
}

func TestDoesNotRetryAmbiguousFailures(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusGatewayTimeout, 422} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(status)
		}))
		res, err := newTestClient(srv.URL, 3).Post(context.Background(), "/email", []byte(`{}`))
		srv.Close()
		if err != nil || res.StatusCode != status || calls.Load() != 1 {
			t.Errorf("status %d: err=%v calls=%d", status, err, calls.Load())
		}
	}
}

func TestRetriesExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	res, err := newTestClient(srv.URL, 2).Post(context.Background(), "/email", []byte(`{}`))
	if err != nil || res.StatusCode != 429 || res.Attempts != 3 {
		t.Fatalf("err=%v res=%+v", err, res)
	}
}

func TestDialErrorRetried(t *testing.T) {
	c := newTestClient("http://127.0.0.1:1", 2)
	_, err := c.Post(context.Background(), "/email", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); !strings.Contains(got, "3 attempt") {
		t.Errorf("error %q should report 3 attempts", got)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	var cur, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		cur.Add(-1)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL, 0)
	done := make(chan struct{})
	for range 10 {
		go func() { c.Post(context.Background(), "/email", nil); done <- struct{}{} }()
	}
	for range 10 {
		<-done
	}
	if peak.Load() > 2 {
		t.Fatalf("peak concurrency %d exceeds limit 2", peak.Load())
	}
}
