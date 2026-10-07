// Command postmarkd is a small daemon exposing a Postmark-compatible HTTP
// mail API on a TCP port or unix socket, relaying mail through Postmark.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rossedman/postmarkd/internal/config"
	"github.com/rossedman/postmarkd/internal/metrics"
	"github.com/rossedman/postmarkd/internal/postmark"
	"github.com/rossedman/postmarkd/internal/server"
	"github.com/rossedman/postmarkd/internal/systemd"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "", "path to a KEY=VALUE config file (environment variables override it)")
	check := flag.Bool("check-config", false, "validate the configuration and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: postmarkd [flags]\n\npostmarkd relays mail sent to its HTTP API through Postmark.\nAll settings are POSTMARKD_* environment variables; see docs/configuration.md.\n\nFlags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("postmarkd", version)
		return 0
	}

	cfg, err := config.Load(*configPath, os.Environ())
	if err != nil {
		fmt.Fprintf(os.Stderr, "postmarkd: invalid configuration:\n%s\n", indent(err.Error()))
		return 2
	}
	if *check {
		fmt.Println("configuration OK")
		return 0
	}
	log := newLogger(cfg)

	listeners, err := listen(cfg)
	if err != nil {
		log.Error("cannot listen", "error", err)
		return 1
	}

	m := metrics.New()
	client := postmark.New(postmark.Options{
		BaseURL:        cfg.APIURL,
		ServerToken:    cfg.ServerToken,
		MaxConcurrency: cfg.MaxConcurrency,
		MaxRetries:     cfg.MaxRetries,
		Timeout:        cfg.UpstreamTimeout,
		UserAgent:      "postmarkd/" + version,
	})
	m.Gauge("postmarkd_upstream_in_flight", "Requests to Postmark currently in flight.", func() float64 { return float64(client.InFlight()) })
	srv := server.New(cfg, client, m, log)

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

	serveErr := make(chan error, len(listeners))
	for _, l := range listeners {
		log.Info("listening", "network", l.Addr().Network(), "address", l.Addr().String())
		go func() { serveErr <- httpSrv.Serve(l) }()
	}
	srv.SetReady(true)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	notify(log, fmt.Sprintf("READY=1\nSTATUS=Accepting mail on %s", addrs(listeners)))
	if wd := systemd.WatchdogInterval(); wd > 0 {
		go watchdog(ctx, wd/2)
	}
	log.Info("postmarkd started", "version", version, "max_concurrency", cfg.MaxConcurrency,
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
	if err := srv.Shutdown(sctx); err != nil {
		log.Error("async queue not fully drained", "error", err)
		exit = 1
	}
	log.Info("stopped")
	return exit
}

// listen returns systemd-activated sockets if present, otherwise it binds
// POSTMARKD_LISTEN.
func listen(cfg config.Config) ([]net.Listener, error) {
	ls, err := systemd.Listeners()
	if err != nil || len(ls) > 0 {
		return ls, err
	}
	path, ok := cfg.UnixSocket()
	if !ok {
		l, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			return nil, err
		}
		return []net.Listener{l}, nil
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, cfg.SocketMode); err != nil {
		l.Close()
		return nil, err
	}
	return []net.Listener{l}, nil
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
		c.Close()
		return fmt.Errorf("%s is in use by another process", path)
	}
	return os.Remove(path)
}

func watchdog(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			systemd.Notify("WATCHDOG=1")
		}
	}
}

func notify(log *slog.Logger, state string) {
	if _, err := systemd.Notify(state); err != nil {
		log.Warn("sd_notify failed", "error", err)
	}
}

func newLogger(cfg config.Config) *slog.Logger {
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
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func addrs(ls []net.Listener) string {
	s := make([]string, len(ls))
	for i, l := range ls {
		s[i] = l.Addr().String()
	}
	return strings.Join(s, ", ")
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
