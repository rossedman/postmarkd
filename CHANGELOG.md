# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.1] - 2026-10-24

### Added

- Install instructions for the release packages: one-step `.deb` and `.rpm`
  installs for amd64 and arm64, a binary-only install, checksum
  verification, and upgrade and uninstall steps.
- Container docs list the image tags (`X.Y.Z`, `X.Y`, `latest`).
- The docs site build fails when an install command or image tag points at
  an older release than the latest one in this changelog.
- Release checksums and container images are signed with cosign
  (keyless), and archives, packages and images carry build provenance
  attestations. See `SECURITY.md` for verification.

### Changed

- Examples use the `0.1` image tag instead of `latest`.
- The `sendmail` symlink instructions work with the packages and no longer
  need a source checkout.
- Building from source needs Go 1.27 or newer.
- A message that gives a checked field (`From`, `To`, `Cc`, `Bcc`,
  `HtmlBody`, `TextBody`, `TemplateId`, `TemplateAlias`, `MessageStream`)
  more than once, under any capitalization, or a template batch with more
  than one `Messages`, is rejected with 422 instead of resolved
  unpredictably.
- A response from Postmark that is not valid JSON is no longer relayed with
  its own content type; the API answers 502 with a JSON error instead.
  JSON responses keep their status and are written compactly with a
  trailing newline.

### Fixed

- The container image reports its version without a `v` prefix, matching
  the binaries and packages.
- A field holding only Unicode whitespace such as a non-breaking space now
  counts as blank whether or not it is JSON-escaped, so the default `From`
  applies and an empty required field is rejected.
- Whitespace-only defaults and stream overrides (`?stream=%20`, a blank
  `X-Postmarkd-Stream`) are ignored instead of being sent as a blank
  `MessageStream`, and defaults are trimmed.

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

[Unreleased]: https://github.com/rossedman/postmarkd/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/rossedman/postmarkd/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/rossedman/postmarkd/releases/tag/v0.1.0
