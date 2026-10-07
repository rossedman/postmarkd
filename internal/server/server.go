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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rossedman/postmarkd/internal/blackhole"
	"github.com/rossedman/postmarkd/internal/config"
	"github.com/rossedman/postmarkd/internal/metrics"
	"github.com/rossedman/postmarkd/internal/postmark"
	"github.com/rossedman/postmarkd/internal/queue"
	"github.com/rossedman/postmarkd/internal/rfc822"
	"github.com/rossedman/postmarkd/internal/smtpd"
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
	if bh, ok := sender.(*blackhole.Sender); ok {
		mux.Handle("GET /blackhole/messages", s.authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"Messages": bh.Messages()})
		})))
		mux.Handle("DELETE /blackhole/messages", s.authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			bh.Reset()
			w.WriteHeader(http.StatusNoContent)
		})))
	}
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

		d := s.defaults
		if stream := r.Header.Get("X-Postmarkd-Stream"); stream != "" {
			d.MessageStream = stream
		} else if stream := r.URL.Query().Get("stream"); stream != "" {
			d.MessageStream = stream
		}
		out, n, aerr := prepare(ep, body, d)
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
		if ep.name == j.Endpoint {
			s.forward(ctx, ep, j.Body, j.Messages, log)
			return
		}
	}
}

var smtpEndpoint = endpoint{"smtp", "/email", kindEmail}

// HandleSMTP converts a message received over SMTP (or from the sendmail
// command) and sends it synchronously, mapping the outcome to SMTP replies
// so the client knows whether to retry.
func (s *Server) HandleSMTP(ctx context.Context, env smtpd.Envelope, data []byte) (string, error) {
	status := http.StatusOK
	defer func() { s.metrics.Request(smtpEndpoint.name, status) }()
	reject := func(st, code int, msg string) (string, error) {
		status = st
		return "", &smtpd.Error{Code: code, Message: msg}
	}

	msg, err := rfc822.Convert(data, rfc822.Envelope{From: env.From, To: env.To})
	if err != nil {
		return reject(http.StatusUnprocessableEntity, 554, "5.6.0 "+err.Error())
	}
	body, _ := json.Marshal(msg)
	out, n, aerr := prepare(smtpEndpoint, body, s.defaults)
	if aerr != nil {
		return reject(aerr.Status, 554, "5.6.0 "+aerr.Message)
	}
	rid := newID()
	log := s.log.With("request_id", rid, "endpoint", smtpEndpoint.name)
	res, err := s.forward(ctx, smtpEndpoint, out, n, log)
	if err != nil {
		return reject(http.StatusBadGateway, 451, "4.4.1 Postmark unreachable, try again later")
	}
	status = res.StatusCode
	var r sendResult
	json.Unmarshal(res.Body, &r)
	switch {
	case res.StatusCode == 200 && r.ErrorCode == 0:
		var ok struct{ MessageID string }
		json.Unmarshal(res.Body, &ok)
		return ok.MessageID, nil
	case res.StatusCode == 429 || res.StatusCode >= 500:
		return "", &smtpd.Error{Code: 451, Message: fmt.Sprintf("4.3.0 Postmark error %d: %s", r.ErrorCode, r.Message)}
	default:
		return "", &smtpd.Error{Code: 554, Message: fmt.Sprintf("5.7.1 Postmark error %d: %s", r.ErrorCode, r.Message)}
	}
}

// SMTPAuth checks SMTP AUTH credentials against the client tokens; the
// token is the password and the username is ignored. It returns nil when
// authentication is disabled.
func (s *Server) SMTPAuth() func(user, pass string) bool {
	if len(s.tokens) == 0 {
		return nil
	}
	return func(_, pass string) bool { return s.validToken(pass) }
}

func (s *Server) validToken(got string) bool {
	ok := 0
	for _, t := range s.tokens {
		ok |= subtle.ConstantTimeCompare([]byte(got), t)
	}
	return got != "" && ok == 1
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
		if !s.validToken(got) {
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
	return newID()
}

func newID() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
