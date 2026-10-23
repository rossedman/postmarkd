# Contributing to postmarkd

Thanks for your interest! Bug reports, docs fixes and pull requests are all welcome.
Everyone taking part is expected to follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## Development

Requirements: Go 1.27 or newer, and `make`.

```sh
make            # vet, test, build
make check      # everything CI checks: lint, coverage gate, doccheck, govulncheck, fuzz, benchmarks
make race       # tests with the race detector
make cover      # race tests plus the 90% coverage gate
make lint       # vet, gofmt and golangci-lint
make fuzz       # every fuzz target for FUZZTIME (default 10s; CI uses 30s)
make bench      # benchmarks, in benchstat format
```

`make check` needs [golangci-lint](https://golangci-lint.run/) and
[GoReleaser](https://goreleaser.com/) installed.

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
tools/               repo checks: doccheck (doc comments), generated (coverage filter)
```

Run with no Postmark account at all:

```sh
POSTMARKD_BLACKHOLE=true POSTMARKD_SMTP_LISTEN=127.0.0.1:2525 go run ./cmd/postmarkd
```

## Documentation and the website

The docs in `docs/` are plain Markdown that reads well on GitHub, and they
are also the source of the website at
[rossedman.io/postmarkd](https://rossedman.io/postmarkd/).
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

## Quality bars

CI enforces these, and `make check` runs the same gates locally:

- **Coverage:** at least 90% of statements in hand-written code (generated
  files and `examples/` are excluded), measured with the race detector on.
- **Doc comments:** every function and method, exported or not, and every
  package has a doc comment (`go run ./tools/doccheck ./...`).
- **Lint and vulnerabilities:** `golangci-lint run` and `govulncheck ./...`
  are clean.
- **Benchmarks and fuzzing:** every critical path below has a benchmark, and
  every one that handles outside input has a fuzz target. CI smoke-runs the
  benchmarks and runs each fuzz target for 30 seconds. Commit any failing
  input the fuzzer finds under the package's `testdata/fuzz/` along with the
  fix.

### Critical paths

Code that parses outside input, transforms messages, sits on the per-message
hot path or resolves configuration. Changes here need a benchmark result
(`make bench`, compared with `benchstat`) in the pull request.

| Path | Why | Benchmark | Fuzz target |
| --- | --- | --- | --- |
| `internal/server` request decoding and validation (`message.go`) | parses every API request body | `internal/server/bench_test.go` | `internal/server` |
| `internal/server` send path (auth, forward, async dispatch) | per-message hot path | `internal/server/bench_test.go` | — |
| `internal/rfc822.Convert` | parses MIME from SMTP and sendmail | `internal/rfc822/bench_test.go` | `internal/rfc822` |
| `internal/smtpd` session, `DATA` and `AUTH` | parses SMTP from the network | `internal/smtpd/bench_test.go` | `internal/smtpd` |
| `internal/postmark.Client.Post` | every upstream request, retries | `internal/postmark/bench_test.go` | — |
| `internal/queue` | async hand-off between handlers and workers | `internal/queue/bench_test.go` | — |
| `internal/metrics` | updated on every request, scraped often | `internal/metrics/bench_test.go` | — |
| `internal/config.Load` / `ParseFile` | resolves file and environment settings | `internal/config/bench_test.go` | `internal/config` |
| `internal/domaincheck` record parsing | parses DNS TXT records | — | `internal/domaincheck` |
| `cmd/postmarkd` sendmail `readMessage` | reads messages from any local program | `cmd/postmarkd/fuzz_test.go` | `cmd/postmarkd` |
- Update `docs/` and `CHANGELOG.md` for user-visible changes, including any
  new `POSTMARKD_*` option (register it in `internal/config`, document it in
  `docs/configuration.md` and `deploy/postmarkd.conf`).
- Never retry a request to Postmark unless it provably was not accepted;
  duplicate email is worse than a reported failure.

## Releases

1. Add a `## [X.Y.Z] - YYYY-MM-DD` section to `CHANGELOG.md` (it becomes the
   release notes), update the version in the install commands in
   `docs/getting-started.md`, `docs/deployment.md` and `README.md`, and merge.
2. Either push a tag (`git tag -a vX.Y.Z -m vX.Y.Z && git push origin vX.Y.Z`)
   or run the **Release** workflow from the Actions tab with the version,
   which tags the branch head for you.

The workflow builds binaries and `.deb`/`.rpm` packages for amd64 and arm64
with GoReleaser, publishes the GitHub release, and pushes the multi-arch
image to `ghcr.io/rossedman/postmarkd` tagged `X.Y.Z`, `X.Y` and `latest`.
