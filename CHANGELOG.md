# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Postmark-compatible API: `/email`, `/email/batch`, `/email/withTemplate`,
  `/email/batchWithTemplates`.
- TCP and unix socket listeners, systemd socket activation, `sd_notify`
  readiness and watchdog.
- Default `From` and `MessageStream`, request validation.
- Optional client token authentication.
- Upstream concurrency limit, connection pooling and safe retries.
- Optional async mode with a bounded in-memory queue and graceful drain.
- Prometheus metrics, `/healthz`, `/readyz`.
- Hardened systemd units, sysusers entry, example config, deb/rpm packaging.
