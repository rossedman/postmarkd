package smtpd

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/textproto"
	"regexp"
	"strings"
	"testing"
	"time"
)

var replyLine = regexp.MustCompile(`^\d{3}[ -]`)

const (
	// maxLine is the longest command line the server accepts; AUTH
	// responses sent after a 334 challenge are not commands and have no
	// limit.
	maxLine = 4096

	fuzzMaxSize       = 512
	fuzzMaxRecipients = 3
)

// FuzzSession plays arbitrary client input, pipelined, against a session
// and checks the protocol invariants that hold whatever the client sends.
func FuzzSession(f *testing.F) {
	for _, seed := range []string{
		cmds("EHLO c", "MAIL FROM:<a@x> SIZE=10", "RCPT TO:<b@x>", "DATA") + "Subject: x\r\n\r\n..stuffed\r\nbody\r\n.\r\n" + cmds("QUIT"),
		cmds("HELO c", "MAIL FROM:<a@x>", "RCPT TO:<b@x>", "RCPT TO:<c@x>", "RCPT TO:<d@x>", "RCPT TO:<e@x>", "DATA") + "x\r\n.\r\n",
		cmds("EHLO c", "MAIL FROM:<a@x>", "RCPT TO:<b@x>", "DATA") + strings.Repeat("too big ", 100) + "\r\n.\r\n" + cmds("NOOP"),
		cmds("MAIL FROM:<a@x>", "RCPT TO:<b@x>", "DATA") + "reject\r\n.\r\n" + cmds("MAIL FROM:<a@x>", "RCPT TO:<b@x>", "DATA") + "fail\r\n.\r\n",
		cmds("AUTH PLAIN "+b64("\x00app\x00secret"), "MAIL FROM:<a@x>", "RCPT TO:<b@x>", "DATA") + "hi\r\n.\r\n",
		cmds("AUTH PLAIN", b64("\x00app\x00secret"), "AUTH LOGIN", b64("app"), b64("pw")),
		cmds("AUTH LOGIN "+b64("app"), b64("pw"), "AUTH PLAIN !!", "AUTH XOAUTH2 abc"),
		cmds("MAIL FROM:<a@x", "RCPT TO:<>", "DATA", "RSET", "VRFY x", "HELP", "STARTTLS", "BOGUS", "NOOP "+strings.Repeat("x", 5000)),
		cmds("MAIL FROM:<@r1,@r2:a@x> BODY=8BITMIME SMTPUTF8", "RCPT TO:<b@x>", "DATA") + ".\n" + cmds("QUIT", "NOOP"),
		cmds("MAIL FROM:<a@x>", "RCPT TO:<b@x>", "DATA") + "unterminated\r\n.",
		"EHLO c\nMAIL FROM:<a@x>\nRCPT TO:<b@x>\nDATA\nbare\rcr\n.\n",
		"",
	} {
		f.Add([]byte(seed), false)
		f.Add([]byte(seed), true)
	}

	f.Fuzz(func(t *testing.T, script []byte, withAuth bool) {
		var delivered []Envelope
		var sizes []int
		authCalls := 0
		s := &Server{
			Hostname:      "fuzz.local",
			Log:           discardLog(),
			MaxSize:       fuzzMaxSize,
			MaxRecipients: fuzzMaxRecipients,
			Handler: func(_ context.Context, env Envelope, data []byte) (string, error) {
				delivered = append(delivered, env)
				sizes = append(sizes, len(data))
				switch {
				case strings.Contains(string(data), "reject"):
					return "", &Error{Code: 554, Message: "5.6.0 rejected\r\nwith a second line"}
				case strings.Contains(string(data), "fail"):
					return "", errors.New("upstream\nunavailable")
				}
				return "queued", nil
			},
		}
		if withAuth {
			// Always accept: a failed AUTH sleeps to slow down guessing.
			s.Auth = func(string, string) bool { authCalls++; return true }
		}
		c := newScriptConn(string(script))
		s.serveConn(c)

		out := c.out.String()
		if !strings.HasSuffix(out, "\r\n") {
			t.Fatalf("output does not end in CRLF: %q", out)
		}
		lines := strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n")
		if lines[0] != "220 fuzz.local ESMTP postmarkd ready" {
			t.Fatalf("greeting = %q", lines[0])
		}
		queued, authOK := 0, 0
		for i, l := range lines {
			if !replyLine.MatchString(l) {
				t.Fatalf("reply line %d = %q", i, l)
			}
			if strings.ContainsAny(l, "\n") {
				t.Fatalf("reply line %d contains LF: %q", i, l)
			}
			if l[3] == '-' && (i+1 == len(lines) || lines[i+1][:3] != l[:3]) {
				t.Fatalf("multi-line reply %q not continued with the same code", l)
			}
			switch {
			case l == "250 2.0.0 OK queued as queued":
				queued++
			case strings.HasPrefix(l, "235 "):
				authOK++
			}
		}

		accepted := 0
		for i, env := range delivered {
			if sizes[i] > fuzzMaxSize {
				t.Errorf("delivered %d bytes, over the %d byte limit", sizes[i], fuzzMaxSize)
			}
			if len(env.To) == 0 || len(env.To) > fuzzMaxRecipients {
				t.Errorf("delivered with %d recipients", len(env.To))
			}
			for _, r := range env.To {
				if r == "" {
					t.Error("delivered to an empty recipient")
				}
			}
			accepted++
		}
		if queued > accepted {
			t.Errorf("%d messages reported queued, handler saw %d", queued, accepted)
		}
		if authOK != authCalls {
			t.Errorf("%d AUTH successes, Auth called %d times", authOK, authCalls)
		}
		if withAuth && authOK == 0 && len(delivered) > 0 {
			t.Errorf("delivered %d messages without authenticating", len(delivered))
		}
	})
}

// pipeSession starts s on one end of a net.Pipe and returns a lockstep
// client on the other. The session must have ended when the test returns.
func pipeSession(t *testing.T, s *Server) (net.Conn, *textproto.Reader) {
	t.Helper()
	server, conn := net.Pipe()
	// A server that stops replying fails the test instead of hanging it.
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = server.Close() }()
		s.serveConn(server)
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})
	r := textproto.NewReader(bufio.NewReader(conn))
	return conn, r
}

func pipeSend(t *testing.T, conn net.Conn, s string) {
	t.Helper()
	if _, err := conn.Write([]byte(s)); err != nil {
		t.Fatalf("writing %q: %v", s, err)
	}
}

func pipeExpect(t *testing.T, r *textproto.Reader, want string) {
	t.Helper()
	line, err := r.ReadLine()
	if err != nil {
		t.Fatalf("reading reply, want %q: %v", want, err)
	}
	if line != want {
		t.Fatalf("reply = %q, want %q", line, want)
	}
}

// dotStuff encodes lines as DATA content: a leading dot is doubled and
// every line ends in CRLF, followed by the terminating dot.
func dotStuff(lines []string) string {
	var b strings.Builder
	for _, l := range lines {
		if strings.HasPrefix(l, ".") {
			b.WriteByte('.')
		}
		b.WriteString(l + "\r\n")
	}
	b.WriteString(".\r\n")
	return b.String()
}

// FuzzData sends an arbitrary, correctly dot-stuffed message and checks the
// handler receives exactly the original content (with LF line endings) when
// it fits the size limit, and that larger messages are refused with 552.
func FuzzData(f *testing.F) {
	for _, seed := range []string{
		"Subject: hi\n\nbody\n",
		".leading dot\n..two dots\n.\n",
		"line with bare \r carriage return\nends with cr\r\n\r",
		"",
		"\n\n\n",
		strings.Repeat("0123456789", 30),
		".",
		"\r.\r\n.\r",
	} {
		f.Add(seed, uint16(64))
		f.Add(seed, uint16(1024))
	}

	f.Fuzz(func(t *testing.T, body string, size uint16) {
		maxSize := int64(size%2048) + 1
		lines := strings.Split(body, "\n")
		want := strings.Join(lines, "\n") + "\n"

		var got []string
		s := &Server{
			Hostname: "fuzz.local",
			Log:      discardLog(),
			MaxSize:  maxSize,
			Handler: func(_ context.Context, _ Envelope, data []byte) (string, error) {
				got = append(got, string(data))
				return "id", nil
			},
		}
		conn, r := pipeSession(t, s)
		pipeExpect(t, r, "220 fuzz.local ESMTP postmarkd ready")
		pipeSend(t, conn, "MAIL FROM:<a@x>\r\n")
		pipeExpect(t, r, "250 2.1.0 OK")
		pipeSend(t, conn, "RCPT TO:<b@x>\r\n")
		pipeExpect(t, r, "250 2.1.5 OK")
		pipeSend(t, conn, "DATA\r\n")
		pipeExpect(t, r, "354 Start mail input; end with <CRLF>.<CRLF>")
		pipeSend(t, conn, dotStuff(lines))
		if int64(len(want)) > maxSize {
			pipeExpect(t, r, "552 5.3.4 Message too big")
		} else {
			pipeExpect(t, r, "250 2.0.0 OK queued as id")
		}
		// The session is back in sync after the message either way.
		pipeSend(t, conn, "NOOP\r\n")
		pipeExpect(t, r, "250 2.0.0 OK")
		pipeSend(t, conn, "QUIT\r\n")
		pipeExpect(t, r, "221 2.0.0 Bye")

		if int64(len(want)) > maxSize {
			if len(got) != 0 {
				t.Fatalf("oversized message (%d > %d) delivered", len(want), maxSize)
			}
			return
		}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("delivered %q, want %q", got, want)
		}
	})
}

// FuzzAuth authenticates with arbitrary credentials using each mechanism
// and form, and checks Auth sees exactly what the client encoded.
func FuzzAuth(f *testing.F) {
	for _, seed := range []struct {
		authz, user, pass string
	}{
		{"", "app", "secret"},
		{"admin", "app", "pa ss"},
		{"", "", ""},
		{"", "ünïcödé", "\xff\xfe"},
		{"", "nul\x00inside", "x"},
		{"", "app", "trailing space "},
		{"", strings.Repeat("u", 3100), strings.Repeat("p", 5000)},
	} {
		for mode := range uint8(4) {
			f.Add(seed.authz, seed.user, seed.pass, mode)
		}
	}

	f.Fuzz(func(t *testing.T, authz, user, pass string, mode uint8) {
		login, initial := mode&1 == 1, mode&2 == 2
		var gotUser, gotPass string
		calls := 0
		s := &Server{
			Hostname: "fuzz.local",
			Log:      discardLog(),
			Handler:  func(context.Context, Envelope, []byte) (string, error) { return "id", nil },
			// Always accept: a failed AUTH sleeps to slow down guessing.
			Auth: func(u, p string) bool { gotUser, gotPass = u, p; calls++; return true },
		}
		conn, r := pipeSession(t, s)
		pipeExpect(t, r, "220 fuzz.local ESMTP postmarkd ready")

		wantOK := true
		if login {
			u := b64(user)
			if cmd := "AUTH LOGIN " + u; initial && u != "" {
				pipeSend(t, conn, cmd+"\r\n")
				if len(cmd) > maxLine {
					pipeExpect(t, r, "500 5.5.2 Line too long")
					return
				}
			} else {
				pipeSend(t, conn, "AUTH LOGIN\r\n")
				pipeExpect(t, r, "334 VXNlcm5hbWU6")
				pipeSend(t, conn, u+"\r\n")
			}
			pipeExpect(t, r, "334 UGFzc3dvcmQ6")
			pipeSend(t, conn, b64(pass)+"\r\n")
		} else {
			resp := b64(authz + "\x00" + user + "\x00" + pass)
			if cmd := "AUTH PLAIN " + resp; initial {
				pipeSend(t, conn, cmd+"\r\n")
				if len(cmd) > maxLine {
					pipeExpect(t, r, "500 5.5.2 Line too long")
					return
				}
			} else {
				pipeSend(t, conn, "AUTH PLAIN\r\n")
				pipeExpect(t, r, "334 ")
				pipeSend(t, conn, resp+"\r\n")
			}
			// NUL separates the fields, so it cannot occur inside them.
			wantOK = !strings.Contains(authz+user+pass, "\x00")
		}

		if wantOK {
			pipeExpect(t, r, "235 2.7.0 Authentication succeeded")
			pipeSend(t, conn, "MAIL FROM:<a@x>\r\n")
			pipeExpect(t, r, "250 2.1.0 OK")
			if calls != 1 || gotUser != user || gotPass != pass {
				t.Fatalf("Auth(%q, %q) called %d times, want (%q, %q)", gotUser, gotPass, calls, user, pass)
			}
		} else {
			pipeExpect(t, r, "501 5.5.2 Malformed AUTH PLAIN")
			pipeSend(t, conn, "MAIL FROM:<a@x>\r\n")
			pipeExpect(t, r, "530 5.7.0 Authentication required")
			if calls != 0 {
				t.Fatalf("Auth called for malformed credentials")
			}
		}
		pipeSend(t, conn, "QUIT\r\n")
		pipeExpect(t, r, "221 2.0.0 Bye")
	})
}
