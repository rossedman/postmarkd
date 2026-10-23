package smtpd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptConn is a net.Conn that reads a fixed client script and records
// everything the server writes. Once the script is consumed reads return
// io.EOF, so a session driven by serveConn runs to completion
// deterministically, including pipelined commands.
type scriptConn struct {
	r     *bytes.Reader
	out   bytes.Buffer
	local net.Addr
}

func newScriptConn(script string) *scriptConn {
	return &scriptConn{r: bytes.NewReader([]byte(script)), local: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 25}}
}

func (c *scriptConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *scriptConn) Write(p []byte) (int, error) { return c.out.Write(p) }
func (c *scriptConn) Close() error                { return nil }
func (c *scriptConn) LocalAddr() net.Addr         { return c.local }
func (c *scriptConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000}
}
func (c *scriptConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptConn) SetWriteDeadline(time.Time) error { return nil }

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// run plays script against s on an in-memory connection and returns the
// server's reply lines without the greeting.
func run(t *testing.T, s *Server, script string) []string {
	t.Helper()
	c := newScriptConn(script)
	return runConn(t, s, c)
}

func runConn(t *testing.T, s *Server, c *scriptConn) []string {
	t.Helper()
	if s.Hostname == "" {
		s.Hostname = "test.local"
	}
	if s.Log == nil {
		s.Log = discardLog()
	}
	s.serveConn(c)
	lines := splitReplies(t, c.out.String())
	if len(lines) == 0 || lines[0] != "220 test.local ESMTP postmarkd ready" {
		t.Fatalf("greeting = %q", lines)
	}
	return lines[1:]
}

// splitReplies splits server output into lines, failing if it does not end
// in CRLF.
func splitReplies(t *testing.T, out string) []string {
	t.Helper()
	if out == "" {
		return nil
	}
	if !strings.HasSuffix(out, "\r\n") {
		t.Fatalf("output does not end in CRLF: %q", out)
	}
	return strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n")
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func cmds(lines ...string) string { return strings.Join(lines, "\r\n") + "\r\n" }

func TestCommands(t *testing.T) {
	t.Parallel()
	long := "NOOP " + strings.Repeat("x", 4100)
	for _, tc := range []struct {
		name   string
		server *Server
		script string
		want   []string
	}{
		{
			name:   "EHLO minimal",
			script: cmds("EHLO client.example"),
			want: []string{
				"250-test.local greets client.example", "250-PIPELINING", "250-8BITMIME",
				"250-SMTPUTF8", "250 ENHANCEDSTATUSCODES",
			},
		},
		{
			name:   "EHLO with SIZE and AUTH",
			server: &Server{MaxSize: 1000, Auth: func(string, string) bool { return true }},
			script: cmds("ehlo c"),
			want: []string{
				"250-test.local greets c", "250-PIPELINING", "250-8BITMIME", "250-SMTPUTF8",
				"250-ENHANCEDSTATUSCODES", "250-SIZE 1000", "250 AUTH PLAIN LOGIN",
			},
		},
		{
			name:   "simple commands",
			script: cmds("HELO c", "NOOP", "VRFY root", "HELP", "STARTTLS", "BOGUS", "", "RSET", "QUIT", "NOOP"),
			want: []string{
				"250 test.local",
				"250 2.0.0 OK",
				"252 2.5.0 Cannot VRFY user, but will accept message",
				"214 2.0.0 See https://github.com/rossedman/postmarkd",
				"502 5.5.1 STARTTLS not supported; use a unix socket, loopback or a private network",
				"500 5.5.2 Unrecognized command",
				"500 5.5.2 Unrecognized command",
				"250 2.0.0 OK",
				"221 2.0.0 Bye",
			},
		},
		{
			name:   "line too long",
			script: cmds(long, "NOOP"),
			want:   []string{"500 5.5.2 Line too long", "250 2.0.0 OK"},
		},
		{
			name:   "MAIL errors",
			server: &Server{MaxSize: 100},
			script: cmds("MAIL", "MAIL TO:<a@x>", "MAIL FROM:<a@x", "MAIL FROM:<a@x> SIZE=101", "MAIL FROM:<a@x> SIZE=100", "MAIL FROM:<b@x>"),
			want: []string{
				"501 5.5.4 Syntax: MAIL FROM:<address>",
				"501 5.5.4 Syntax: MAIL FROM:<address>",
				"501 5.5.4 Syntax: MAIL FROM:<address>",
				"552 5.3.4 Message too big",
				"250 2.1.0 OK",
				"503 5.5.1 Nested MAIL command",
			},
		},
		{
			name:   "SIZE ignored without limit or when unparseable",
			script: cmds("MAIL FROM:<a@x> SIZE=999999999999", "RSET", "MAIL FROM:<a@x> SIZE=big"),
			want:   []string{"250 2.1.0 OK", "250 2.0.0 OK", "250 2.1.0 OK"},
		},
		{
			name:   "RCPT errors",
			server: &Server{MaxRecipients: 1},
			script: cmds("RCPT TO:<a@x>", "MAIL FROM:<>", "RCPT TO:<>", "RCPT FROM:<a@x>", "RCPT TO:<a@x>", "RCPT TO:<b@x>"),
			want: []string{
				"503 5.5.1 Need MAIL first",
				"250 2.1.0 OK",
				"501 5.5.4 Syntax: RCPT TO:<address>",
				"501 5.5.4 Syntax: RCPT TO:<address>",
				"250 2.1.5 OK",
				"452 4.5.3 Too many recipients",
			},
		},
		{
			name:   "DATA without transaction",
			script: cmds("DATA", "MAIL FROM:<a@x>", "DATA"),
			want:   []string{"503 5.5.1 Need MAIL first", "250 2.1.0 OK", "554 5.5.1 No valid recipients"},
		},
		{
			name:   "HELO and EHLO reset the transaction",
			script: cmds("MAIL FROM:<a@x>", "HELO c", "RCPT TO:<b@x>", "MAIL FROM:<a@x>", "EHLO c", "RCPT TO:<b@x>"),
			want: []string{
				"250 2.1.0 OK", "250 test.local", "503 5.5.1 Need MAIL first", "250 2.1.0 OK",
				"250-test.local greets c", "250-PIPELINING", "250-8BITMIME", "250-SMTPUTF8", "250 ENHANCEDSTATUSCODES",
				"503 5.5.1 Need MAIL first",
			},
		},
		{
			name:   "AUTH not offered",
			script: cmds("AUTH PLAIN AGFwcABzZWNyZXQ="),
			want:   []string{"502 5.5.1 AUTH not available"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := tc.server
			if s == nil {
				s = &Server{}
			}
			if s.Handler == nil {
				s.Handler = func(context.Context, Envelope, []byte) (string, error) { return "id", nil }
			}
			got := run(t, s, tc.script)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("replies:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

func TestData(t *testing.T) {
	t.Parallel()
	type call struct {
		env  Envelope
		data string
	}
	for _, tc := range []struct {
		name    string
		maxSize int64
		result  error
		body    string // after DATA, including the terminating line
		want    []string
		calls   []call
	}{
		{
			name:  "dot unstuffing and CRLF",
			body:  "Subject: x\r\n\r\n..leading\r\n.\r\n",
			want:  []string{"250 2.0.0 OK queued as id-1"},
			calls: []call{{Envelope{From: "a@x", To: []string{"b@x", "c@x"}}, "Subject: x\n\n.leading\n"}},
		},
		{
			name:    "exactly at limit",
			maxSize: 4,
			body:    "abc\r\n.\r\n",
			want:    []string{"250 2.0.0 OK queued as id-1"},
			calls:   []call{{Envelope{From: "a@x", To: []string{"b@x", "c@x"}}, "abc\n"}},
		},
		{
			name:    "one byte over limit",
			maxSize: 4,
			body:    "abcd\r\n.\r\n",
			want:    []string{"552 5.3.4 Message too big"},
		},
		{
			name:   "handler SMTP error",
			result: &Error{Code: 554, Message: "5.6.0 rejected\r\n by Postmark"},
			body:   "x\r\n.\r\n",
			want:   []string{"554 5.6.0 rejected by Postmark"},
			calls:  []call{{Envelope{From: "a@x", To: []string{"b@x", "c@x"}}, "x\n"}},
		},
		{
			name:   "handler other error is temporary",
			result: errors.New("postmark\nunreachable"),
			body:   "x\r\n.\r\n",
			want:   []string{"451 4.3.0 postmark unreachable"},
			calls:  []call{{Envelope{From: "a@x", To: []string{"b@x", "c@x"}}, "x\n"}},
		},
		{
			name: "wrapped SMTP error keeps its code",
			result: errors.Join(errors.New("context"),
				&Error{Code: 550, Message: "5.1.1 inactive recipient"}),
			body:  "x\r\n.\r\n",
			want:  []string{"550 5.1.1 inactive recipient"},
			calls: []call{{Envelope{From: "a@x", To: []string{"b@x", "c@x"}}, "x\n"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []call
			s := &Server{
				MaxSize: tc.maxSize,
				Handler: func(ctx context.Context, env Envelope, data []byte) (string, error) {
					if _, ok := ctx.Deadline(); !ok {
						t.Error("handler context has no deadline")
					}
					calls = append(calls, call{env, string(data)})
					return "id-1", tc.result
				},
			}
			got := run(t, s, cmds("MAIL FROM:<a@x>", "RCPT TO:<b@x>", "RCPT TO:<c@x>", "DATA")+tc.body+cmds("RCPT TO:<d@x>"))
			want := append([]string{"250 2.1.0 OK", "250 2.1.5 OK", "250 2.1.5 OK", "354 Start mail input; end with <CRLF>.<CRLF>"}, tc.want...)
			// The transaction is over whatever the outcome.
			want = append(want, "503 5.5.1 Need MAIL first")
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("replies:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			if len(calls) != len(tc.calls) {
				t.Fatalf("handler calls = %+v, want %+v", calls, tc.calls)
			}
			for i, c := range calls {
				w := tc.calls[i]
				if c.data != w.data || c.env.From != w.env.From || strings.Join(c.env.To, ",") != strings.Join(w.env.To, ",") {
					t.Errorf("call %d = %+v, want %+v", i, c, w)
				}
			}
		})
	}
}

func TestDataInterrupted(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"within limit": "partial",
		"over limit":   strings.Repeat("x", 100),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			called := false
			s := &Server{MaxSize: 10, Handler: func(context.Context, Envelope, []byte) (string, error) { called = true; return "id", nil }}
			got := run(t, s, cmds("MAIL FROM:<a@x>", "RCPT TO:<b@x>", "DATA")+body)
			if want := []string{"250 2.1.0 OK", "250 2.1.5 OK", "354 Start mail input; end with <CRLF>.<CRLF>"}; strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("replies = %q", got)
			}
			if called {
				t.Error("handler called for an incomplete message")
			}
		})
	}
}

func TestAuth(t *testing.T) {
	t.Parallel()
	const ok235 = "235 2.7.0 Authentication succeeded"
	for _, tc := range []struct {
		name   string
		script string
		want   []string
		creds  []string // user:pass pairs passed to Auth
	}{
		{
			name:   "required before MAIL",
			script: cmds("MAIL FROM:<a@x>"),
			want:   []string{"530 5.7.0 Authentication required"},
		},
		{
			name:   "PLAIN initial response",
			script: cmds("AUTH PLAIN "+b64("authz\x00app\x00secret"), "MAIL FROM:<a@x>"),
			want:   []string{ok235, "250 2.1.0 OK"},
			creds:  []string{"app:secret"},
		},
		{
			name:   "PLAIN challenge, lower case",
			script: cmds("auth plain", "  "+b64("\x00app\x00secret")+"  "),
			want:   []string{"334 ", ok235},
			creds:  []string{"app:secret"},
		},
		{
			name:   "PLAIN malformed",
			script: cmds("AUTH PLAIN !!!", "AUTH PLAIN "+b64("app\x00secret"), "MAIL FROM:<a@x>"),
			want:   []string{"501 5.5.2 Malformed AUTH PLAIN", "501 5.5.2 Malformed AUTH PLAIN", "530 5.7.0 Authentication required"},
		},
		{
			name:   "LOGIN with challenges",
			script: cmds("AUTH LOGIN", b64("app"), b64("pa ss")),
			want:   []string{"334 VXNlcm5hbWU6", "334 UGFzc3dvcmQ6", ok235},
			creds:  []string{"app:pa ss"},
		},
		{
			name:   "LOGIN with initial user",
			script: cmds("AUTH LOGIN "+b64("app"), b64("secret")),
			want:   []string{"334 UGFzc3dvcmQ6", ok235},
			creds:  []string{"app:secret"},
		},
		{
			name:   "LOGIN malformed user",
			script: cmds("AUTH LOGIN %%%", b64("secret")),
			want:   []string{"334 UGFzc3dvcmQ6", "501 5.5.2 Malformed AUTH LOGIN"},
		},
		{
			name:   "LOGIN malformed password",
			script: cmds("AUTH LOGIN", b64("app"), "%%%"),
			want:   []string{"334 VXNlcm5hbWU6", "334 UGFzc3dvcmQ6", "501 5.5.2 Malformed AUTH LOGIN"},
		},
		{
			name:   "unsupported mechanism",
			script: cmds("AUTH CRAM-MD5", "AUTH"),
			want:   []string{"504 5.5.4 Unsupported AUTH mechanism", "504 5.5.4 Unsupported AUTH mechanism"},
		},
		{
			name:   "already authenticated",
			script: cmds("AUTH PLAIN "+b64("\x00app\x00secret"), "AUTH PLAIN "+b64("\x00app\x00secret")),
			want:   []string{ok235, "503 5.5.1 Already authenticated"},
			creds:  []string{"app:secret"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var creds []string
			s := &Server{
				Handler: func(context.Context, Envelope, []byte) (string, error) { return "id", nil },
				Auth: func(user, pass string) bool {
					creds = append(creds, user+":"+pass)
					return true
				},
			}
			got := run(t, s, tc.script)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("replies:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
			if strings.Join(creds, ",") != strings.Join(tc.creds, ",") {
				t.Errorf("Auth called with %q, want %q", creds, tc.creds)
			}
		})
	}
}

func TestAuthFailure(t *testing.T) {
	t.Parallel()
	s := &Server{
		Handler: func(context.Context, Envelope, []byte) (string, error) { return "id", nil },
		Auth:    func(user, pass string) bool { return user == "app" && pass == "secret" },
	}
	got := run(t, s, cmds("AUTH LOGIN "+b64("app"), b64("wrong"), "MAIL FROM:<a@x>", "AUTH PLAIN "+b64("\x00app\x00secret"), "MAIL FROM:<a@x>"))
	want := []string{
		"334 UGFzc3dvcmQ6", "535 5.7.8 Authentication failed", "530 5.7.0 Authentication required",
		"235 2.7.0 Authentication succeeded", "250 2.1.0 OK",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("replies:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestAuthNotRequiredOnUnixSocket(t *testing.T) {
	t.Parallel()
	// A short directory: unix socket paths are limited to ~104 bytes.
	dir, err := os.MkdirTemp("", "smtpd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu  sync.Mutex
		got []string
	)
	s := &Server{
		Hostname: "test.local",
		Log:      discardLog(),
		Auth:     func(string, string) bool { t.Error("Auth called"); return false },
		Handler: func(_ context.Context, env Envelope, data []byte) (string, error) {
			mu.Lock()
			got = append(got, env.From+" "+string(data))
			mu.Unlock()
			return "q1", nil
		},
	}
	go func() { _ = s.Serve(l) }()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	c := dial(t, "unix", l.Addr().String())
	c.expect(t, "220 ")
	c.send(t, "MAIL FROM:<a@x>")
	c.expect(t, "250 2.1.0 OK")
	c.send(t, "RCPT TO:<b@x>")
	c.expect(t, "250 2.1.5 OK")
	c.send(t, "DATA")
	c.expect(t, "354 ")
	c.send(t, "hi\r\n.")
	c.expect(t, "250 2.0.0 OK queued as q1")
	c.send(t, "QUIT")
	c.expect(t, "221 ")

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "a@x hi\n" {
		t.Errorf("delivered %q", got)
	}
}

// client is a lockstep SMTP client for tests over real connections.
type client struct {
	conn net.Conn
	r    *textproto.Reader
}

func dial(t *testing.T, network, addr string) *client {
	t.Helper()
	conn, err := net.Dial(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &client{conn: conn, r: textproto.NewReader(bufio.NewReader(conn))}
}

func (c *client) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(c.conn, line+"\r\n"); err != nil {
		t.Fatal(err)
	}
}

// expect reads one reply line and checks its prefix.
func (c *client) expect(t *testing.T, prefix string) {
	t.Helper()
	line, err := c.r.ReadLine()
	if err != nil {
		t.Fatalf("reading reply, want %q: %v", prefix, err)
	}
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("reply = %q, want prefix %q", line, prefix)
	}
}

func TestErrorString(t *testing.T) {
	t.Parallel()
	if got := (&Error{Code: 451, Message: "4.3.0 try later"}).Error(); got != "451 4.3.0 try later" {
		t.Errorf("Error() = %q", got)
	}
}

func TestTimeout(t *testing.T) {
	t.Parallel()
	if got := (&Server{}).timeout(); got != 5*time.Minute {
		t.Errorf("default timeout = %v", got)
	}
	if got := (&Server{Timeout: time.Second}).timeout(); got != time.Second {
		t.Errorf("timeout = %v", got)
	}
}

func TestIdleTimeoutClosesSession(t *testing.T) {
	t.Parallel()
	s := &Server{Timeout: 50 * time.Millisecond, Handler: func(context.Context, Envelope, []byte) (string, error) { return "id", nil }}
	addr := start(t, s)
	c := dial(t, "tcp", addr)
	c.expect(t, "220 ")
	// Say nothing: the server must hang up once the deadline passes.
	if line, err := c.r.ReadLine(); err == nil {
		t.Fatalf("got %q, want connection closed", line)
	}
}

func TestParsePathCases(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in, prefix string
		addr       string
		params     map[string]string
		ok         bool
	}{
		{"FROM:<a@b.com> SIZE=100 body=8BITMIME", "FROM:", "a@b.com", map[string]string{"SIZE": "100", "BODY": "8BITMIME"}, true},
		{"from:   <a@b.com>", "FROM:", "a@b.com", map[string]string{}, true},
		{"FROM:a@b.com SMTPUTF8", "FROM:", "a@b.com", map[string]string{"SMTPUTF8": ""}, true},
		{"FROM:<@r1,@r2:a@b.com>", "FROM:", "a@b.com", map[string]string{}, true},
		{"FROM:<a:b@c>", "FROM:", "a:b@c", map[string]string{}, true},
		{"TO:<a@b.com>", "TO:", "a@b.com", map[string]string{}, true},
		{"FROM:<a@b.com", "FROM:", "", nil, false},
		{"TO:<a@b.com>", "FROM:", "", nil, false},
		{"FRO", "FROM:", "", nil, false},
		{"", "TO:", "", nil, false},
	} {
		addr, params, ok := parsePath(c.in, c.prefix)
		if addr != c.addr || ok != c.ok || len(params) != len(c.params) {
			t.Errorf("parsePath(%q) = %q, %v, %v", c.in, addr, params, ok)
			continue
		}
		for k, v := range c.params {
			if params[k] != v {
				t.Errorf("parsePath(%q) params = %v, want %v", c.in, params, c.params)
			}
		}
	}
}

func TestOneLine(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"a\r\nb":        "a b",
		"  x\t\ty  ":    "x y",
		"":              "",
		"one line only": "one line only",
	} {
		if got := oneLine(in); got != want {
			t.Errorf("oneLine(%q) = %q, want %q", in, got, want)
		}
	}
}
