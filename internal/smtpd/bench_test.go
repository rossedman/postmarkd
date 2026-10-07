package smtpd

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/smtp"
	"testing"
)

// BenchmarkTransaction measures a full SMTP transaction (MAIL, RCPT, DATA)
// on a reused connection, as an application's SMTP client pool would do.
func BenchmarkTransaction(b *testing.B) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	s := &Server{
		Hostname: "bench", MaxSize: 1 << 20, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Handler: func(context.Context, Envelope, []byte) (string, error) { return "id", nil },
	}
	go s.Serve(l)
	defer s.Shutdown(context.Background())

	msg := []byte("From: a@example.com\r\nTo: b@example.com\r\nSubject: hi\r\n\r\nHello there, this is a short message.\r\n")
	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		c, err := smtp.Dial(l.Addr().String())
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close()
		for pb.Next() {
			if err := c.Mail("a@example.com"); err != nil {
				b.Fatal(err)
			}
			if err := c.Rcpt("b@example.com"); err != nil {
				b.Fatal(err)
			}
			w, err := c.Data()
			if err != nil {
				b.Fatal(err)
			}
			w.Write(msg)
			if err := w.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
