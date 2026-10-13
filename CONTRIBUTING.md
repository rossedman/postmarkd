# Contributing to postmarkd

Thanks for your interest! Bug reports, docs fixes and pull requests are all welcome.

## Development

Requirements: Go 1.23 or newer, and `make`.

```sh
make            # vet, test, build
make race       # tests with the race detector
make lint       # vet + gofmt check
```

Run the daemon locally against Postmark's test token, which accepts mail
without delivering it:

```sh
POSTMARKD_SERVER_TOKEN=POSTMARK_API_TEST \
POSTMARKD_LISTEN=127.0.0.1:8025 \
POSTMARKD_DEFAULT_FROM=you@example.com \
POSTMARKD_LOG_LEVEL=debug \
  go run ./cmd/postmarkd

curl localhost:8025/email -d '{"To":"test@blackhole.postmarkapp.com","Subject":"hi","TextBody":"hello"}'
```

Test socket activation without installing anything:

```sh
make build
systemd-socket-activate -l /tmp/postmarkd.sock \
  -E POSTMARKD_SERVER_TOKEN=POSTMARK_API_TEST ./bin/postmarkd
```

## Layout

```
cmd/postmarkd/       commands: serve (daemon), send, sendmail, check-domain, healthcheck
internal/config/     configuration loading and validation
internal/server/     HTTP API, SMTP delivery, validation, auth, async dispatch
internal/smtpd/      minimal ESMTP server
internal/rfc822/     MIME message -> Postmark message conversion
internal/blackhole/  fake Postmark for blackhole mode
internal/client/     client for a running daemon (used by the CLI)
internal/domaincheck/ DKIM / Return-Path / DMARC DNS checks
internal/postmark/   Postmark HTTP client (pooling, concurrency limit, retries)
internal/queue/      bounded in-memory work queue
internal/metrics/    Prometheus text-format metrics
internal/systemd/    sd_notify, socket activation, watchdog
deploy/              systemd units, sysusers entry, example config
examples/            Docker Compose, Kubernetes, CI examples
docs/                user documentation
```

Run with no Postmark account at all:

```sh
POSTMARKD_BLACKHOLE=true POSTMARKD_SMTP_LISTEN=127.0.0.1:2525 go run ./cmd/postmarkd
```

## Documentation and the website

The docs in `docs/` are plain Markdown that reads well on GitHub, and they
are also the source of the website at
[rossedman.github.io/postmarkd](https://rossedman.github.io/postmarkd/).
The site is built by a small generator in `site/` (its own Go module, so the
daemon stays dependency-free) and deployed by the Pages workflow.

```sh
make site        # build into _site/; fails on broken links or anchors
make site-serve  # build and serve at http://localhost:8000
```

To add a page, write `docs/<name>.md` (the first `#` heading is the title)
and add it to the `nav` list in `site/main.go`. Link between pages with
relative `.md` links, as you would on GitHub; the generator rewrites them.

Brand assets (logo, banners, social card) live in `assets/brand/`. Their
sources are in `assets/brand/src/`; regenerate them with `make brand`.

## Guidelines

- Keep postmarkd small. It intentionally has **no third-party dependencies**;
  please discuss before adding one.
- Add tests for behaviour changes; `go test -race ./...` must pass.
- Run `gofmt`. CI rejects unformatted code.
- Update `docs/` and `CHANGELOG.md` for user-visible changes, including any
  new `POSTMARKD_*` option (register it in `internal/config`, document it in
  `docs/configuration.md` and `deploy/postmarkd.conf`).
- Never retry a request to Postmark unless it provably was not accepted;
  duplicate email is worse than a reported failure.

## Releases

Tag `vX.Y.Z` on the default branch. The release workflow builds binaries,
`.deb` and `.rpm` packages with GoReleaser.
