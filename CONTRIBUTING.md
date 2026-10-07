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
cmd/postmarkd/       main: flags, listeners, signals, lifecycle
internal/config/     configuration loading and validation
internal/server/     HTTP API, validation, auth, async dispatch
internal/postmark/   Postmark HTTP client (pooling, concurrency limit, retries)
internal/queue/      bounded in-memory work queue
internal/metrics/    Prometheus text-format metrics
internal/systemd/    sd_notify, socket activation, watchdog
deploy/              systemd units, sysusers entry, example config
docs/                user documentation
```

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
