# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-10-13

The first release: a small daemon that lets anything on a host or in a pod
send mail through Postmark.

### Sending

- Postmark-compatible HTTP API: `/email`, `/email/batch`,
  `/email/withTemplate` and `/email/batchWithTemplates`. Existing Postmark
  SDKs work by changing their base URL.
- SMTP server (`POSTMARKD_SMTP_LISTEN`) that converts MIME mail, including
  attachments, inline images and Postmark's `X-PM-*` headers.
- `sendmail` compatibility: invoke the binary as `sendmail` and cron,
  `mail`, PHP `mail()` and friends deliver through Postmark.
- `postmarkd send` for scripts: bodies from stdin or files, attachments,
  templates, streams, tags and metadata.
- Default `From` and `MessageStream`, plus per-request stream selection
  (`?stream=`, `X-Postmarkd-Stream`).

### Reliability and scale

- Validation before Postmark sees a message; original bytes are forwarded
  untouched.
- Pooled upstream connections, a concurrency limit, and retries only when
  Postmark cannot have accepted the message (no duplicate mail).
- Optional async mode with a bounded in-memory queue, back-pressure and
  graceful drain on shutdown.

### Operations

- TCP and unix socket listeners (several at once), systemd socket
  activation (including SMTP on port 25 via `postmarkd-smtp.socket`),
  `sd_notify` readiness and watchdog, and a hardened unit.
- Optional client token authentication; the Postmark token can be loaded as
  a systemd credential.
- Prometheus metrics, `/healthz` and `/readyz`; `postmarkd healthcheck`.
- `postmarkd check-domain` checks DKIM, Return-Path (SPF alignment) and
  DMARC records.
- Blackhole mode (`POSTMARKD_BLACKHOLE`) for tests, with captured mail at
  `/blackhole/messages`.

### Distribution

- Linux binaries, `.deb` and `.rpm` packages for amd64 and arm64, and a
  multi-arch distroless container image at `ghcr.io/rossedman/postmarkd`.
- Docker Compose, Kubernetes sidecar and GitHub Actions examples.
- Documentation site at https://rossedman.io/postmarkd/.
- Benchmarks for every component, run in CI with a regression gate.

[Unreleased]: https://github.com/rossedman/postmarkd/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/rossedman/postmarkd/releases/tag/v0.1.0
