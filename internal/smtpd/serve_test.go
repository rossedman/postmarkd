package smtpd

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
)

// fakeListener returns the results queued in accepts, then blocks until
// closed.
type fakeListener struct {
	accepts chan acceptResult
	closed  chan struct{}
	once    sync.Once
}

type acceptResult struct {
	conn net.Conn
	err  error
	// before runs inside Accept, before it returns.
	before func()
}

func newFakeListener(results ...acceptResult) *fakeListener {
	l := &fakeListener{accepts: make(chan acceptResult, len(results)), closed: make(chan struct{})}
	for _, r := range results {
		l.accepts <- r
	}
	return l
}

func (l *fakeListener) Accept() (net.Conn, error) {
	select {
	case r := <-l.accepts:
		if r.before != nil {
			r.before()
		}
		return r.conn, r.err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *fakeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeListener) Addr() net.Addr { return &net.TCPAddr{} }

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "accept timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestServeAfterShutdown(t *testing.T) {
	t.Parallel()
	s := &Server{Log: discardLog()}
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Serve(newFakeListener()); !errors.Is(err, ErrServerClosed) {
		t.Errorf("Serve = %v, want ErrServerClosed", err)
	}
}

func TestServeAcceptErrors(t *testing.T) {
	t.Parallel()
	fatal := errors.New("listener broken")
	for name, tc := range map[string]struct {
		results []acceptResult
		want    error
	}{
		"permanent error returned": {
			results: []acceptResult{{err: fatal}},
			want:    fatal,
		},
		"temporary errors retried": {
			results: []acceptResult{{err: timeoutErr{}}, {err: syscall.EMFILE}, {err: timeoutErr{}}, {err: fatal}},
			want:    fatal,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := &Server{Log: discardLog()}
			if err := s.Serve(newFakeListener(tc.results...)); !errors.Is(err, tc.want) {
				t.Errorf("Serve = %v, want %v", err, tc.want)
			}
		})
	}
}

// closeConn records Close on a net.Pipe end.
type closeConn struct {
	net.Conn
	closed chan struct{}
}

func (c *closeConn) Close() error {
	close(c.closed)
	return c.Conn.Close()
}

func TestServeClosesConnAcceptedDuringShutdown(t *testing.T) {
	t.Parallel()
	server, other := net.Pipe()
	defer func() { _ = other.Close() }()
	c := &closeConn{Conn: server, closed: make(chan struct{})}
	s := &Server{Log: discardLog()}
	// Shutdown begins while Accept is returning a connection.
	l := newFakeListener(acceptResult{conn: c, before: func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
	}})
	if err := s.Serve(l); !errors.Is(err, ErrServerClosed) {
		t.Errorf("Serve = %v, want ErrServerClosed", err)
	}
	select {
	case <-c.closed:
	default:
		t.Error("connection accepted during shutdown was not closed")
	}
}

func TestServeReturnsErrServerClosedOnShutdown(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Hostname: "test.local", Log: discardLog()}
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	// A served connection proves Serve has registered the listener.
	c := dial(t, "tcp", l.Addr().String())
	c.expect(t, "220 ")
	c.send(t, "QUIT")
	c.expect(t, "221 ")
	if err := s.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown = %v", err)
	}
	if err := <-done; !errors.Is(err, ErrServerClosed) {
		t.Errorf("Serve = %v, want ErrServerClosed", err)
	}
}

// notifyListener signals when Shutdown closes it; Shutdown marks the server
// closing before closing listeners.
type notifyListener struct {
	net.Listener
	once   sync.Once
	closed chan struct{}
}

// Close signals closed, then closes the listener.
func (l *notifyListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

// readingListener hands out connections that signal on reading each time
// the server starts a Read, so a test can act while the server is blocked
// waiting for a command.
type readingListener struct {
	*notifyListener
	reading chan struct{}
}

// Accept wraps the accepted connection.
func (l *readingListener) Accept() (net.Conn, error) {
	c, err := l.notifyListener.Accept()
	if err != nil {
		return nil, err
	}
	return &readingConn{Conn: c, reading: l.reading}, nil
}

// readingConn signals before each Read.
type readingConn struct {
	net.Conn
	reading chan struct{}
}

// Read signals reading without blocking, then reads.
func (c *readingConn) Read(p []byte) (int, error) {
	select {
	case c.reading <- struct{}{}:
	default:
	}
	return c.Conn.Read(p)
}

func TestShutdownLetsSessionFinishCommand(t *testing.T) {
	t.Parallel()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &readingListener{
		notifyListener: &notifyListener{Listener: inner, closed: make(chan struct{})},
		reading:        make(chan struct{}, 1), // keeps a signal sent before the test waits
	}
	s := &Server{Hostname: "test.local", Log: discardLog()}
	go func() { _ = s.Serve(l) }()

	c := dial(t, "tcp", l.Addr().String())
	c.expect(t, "220 ")
	// Shut down only once the session is blocked reading its next command;
	// before that, the session would see the shutdown and reply 421 at once.
	<-l.reading
	shut := make(chan error, 1)
	go func() { shut <- s.Shutdown(context.Background()) }()
	<-l.closed

	// The in-flight session gets its reply, then is told the server is
	// going away, and Shutdown returns once it has ended.
	c.send(t, "NOOP")
	c.expect(t, "250 2.0.0 OK")
	c.expect(t, "421 4.3.2 Service shutting down")
	if err := <-shut; err != nil {
		t.Errorf("Shutdown = %v", err)
	}
}

func TestShutdownDeadlineClosesSessions(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Hostname: "test.local", Log: discardLog()}
	go func() { _ = s.Serve(l) }()

	c := dial(t, "tcp", l.Addr().String())
	c.expect(t, "220 ") // the connection is registered before the greeting
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Shutdown = %v, want context.Canceled", err)
	}
	if line, err := c.r.ReadLine(); err == nil {
		t.Errorf("read %q, want closed connection", line)
	}
}
