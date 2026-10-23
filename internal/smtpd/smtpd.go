// Package smtpd is a minimal ESMTP server (RFC 5321) for accepting mail
// from local applications. It supports PIPELINING, SIZE, 8BITMIME,
// SMTPUTF8 and AUTH PLAIN/LOGIN; it does not implement STARTTLS and is
// meant for loopback, unix sockets and private container networks.
package smtpd

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Envelope is one SMTP transaction's sender and recipients.
type Envelope struct {
	From string
	To   []string
}

// Error is an SMTP reply returned by a Handler to reject a message.
type Error struct {
	Code    int    // e.g. 451 (temporary) or 554 (permanent)
	Message string // single line
}

func (e *Error) Error() string { return fmt.Sprintf("%d %s", e.Code, e.Message) }

// Handler delivers a message. It returns a queue/message ID on success; a
// non-*Error error is reported as a temporary failure.
type Handler func(ctx context.Context, env Envelope, data []byte) (id string, err error)

// Server is an SMTP server.
type Server struct {
	Hostname      string
	MaxSize       int64 // bytes, per message
	MaxRecipients int   // per transaction
	Handler       Handler
	// Auth validates AUTH credentials. When nil, AUTH is not offered. When
	// set, it is required except on unix socket connections.
	Auth    func(user, pass string) bool
	Log     *slog.Logger
	Timeout time.Duration // per command

	mu        sync.Mutex
	listeners map[net.Listener]struct{}
	conns     map[net.Conn]struct{}
	closing   bool
	wg        sync.WaitGroup
}

// ErrServerClosed is returned by Serve after Shutdown.
var ErrServerClosed = errors.New("smtpd: server closed")

// Serve accepts connections on l until Shutdown is called.
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return ErrServerClosed
	}
	if s.listeners == nil {
		s.listeners = map[net.Listener]struct{}{}
		s.conns = map[net.Conn]struct{}{}
	}
	s.listeners[l] = struct{}{}
	s.mu.Unlock()

	var backoff time.Duration
	for {
		c, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing {
				return ErrServerClosed
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, syscall.EMFILE) {
				backoff = min(max(backoff*2, 5*time.Millisecond), time.Second)
				time.Sleep(backoff)
				continue
			}
			return err
		}
		backoff = 0
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			_ = c.Close()
			return ErrServerClosed
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				_ = c.Close()
				s.wg.Done()
			}()
			s.serveConn(c)
		}()
	}
}

// Shutdown stops accepting connections and waits for sessions to finish
// their current transaction, closing any still open when ctx expires.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	for l := range s.listeners {
		_ = l.Close()
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		<-done
		return ctx.Err()
	}
}

func (s *Server) shuttingDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

type session struct {
	s        *Server
	conn     net.Conn
	r        *textproto.Reader
	w        *bufio.Writer
	log      *slog.Logger
	helo     string
	authed   bool
	needAuth bool
	env      *Envelope
}

func (s *Server) serveConn(c net.Conn) {
	sess := &session{
		s:        s,
		conn:     c,
		r:        textproto.NewReader(bufio.NewReader(c)),
		w:        bufio.NewWriter(c),
		log:      s.Log.With("remote", c.RemoteAddr().String()),
		needAuth: s.Auth != nil && c.LocalAddr().Network() != "unix",
	}
	sess.reply(220, s.Hostname+" ESMTP postmarkd ready")
	for {
		if s.shuttingDown() {
			sess.reply(421, "4.3.2 Service shutting down")
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(s.timeout())) // a failed read reports it
		line, err := sess.r.ReadLine()
		if err != nil {
			return
		}
		if len(line) > 4096 {
			sess.reply(500, "5.5.2 Line too long")
			continue
		}
		verb, arg, _ := strings.Cut(line, " ")
		if quit := sess.handle(strings.ToUpper(verb), strings.TrimSpace(arg)); quit {
			return
		}
	}
}

func (s *Server) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return 5 * time.Minute
}

func (sess *session) reply(code int, lines ...string) {
	for i, l := range lines {
		sep := " "
		if i < len(lines)-1 {
			sep = "-"
		}
		_, _ = fmt.Fprintf(sess.w, "%d%s%s\r\n", code, sep, l) // write errors surface on Flush
	}
	// Flush unless the client has pipelined more commands.
	if sess.r.R.Buffered() == 0 {
		sess.flush()
	}
}

func (sess *session) flush() {
	// A broken connection fails the next read, which ends the session.
	_ = sess.w.Flush()
}

func (sess *session) handle(verb, arg string) (quit bool) {
	s := sess.s
	switch verb {
	case "HELO":
		sess.helo, sess.env = arg, nil
		sess.reply(250, s.Hostname)
	case "EHLO":
		sess.helo, sess.env = arg, nil
		ext := []string{s.Hostname + " greets " + arg, "PIPELINING", "8BITMIME", "SMTPUTF8", "ENHANCEDSTATUSCODES"}
		if s.MaxSize > 0 {
			ext = append(ext, "SIZE "+strconv.FormatInt(s.MaxSize, 10))
		}
		if s.Auth != nil {
			ext = append(ext, "AUTH PLAIN LOGIN")
		}
		sess.reply(250, ext...)
	case "AUTH":
		sess.auth(arg)
	case "MAIL":
		if sess.needAuth && !sess.authed {
			sess.reply(530, "5.7.0 Authentication required")
			return
		}
		if sess.env != nil {
			sess.reply(503, "5.5.1 Nested MAIL command")
			return
		}
		from, params, ok := parsePath(arg, "FROM:")
		if !ok {
			sess.reply(501, "5.5.4 Syntax: MAIL FROM:<address>")
			return
		}
		if sz, err := strconv.ParseInt(params["SIZE"], 10, 64); err == nil && s.MaxSize > 0 && sz > s.MaxSize {
			sess.reply(552, "5.3.4 Message too big")
			return
		}
		sess.env = &Envelope{From: from}
		sess.reply(250, "2.1.0 OK")
	case "RCPT":
		if sess.env == nil {
			sess.reply(503, "5.5.1 Need MAIL first")
			return
		}
		to, _, ok := parsePath(arg, "TO:")
		if !ok || to == "" {
			sess.reply(501, "5.5.4 Syntax: RCPT TO:<address>")
			return
		}
		if s.MaxRecipients > 0 && len(sess.env.To) >= s.MaxRecipients {
			sess.reply(452, "4.5.3 Too many recipients")
			return
		}
		sess.env.To = append(sess.env.To, to)
		sess.reply(250, "2.1.5 OK")
	case "DATA":
		sess.data()
	case "RSET":
		sess.env = nil
		sess.reply(250, "2.0.0 OK")
	case "NOOP":
		sess.reply(250, "2.0.0 OK")
	case "VRFY":
		sess.reply(252, "2.5.0 Cannot VRFY user, but will accept message")
	case "HELP":
		sess.reply(214, "2.0.0 See https://github.com/rossedman/postmarkd")
	case "QUIT":
		sess.reply(221, "2.0.0 Bye")
		sess.flush()
		return true
	case "STARTTLS":
		sess.reply(502, "5.5.1 STARTTLS not supported; use a unix socket, loopback or a private network")
	default:
		sess.reply(500, "5.5.2 Unrecognized command")
	}
	return false
}

func (sess *session) data() {
	s := sess.s
	if sess.env == nil {
		sess.reply(503, "5.5.1 Need MAIL first")
		return
	}
	if len(sess.env.To) == 0 {
		sess.reply(554, "5.5.1 No valid recipients")
		return
	}
	sess.reply(354, "Start mail input; end with <CRLF>.<CRLF>")
	sess.flush()

	_ = sess.conn.SetReadDeadline(time.Now().Add(s.timeout())) // a failed read reports it
	limit := s.MaxSize
	if limit <= 0 {
		limit = 50 << 20
	}
	dr := sess.r.DotReader()
	data, err := io.ReadAll(io.LimitReader(dr, limit+1))
	env := *sess.env
	sess.env = nil
	if err != nil {
		return
	}
	if int64(len(data)) > limit {
		if _, err := io.Copy(io.Discard, dr); err == nil {
			sess.reply(552, "5.3.4 Message too big")
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout())
	defer cancel()
	id, err := s.Handler(ctx, env, data)
	if err != nil {
		var se *Error
		if !errors.As(err, &se) {
			se = &Error{Code: 451, Message: "4.3.0 " + err.Error()}
		}
		sess.log.Info("smtp message rejected", "from", env.From, "recipients", len(env.To), "code", se.Code, "error", se.Message)
		sess.reply(se.Code, oneLine(se.Message))
		return
	}
	sess.reply(250, "2.0.0 OK queued as "+id)
}

func (sess *session) auth(arg string) {
	s := sess.s
	if s.Auth == nil {
		sess.reply(502, "5.5.1 AUTH not available")
		return
	}
	if sess.authed {
		sess.reply(503, "5.5.1 Already authenticated")
		return
	}
	mech, initial, _ := strings.Cut(arg, " ")
	var user, pass string
	switch strings.ToUpper(mech) {
	case "PLAIN":
		resp := initial
		if resp == "" {
			sess.reply(334, "")
			resp = sess.readAuthLine()
		}
		b, err := base64.StdEncoding.DecodeString(resp)
		parts := strings.Split(string(b), "\x00")
		if err != nil || len(parts) != 3 {
			sess.reply(501, "5.5.2 Malformed AUTH PLAIN")
			return
		}
		user, pass = parts[1], parts[2]
	case "LOGIN":
		u := initial
		if u == "" {
			sess.reply(334, "VXNlcm5hbWU6") // "Username:"
			u = sess.readAuthLine()
		}
		sess.reply(334, "UGFzc3dvcmQ6") // "Password:"
		p := sess.readAuthLine()
		ub, err1 := base64.StdEncoding.DecodeString(u)
		pb, err2 := base64.StdEncoding.DecodeString(p)
		if err1 != nil || err2 != nil {
			sess.reply(501, "5.5.2 Malformed AUTH LOGIN")
			return
		}
		user, pass = string(ub), string(pb)
	default:
		sess.reply(504, "5.5.4 Unsupported AUTH mechanism")
		return
	}
	if !s.Auth(user, pass) {
		time.Sleep(500 * time.Millisecond) // slow down guessing
		sess.reply(535, "5.7.8 Authentication failed")
		return
	}
	sess.authed = true
	sess.reply(235, "2.7.0 Authentication succeeded")
}

func (sess *session) readAuthLine() string {
	sess.flush()
	_ = sess.conn.SetReadDeadline(time.Now().Add(sess.s.timeout())) // a failed read reports it
	l, _ := sess.r.ReadLine()
	return strings.TrimSpace(l)
}

// parsePath parses "FROM:<addr> PARAM=VALUE ..." (or TO:).
func parsePath(arg, prefix string) (string, map[string]string, bool) {
	if len(arg) < len(prefix) || !strings.EqualFold(arg[:len(prefix)], prefix) {
		return "", nil, false
	}
	rest := strings.TrimSpace(arg[len(prefix):])
	var addr string
	if strings.HasPrefix(rest, "<") {
		end := strings.IndexByte(rest, '>')
		if end < 0 {
			return "", nil, false
		}
		addr, rest = rest[1:end], rest[end+1:]
	} else {
		addr, rest, _ = strings.Cut(rest, " ")
	}
	// Strip source routes: <@a,@b:user@host>
	if i := strings.IndexByte(addr, ':'); i >= 0 && strings.HasPrefix(addr, "@") {
		addr = addr[i+1:]
	}
	params := map[string]string{}
	for _, f := range strings.Fields(rest) {
		k, v, _ := strings.Cut(f, "=")
		params[strings.ToUpper(k)] = v
	}
	return addr, params, true
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
