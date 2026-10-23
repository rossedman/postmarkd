package smtpd

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"testing"
)

type captured struct {
	mu   sync.Mutex
	envs []Envelope
	data []string
}

func start(t *testing.T, s *Server) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.Hostname = "test.local"
	s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { _ = s.Serve(l) }()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return l.Addr().String()
}

func newServer(c *captured) *Server {
	return &Server{
		MaxSize:       1024,
		MaxRecipients: 2,
		Handler: func(_ context.Context, env Envelope, data []byte) (string, error) {
			if strings.Contains(string(data), "reject-me") {
				return "", &Error{Code: 554, Message: "5.6.0 Postmark said no"}
			}
			c.mu.Lock()
			c.envs = append(c.envs, env)
			c.data = append(c.data, string(data))
			c.mu.Unlock()
			return "id-1", nil
		},
	}
}

func TestSendMail(t *testing.T) {
	c := &captured{}
	addr := start(t, newServer(c))
	msg := "From: a@example.com\r\nTo: b@example.com\r\nSubject: hi\r\n\r\n.leading dot\r\nbody\r\n"
	if err := smtp.SendMail(addr, nil, "a@example.com", []string{"b@example.com", "c@example.com"}, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	if len(c.envs) != 1 || c.envs[0].From != "a@example.com" || len(c.envs[0].To) != 2 {
		t.Fatalf("envs = %+v", c.envs)
	}
	if c.data[0] != strings.ReplaceAll(msg, "\r\n", "\n") {
		t.Errorf("data = %q", c.data[0])
	}
}

func TestTooManyRecipientsAndRejection(t *testing.T) {
	c := &captured{}
	addr := start(t, newServer(c))
	err := smtp.SendMail(addr, nil, "a@example.com", []string{"1@x.com", "2@x.com", "3@x.com"}, []byte("Subject: x\r\n\r\nx\r\n"))
	if err == nil || !strings.Contains(err.Error(), "452") {
		t.Errorf("err = %v, want 452", err)
	}
	err = smtp.SendMail(addr, nil, "a@example.com", []string{"1@x.com"}, []byte("Subject: x\r\n\r\nreject-me\r\n"))
	if err == nil || !strings.Contains(err.Error(), "554") {
		t.Errorf("err = %v, want 554", err)
	}
}

func TestTooBig(t *testing.T) {
	c := &captured{}
	addr := start(t, newServer(c))
	err := smtp.SendMail(addr, nil, "a@example.com", []string{"1@x.com"}, []byte("Subject: x\r\n\r\n"+strings.Repeat("x", 2000)+"\r\n"))
	if err == nil || !strings.Contains(err.Error(), "552") {
		t.Errorf("err = %v, want 552", err)
	}
}

func TestAuthRequired(t *testing.T) {
	c := &captured{}
	s := newServer(c)
	s.Auth = func(_, pass string) bool { return pass == "secret" }
	addr := start(t, s)
	msg := []byte("Subject: x\r\n\r\nx\r\n")
	if err := smtp.SendMail(addr, nil, "a@example.com", []string{"1@x.com"}, msg); err == nil || !strings.Contains(err.Error(), "530") {
		t.Errorf("no auth: err = %v", err)
	}
	host, _, _ := net.SplitHostPort(addr)
	if err := smtp.SendMail(addr, smtp.PlainAuth("", "app", "wrong", host), "a@example.com", []string{"1@x.com"}, msg); err == nil {
		t.Error("wrong password accepted")
	}
	if err := smtp.SendMail(addr, smtp.PlainAuth("", "app", "secret", host), "a@example.com", []string{"1@x.com"}, msg); err != nil {
		t.Errorf("good auth: %v", err)
	}
}

func TestParsePath(t *testing.T) {
	for _, c := range []struct{ in, addr, size string }{
		{"FROM:<a@b.com> SIZE=100 BODY=8BITMIME", "a@b.com", "100"},
		{"from: <>", "", ""},
		{"FROM:<@relay:a@b.com>", "a@b.com", ""},
	} {
		addr, params, ok := parsePath(c.in, "FROM:")
		if !ok || addr != c.addr || params["SIZE"] != c.size {
			t.Errorf("%q: %q %v %v", c.in, addr, params, ok)
		}
	}
}
