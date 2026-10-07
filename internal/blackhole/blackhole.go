// Package blackhole provides a stand-in for the Postmark API that accepts
// every message, answers exactly like Postmark would, and delivers nothing.
// The most recent messages are kept in memory so tests can inspect them.
package blackhole

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rossedman/postmarkd/internal/postmark"
)

// Message is a captured message.
type Message struct {
	MessageID  string          `json:"MessageID"`
	ReceivedAt time.Time       `json:"ReceivedAt"`
	Path       string          `json:"Path"`
	Message    json.RawMessage `json:"Message"`
}

// Sender implements the same Post method as postmark.Client.
type Sender struct {
	mu   sync.Mutex
	keep int
	buf  []Message // ring buffer
	next int
	full bool
}

// New returns a Sender that keeps the last keep messages (0 keeps none).
func New(keep int) *Sender {
	return &Sender{keep: keep, buf: make([]Message, keep)}
}

type result struct {
	To          string `json:"To"`
	SubmittedAt string `json:"SubmittedAt"`
	MessageID   string `json:"MessageID"`
	ErrorCode   int    `json:"ErrorCode"`
	Message     string `json:"Message"`
}

// Post accepts a Postmark send request.
func (s *Sender) Post(_ context.Context, path string, body []byte) (*postmark.Response, error) {
	var msgs []json.RawMessage
	batch := true
	switch path {
	case "/email", "/email/withTemplate":
		msgs, batch = []json.RawMessage{body}, false
	case "/email/batch":
		if err := json.Unmarshal(body, &msgs); err != nil {
			return nil, err
		}
	case "/email/batchWithTemplates":
		var w struct{ Messages []json.RawMessage }
		if err := json.Unmarshal(body, &w); err != nil {
			return nil, err
		}
		msgs = w.Messages
	default:
		return &postmark.Response{StatusCode: 404, ContentType: "application/json", Body: []byte(`{"ErrorCode":404,"Message":"not found"}`), Attempts: 1}, nil
	}

	now := time.Now().UTC()
	results := make([]result, len(msgs))
	for i, m := range msgs {
		var to struct{ To string }
		json.Unmarshal(m, &to)
		id := uuid()
		results[i] = result{To: to.To, SubmittedAt: now.Format(time.RFC3339Nano), MessageID: id, Message: "OK"}
		s.store(Message{MessageID: id, ReceivedAt: now, Path: path, Message: m})
	}
	var out []byte
	if batch {
		out, _ = json.Marshal(results)
	} else {
		out, _ = json.Marshal(results[0])
	}
	return &postmark.Response{StatusCode: 200, ContentType: "application/json", Body: out, Attempts: 1}, nil
}

func (s *Sender) store(m Message) {
	if s.keep == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf[s.next] = m
	s.next = (s.next + 1) % s.keep
	if s.next == 0 {
		s.full = true
	}
}

// Messages returns captured messages, oldest first.
func (s *Sender) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.full {
		return append([]Message(nil), s.buf[:s.next]...)
	}
	return append(append([]Message(nil), s.buf[s.next:]...), s.buf[:s.next]...)
}

// Reset discards captured messages.
func (s *Sender) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.buf)
	s.next, s.full = 0, false
}

func uuid() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
