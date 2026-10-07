package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossedman/postmarkd/internal/blackhole"
	"github.com/rossedman/postmarkd/internal/config"
	"github.com/rossedman/postmarkd/internal/metrics"
	"github.com/rossedman/postmarkd/internal/postmark"
	"github.com/rossedman/postmarkd/internal/smtpd"
)

// A typical transactional message: text + HTML, a tag and metadata.
const benchMessage = `{"From":"Acme <noreply@example.com>","To":"ada@example.com","Subject":"Your receipt #1234",` +
	`"HtmlBody":"<html><body><h1>Thanks for your order</h1><p>Order 1234 has shipped and will arrive on Tuesday.</p></body></html>",` +
	`"TextBody":"Thanks for your order. Order 1234 has shipped and will arrive on Tuesday.",` +
	`"Tag":"receipt","Metadata":{"order":"1234","customer":"42"},"MessageStream":"outbound","TrackOpens":true}`

func benchBatch(n int) []byte {
	var b bytes.Buffer
	b.WriteByte('[')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strings.Replace(benchMessage, "ada@example.com", fmt.Sprintf("user%d@example.com", i), 1))
	}
	b.WriteByte(']')
	return b.Bytes()
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func blackholeServer(b *testing.B) *Server {
	cfg := config.Default()
	cfg.Blackhole = true
	cfg.BlackholeKeep = 0
	s := New(cfg, blackhole.New(0), metrics.New(), discard())
	b.Cleanup(func() { s.Shutdown(context.Background()) })
	return s
}

// BenchmarkPrepare measures parsing, validating and defaulting a request
// body: the CPU work postmarkd adds to every send.
func BenchmarkPrepare(b *testing.B) {
	for _, tc := range []struct {
		name string
		ep   endpoint
		body []byte
		d    defaults
	}{
		{"Email", endpoints[0], []byte(benchMessage), defaults{}},
		{"EmailWithDefaults", endpoints[0], []byte(strings.Replace(benchMessage, `"MessageStream":"outbound",`, "", 1)), defaults{From: "x@example.com", MessageStream: "outbound"}},
		{"Batch500", endpoints[1], benchBatch(500), defaults{}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.body)))
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := prepare(tc.ep, tc.body, tc.d); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkHandler measures a full in-process HTTP request through the API
// handler (auth, prepare, metrics, response) with an instant upstream.
func BenchmarkHandler(b *testing.B) {
	s := blackholeServer(b)
	for _, tc := range []struct {
		name, path string
		body       []byte
	}{
		{"Email", "/email", []byte(benchMessage)},
		{"Batch500", "/email/batch", benchBatch(500)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.body)))
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					r := httptest.NewRequest("POST", tc.path, bytes.NewReader(tc.body))
					w := httptest.NewRecorder()
					s.ServeHTTP(w, r)
					if w.Code != 200 {
						b.Fatalf("status %d: %s", w.Code, w.Body)
					}
				}
			})
		})
	}
}

// BenchmarkUnixSocket is the end-to-end daemon overhead: concurrent clients
// sending over a real unix socket to the HTTP server, with an instant
// upstream. ns/op is per message; messages/s is the daemon's ceiling.
func BenchmarkUnixSocket(b *testing.B) {
	s := blackholeServer(b)
	sock := filepath.Join(b.TempDir(), "postmarkd.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		b.Fatal(err)
	}
	srv := &http.Server{Handler: s}
	go srv.Serve(l)
	b.Cleanup(func() { srv.Close() })

	client := &http.Client{Transport: &http.Transport{
		MaxIdleConnsPerHost: 512,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	body := []byte(benchMessage)
	b.SetParallelism(16) // many concurrent callers per CPU, like a busy app
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			res, err := client.Post("http://postmarkd/email", "application/json", bytes.NewReader(body))
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				b.Fatalf("status %d", res.StatusCode)
			}
		}
	})
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "messages/s")
}

// BenchmarkProxy is the whole relay path: client → postmarkd (unix socket)
// → pooled HTTP client → a local fake Postmark. It shows what postmarkd adds
// on top of the Postmark round trip when upstream latency is zero.
func BenchmarkProxy(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"To":"ada@example.com","SubmittedAt":"2026-10-07T12:00:00Z","MessageID":"b7bc2f4a-e38e-4336-af7d-e6c392c2f817","ErrorCode":0,"Message":"OK"}`)
	}))
	b.Cleanup(upstream.Close)

	cfg := config.Default()
	cfg.ServerToken = "t"
	pm := postmark.New(postmark.Options{BaseURL: upstream.URL, ServerToken: "t", MaxConcurrency: 128})
	s := New(cfg, pm, metrics.New(), discard())
	b.Cleanup(func() { s.Shutdown(context.Background()) })

	sock := filepath.Join(b.TempDir(), "postmarkd.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		b.Fatal(err)
	}
	srv := &http.Server{Handler: s}
	go srv.Serve(l)
	b.Cleanup(func() { srv.Close() })
	client := &http.Client{Transport: &http.Transport{
		MaxIdleConnsPerHost: 512,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}

	body := []byte(benchMessage)
	b.SetParallelism(16)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			res, err := client.Post("http://postmarkd/email", "application/json", bytes.NewReader(body))
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				b.Fatalf("status %d", res.StatusCode)
			}
		}
	})
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "messages/s")
}

// BenchmarkHandleSMTP measures turning a raw MIME message into a sent
// Postmark message (conversion, validation, delivery) with an instant upstream.
func BenchmarkHandleSMTP(b *testing.B) {
	s := blackholeServer(b)
	raw := []byte("From: Acme <noreply@example.com>\r\nTo: ada@example.com\r\nSubject: Your receipt\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=\"b\"\r\n\r\n" +
		"--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nThanks for your order.\r\n" +
		"--b\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Thanks for your order.</p>\r\n--b--\r\n")
	env := smtpd.Envelope{From: "noreply@example.com", To: []string{"ada@example.com"}}
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := s.HandleSMTP(context.Background(), env, raw); err != nil {
				b.Fatal(err)
			}
		}
	})
}
