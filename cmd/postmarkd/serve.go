package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rossedman/postmarkd/internal/blackhole"
	"github.com/rossedman/postmarkd/internal/config"
	"github.com/rossedman/postmarkd/internal/metrics"
	"github.com/rossedman/postmarkd/internal/postmark"
	"github.com/rossedman/postmarkd/internal/server"
	"github.com/rossedman/postmarkd/internal/smtpd"
	"github.com/rossedman/postmarkd/internal/systemd"
)

// serve runs the daemon until SIGINT, SIGTERM or c.ctx ends, then drains
// in-flight and queued mail.
func (c *cli) serve(args []string) int {
	fl := c.flags("serve")
	configPath := fl.String("config", "", "path to a KEY=VALUE config file (environment variables override it)")
	check := fl.Bool("check-config", false, "validate the configuration and exit")
	showVersion := fl.Bool("version", false, "print the version and exit")
	fl.Usage = func() {
		_, _ = fmt.Fprint(fl.Output(), usage+"\nDaemon flags:\n")
		fl.PrintDefaults()
		_, _ = fmt.Fprint(fl.Output(), "\nAll settings are POSTMARKD_* environment variables; see docs/configuration.md.\n")
	}
	if code := parse(fl, args); code >= 0 {
		return code
	}

	if *showVersion {
		c.printf("postmarkd %s\n", version)
		return 0
	}

	cfg, err := config.Load(*configPath, os.Environ())
	if err != nil {
		c.errorf("postmarkd: invalid configuration:\n%s\n", indent(err.Error()))
		return 2
	}
	if *check {
		c.printf("configuration OK\n")
		return 0
	}
	log := newLogger(cfg, c.stderr)

	httpLs, smtpLs, err := listen(cfg)
	if err != nil {
		log.Error("cannot listen", "error", err)
		return 1
	}

	m := metrics.New()
	var sender server.Sender
	if cfg.Blackhole {
		sender = blackhole.New(cfg.BlackholeKeep)
		log.Warn("BLACKHOLE MODE: mail is accepted but never sent", "inspect", "GET /blackhole/messages")
	} else {
		client := postmark.New(postmark.Options{
			BaseURL:        cfg.APIURL,
			ServerToken:    cfg.ServerToken,
			MaxConcurrency: cfg.MaxConcurrency,
			MaxRetries:     cfg.MaxRetries,
			Timeout:        cfg.UpstreamTimeout,
			UserAgent:      "postmarkd/" + version,
		})
		m.Gauge("postmarkd_upstream_in_flight", "Requests to Postmark currently in flight.", func() float64 { return float64(client.InFlight()) })
		sender = client
	}
	m.Gauge("postmarkd_blackhole_mode", "1 if mail is accepted but not sent (POSTMARKD_BLACKHOLE).", func() float64 {
		if cfg.Blackhole {
			return 1
		}
		return 0
	})
	srv := server.New(cfg, sender, m, log)

	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// Long enough for every retry of a synchronous send.
		WriteTimeout:   time.Duration(cfg.MaxRetries+1)*cfg.UpstreamTimeout + 30*time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
		ErrorLog:       slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	hostname := cfg.SMTPHostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	smtpSrv := &smtpd.Server{
		Hostname:      hostname,
		MaxSize:       cfg.MaxBodyBytes,
		MaxRecipients: 50, // Postmark's per-message limit
		Handler:       srv.HandleSMTP,
		Auth:          srv.SMTPAuth(),
		Log:           log,
		Timeout:       time.Duration(cfg.MaxRetries+1)*cfg.UpstreamTimeout + 30*time.Second,
	}

	serveErr := make(chan error, len(httpLs)+len(smtpLs))
	for _, l := range httpLs {
		log.Info("HTTP API listening", "address", describe(l))
		go func() { serveErr <- httpSrv.Serve(l) }()
	}
	for _, l := range smtpLs {
		log.Info("SMTP listening", "address", describe(l))
		go func() { serveErr <- smtpSrv.Serve(l) }()
	}
	srv.SetReady(true)

	ctx, stop := signal.NotifyContext(c.ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	status := "Accepting mail on " + strings.Join(append(names(httpLs, ""), names(smtpLs, "smtp ")...), ", ")
	if cfg.Blackhole {
		status += " (BLACKHOLE MODE)"
	}
	notify(log, "READY=1\nSTATUS="+status)
	if wd := systemd.WatchdogInterval(); wd > 0 {
		go watchdog(ctx, wd/2)
	}
	log.Info("postmarkd started", "version", version, "blackhole", cfg.Blackhole, "max_concurrency", cfg.MaxConcurrency,
		"queue_size", cfg.QueueSize, "workers", cfg.Workers, "auth", len(cfg.AuthTokens) > 0)

	exit := 0
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serveErr:
		log.Error("server failed", "error", err)
		exit = 1
	}
	stop()
	notify(log, "STOPPING=1\nSTATUS=Draining")

	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	srv.SetReady(false)
	if err := httpSrv.Shutdown(sctx); err != nil {
		log.Warn("http shutdown incomplete", "error", err)
	}
	if err := smtpSrv.Shutdown(sctx); err != nil {
		log.Warn("smtp shutdown incomplete", "error", err)
	}
	if err := srv.Shutdown(sctx); err != nil {
		log.Error("async queue not fully drained", "error", err)
		exit = 1
	}
	log.Info("stopped")
	return exit
}

// listen returns the HTTP and SMTP listeners. Sockets passed by systemd are
// used when present: those whose FileDescriptorName contains "smtp" serve
// SMTP, the rest HTTP. A protocol with no passed sockets binds its
// configured addresses instead.
func listen(cfg config.Config) (httpLs, smtpLs []net.Listener, err error) {
	activated, err := systemd.Listeners()
	if err != nil {
		return nil, nil, err
	}
	for _, l := range activated {
		if strings.Contains(strings.ToLower(l.Name), "smtp") {
			smtpLs = append(smtpLs, l.Listener)
		} else {
			httpLs = append(httpLs, l.Listener)
		}
	}
	closeAll := func() {
		for _, l := range append(httpLs, smtpLs...) {
			_ = l.Close()
		}
	}
	if len(httpLs) == 0 {
		for _, a := range config.ParseAddrs(cfg.Listen) {
			l, err := bind(a, cfg.SocketMode)
			if err != nil {
				closeAll()
				return nil, nil, err
			}
			httpLs = append(httpLs, l)
		}
	}
	if len(smtpLs) == 0 {
		for _, a := range config.ParseAddrs(cfg.SMTPListen) {
			l, err := bind(a, cfg.SocketMode)
			if err != nil {
				closeAll()
				return nil, nil, err
			}
			smtpLs = append(smtpLs, l)
		}
	}
	return httpLs, smtpLs, nil
}

// bind listens on a TCP address or a unix socket, replacing a stale socket
// file and setting the socket's permissions to mode.
func bind(a config.Addr, mode fs.FileMode) (net.Listener, error) {
	if a.Network == "tcp" {
		return net.Listen("tcp", a.Address)
	}
	if err := removeStaleSocket(a.Address); err != nil {
		return nil, err
	}
	l, err := net.Listen("unix", a.Address)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(a.Address, mode); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

// removeStaleSocket deletes a socket file left behind by an unclean exit,
// refusing if another process is still accepting connections on it.
func removeStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		_ = c.Close()
		return fmt.Errorf("%s is in use by another process", path)
	}
	return os.Remove(path)
}

// watchdog pings the systemd watchdog every interval until ctx ends.
func watchdog(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = systemd.Notify("WATCHDOG=1") // a missed ping restarts us, which is the point
		}
	}
}

// notify sends state to systemd, logging rather than failing on errors.
func notify(log *slog.Logger, state string) {
	if _, err := systemd.Notify(state); err != nil {
		log.Warn("sd_notify failed", "error", err)
	}
}

// newLogger returns a logger writing to w in the configured format and
// level, leaving out timestamps under journald, which adds its own.
func newLogger(cfg config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	// journald already timestamps every line.
	if os.Getenv("JOURNAL_STREAM") != "" {
		opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}
	}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// describe names a listener as network:address for logs.
func describe(l net.Listener) string {
	return l.Addr().Network() + ":" + l.Addr().String()
}

// names lists the listeners' addresses, each with prefix, for the systemd
// status line.
func names(ls []net.Listener, prefix string) []string {
	s := make([]string, len(ls))
	for i, l := range ls {
		s[i] = prefix + l.Addr().String()
	}
	return s
}

// indent indents every line of s by two spaces.
func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
