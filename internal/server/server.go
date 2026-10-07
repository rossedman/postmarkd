// Package server implements postmarkd's HTTP API: a Postmark-compatible mail
// sending API plus health and metrics endpoints.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rossedman/postmarkd/internal/config"
	"github.com/rossedman/postmarkd/internal/metrics"
	"github.com/rossedman/postmarkd/internal/postmark"
	"github.com/rossedman/postmarkd/internal/queue"
)

// Sender posts a JSON body to a Postmark API path.
type Sender interface {
	Post(ctx context.Context, path string, body []byte) (*postmark.Response, error)
}

// Server is the postmarkd HTTP API.
type Server struct {
	cfg      config.Config
	defaults defaults
	sender   Sender
	log      *slog.Logger
	metrics  *metrics.Metrics
	queue    *queue.Queue // nil when async sending is disabled
	tokens   [][]byte
	ready    atomic.Bool
	handler  http.Handler
}

// New builds a Server. Call Shutdown to drain the async queue.
func New(cfg config.Config, sender Sender, m *metrics.Metrics, log *slog.Logger) *Server {
	s := &Server{
		cfg:      cfg,
		defaults: defaults{From: cfg.DefaultFrom, MessageStream: cfg.DefaultMessageStream},
		sender:   sender,
		log:      log,
		metrics:  m,
	}
	for _, t := range cfg.AuthTokens {
		s.tokens = append(s.tokens, []byte(t))
	}
	if cfg.QueueSize > 0 {
		s.queue = queue.New(cfg.QueueSize, cfg.Workers, s.deliver)
		m.Gauge("postmarkd_async_queue_depth", "Requests waiting in the async queue.", func() float64 { return float64(s.queue.Len()) })
		m.Gauge("postmarkd_async_queue_capacity", "Capacity of the async queue.", func() float64 { return float64(s.queue.Cap()) })
	}

	mux := http.NewServeMux()
	for _, ep := range endpoints {
		mux.Handle("POST "+ep.path, s.authenticate(s.send(ep)))
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ready\n")
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.Write(w)
	})
	s.handler = s.recover(mux)
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// SetReady flips the /readyz state.
func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }

// Shutdown waits for queued mail to be delivered, or for ctx to expire.
func (s *Server) Shutdown(ctx context.Context) error {
	s.SetReady(false)
	if s.queue == nil {
		return nil
	}
	if n := s.queue.Len(); n > 0 {
		s.log.Info("draining async queue", "pending", n)
	}
	return s.queue.Shutdown(ctx)
}

func (s *Server) send(ep endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid := requestID(r)
		w.Header().Set("X-Request-Id", rid)
		log := s.log.With("request_id", rid, "endpoint", ep.name)
		status := http.StatusOK
		defer func() { s.metrics.Request(ep.name, status) }()

		fail := func(e *apiError) {
			status = e.Status
			if e.Status >= 500 {
				log.Warn("request failed", "status", e.Status, "error", e.Message)
			} else {
				log.Debug("request rejected", "status", e.Status, "error", e.Message)
			}
			writeJSON(w, e.Status, e)
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				fail(&apiError{Status: http.StatusRequestEntityTooLarge, ErrorCode: codeInvalidRequest,
					Message: "request body exceeds " + strconv.FormatInt(s.cfg.MaxBodyBytes, 10) + " bytes"})
				return
			}
			fail(&apiError{Status: http.StatusBadRequest, ErrorCode: codeInvalidRequest, Message: "error reading request body"})
			return
		}

		out, n, aerr := prepare(ep, body, s.defaults)
		if aerr != nil {
			fail(aerr)
			return
		}

		if isAsync(r) {
			if s.queue == nil {
				fail(&apiError{Status: http.StatusBadRequest, ErrorCode: codeAsyncDisabled, Message: "async sending is disabled (POSTMARKD_QUEUE_SIZE=0)"})
				return
			}
			job := queue.Job{ID: rid, Path: ep.path, Endpoint: ep.name, Body: out, Messages: n, Enqueued: time.Now()}
			if !s.queue.Enqueue(job) {
				s.metrics.AsyncRejected.Add(1)
				w.Header().Set("Retry-After", "1")
				fail(&apiError{Status: http.StatusServiceUnavailable, ErrorCode: codeQueueFull, Message: "async queue is full; retry later"})
				return
			}
			s.metrics.AsyncEnqueued.Add(1)
			status = http.StatusAccepted
			log.Debug("queued", "messages", n)
			writeJSON(w, status, map[string]any{"ErrorCode": 0, "Message": "Queued", "RequestID": rid, "Messages": n})
			return
		}

		res, err := s.forward(r.Context(), ep, out, n, log)
		if err != nil {
			if r.Context().Err() != nil {
				status = 499 // client went away; nothing to write
				return
			}
			fail(&apiError{Status: http.StatusBadGateway, ErrorCode: codeUpstream, Message: "could not reach Postmark: " + err.Error()})
			return
		}
		status = res.StatusCode
		ct := res.ContentType
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(res.StatusCode)
		w.Write(res.Body)
	}
}

// forward sends a prepared body to Postmark and records the outcome.
func (s *Server) forward(ctx context.Context, ep endpoint, body []byte, n int, log *slog.Logger) (*postmark.Response, error) {
	start := time.Now()
	res, err := s.sender.Post(ctx, ep.path, body)
	elapsed := time.Since(start)
	if err != nil {
		s.metrics.Upstream(ep.name, "error", elapsed)
		s.metrics.MessagesFailed.Add(uint64(n))
		log.Warn("postmark request failed", "error", err, "messages", n, "duration", elapsed)
		return nil, err
	}
	s.metrics.Upstream(ep.name, strconv.Itoa(res.StatusCode), elapsed)
	if res.Attempts > 1 {
		s.metrics.Retries.Add(uint64(res.Attempts - 1))
	}
	sent, failed, firstErr := outcome(ep, res.StatusCode, res.Body, n)
	s.metrics.MessagesSent.Add(uint64(sent))
	s.metrics.MessagesFailed.Add(uint64(failed))
	if failed > 0 {
		log.Warn("postmark rejected messages", "status", res.StatusCode, "sent", sent, "failed", failed, "error", firstErr, "duration", elapsed)
	} else {
		log.Debug("sent", "messages", sent, "attempts", res.Attempts, "duration", elapsed)
	}
	return res, nil
}

// deliver is the async queue worker.
func (s *Server) deliver(ctx context.Context, j queue.Job) {
	log := s.log.With("request_id", j.ID, "endpoint", j.Endpoint, "async", true)
	if ctx.Err() != nil {
		s.metrics.AsyncDropped.Add(1)
		s.metrics.MessagesFailed.Add(uint64(j.Messages))
		log.Error("dropping queued request at shutdown", "messages", j.Messages)
		return
	}
	for _, ep := range endpoints {
		if ep.path == j.Path {
			s.forward(ctx, ep, j.Body, j.Messages, log)
			return
		}
	}
}

// authenticate enforces POSTMARKD_AUTH_TOKENS. Clients may present a token
// as "Authorization: Bearer <token>" or, so unmodified Postmark SDKs work,
// as "X-Postmark-Server-Token: <token>".
func (s *Server) authenticate(next http.Handler) http.Handler {
	if len(s.tokens) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Postmark-Server-Token")
		if got == "" {
			if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
				got = strings.TrimSpace(v)
			}
		}
		ok := 0
		for _, t := range s.tokens {
			ok |= subtle.ConstantTimeCompare([]byte(got), t)
		}
		if got == "" || ok == 0 {
			s.metrics.Request("auth", http.StatusUnauthorized)
			w.Header().Set("WWW-Authenticate", `Bearer realm="postmarkd"`)
			writeJSON(w, http.StatusUnauthorized, &apiError{ErrorCode: codeUnauthorized, Message: "missing or invalid API token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic serving request", "path", r.URL.Path, "panic", v)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func isAsync(r *http.Request) bool {
	v := r.Header.Get("X-Postmarkd-Async")
	if v == "" {
		v = r.URL.Query().Get("async")
	}
	b, _ := strconv.ParseBool(v)
	return b
}

func requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); id != "" && len(id) <= 128 {
		return id
	}
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
