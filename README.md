# postmarkd

[![CI](https://github.com/rossedman/postmarkd/actions/workflows/ci.yml/badge.svg)](https://github.com/rossedman/postmarkd/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**Sending mail should be one line.** postmarkd is a small daemon that runs next
to your apps (as a systemd service or a container sidecar) and delivers mail
through [Postmark](https://postmarkapp.com). Apps hand it mail however they
like: HTTP, SMTP, the `sendmail` command or a CLI. postmarkd holds the
credentials, fills in defaults, sends in parallel and retries safely.

```sh
echo "The nightly backup finished." | postmarkd send -to ops@example.com -subject "Backup OK"
```

## Ways to send

| From | How |
| --- | --- |
| **Shell scripts** | `postmarkd send -to … -subject …` (body from stdin or `-text`/`-html`, `-attach` files) |
| **Anything that uses `sendmail`**: cron, `mail`/`mailx`, PHP `mail()`, git, logwatch, smartd | Symlink postmarkd as `/usr/sbin/sendmail`. No other change needed. |
| **Apps that speak SMTP**: Rails, Django, Laravel, WordPress, Grafana, Gitea… | Set `POSTMARKD_SMTP_LISTEN=127.0.0.1:2525` (on by default in the container) and point them at it. No username, password or TLS needed. |
| **Apps with a Postmark SDK** | Change the SDK's base URL to postmarkd |
| **Any HTTP client** | `curl --unix-socket /run/postmarkd/postmarkd.sock http://localhost/email -d '{…}'` |

All of them end up in the same place: validated, defaulted, rate-limited and
sent to Postmark over pooled HTTPS connections.

```
 shell / cron / PHP ──sendmail──┐
 Rails / Django ─────SMTP :2525─┤
 Postmark SDK ───HTTP (socket)──┼──▶ postmarkd ──HTTPS──▶ Postmark ──▶ inbox
 curl ───────────HTTP :8025─────┘      │
                                       ├─ defaults (From, stream), validation
                                       ├─ parallel sends, safe retries
                                       ├─ optional async queue
                                       └─ blackhole mode for tests
```

## Features

- **Send mail** over HTTP (Postmark-compatible API), SMTP, `sendmail` or the CLI.
- **Lots of mail, in parallel.** Batches of up to 500 messages per request,
  up to `POSTMARKD_MAX_CONCURRENCY` requests to Postmark at once over pooled
  keep-alive connections, and an optional bounded async queue with back-pressure.
- **Message streams.** Every message can pick its stream (`MessageStream`, the
  `X-PM-Message-Stream` header, `-stream`, or `?stream=` per request), so
  transactional and broadcast mail flow side by side, even in one batch.
- **Black hole for testing.** `POSTMARKD_BLACKHOLE=true` accepts everything,
  answers exactly like Postmark, sends nothing, and lets your tests read back
  what was "sent" at `/blackhole/messages`.
- **DKIM, SPF and DMARC check.** `postmarkd check-domain example.com` tells
  you which DNS records are missing so your mail doesn't land in spam.
- **systemd native:** unix socket, socket activation (even port 25 without
  root), `Type=notify`, watchdog, graceful drain, hardened unit.
- **Container ready:** a 16 MB non-root image with built-in health check,
  plus Compose, Kubernetes sidecar and CI examples.
- **Observable:** Prometheus `/metrics`, `/healthz`, `/readyz`.
- **No dependencies:** one static Go binary, standard library only.

## Quick start

### On a server (systemd)

```sh
git clone https://github.com/rossedman/postmarkd && cd postmarkd
sudo make install                        # binary, units, user, /etc/postmarkd/postmarkd.conf
sudo install -m 0600 /dev/stdin /etc/postmarkd/server-token <<< 'your-postmark-server-token'
sudoedit /etc/postmarkd/postmarkd.conf   # set POSTMARKD_DEFAULT_FROM=noreply@yourdomain.com
sudo systemctl enable --now postmarkd

sudo usermod -aG postmarkd "$USER"       # let your user use the socket (log in again)
echo "Hello from $(hostname)" | postmarkd send -to you@example.com -subject "It works"
```

Make every program on the host send through Postmark:

```sh
sudo make install-sendmail               # /usr/sbin/sendmail -> postmarkd
echo "Subject: test" | sendmail you@example.com
```

Release packages (`.deb`, `.rpm`, tarballs) are on the
[releases page](https://github.com/rossedman/postmarkd/releases).

### In a container

```sh
docker run -d --name postmarkd -p 127.0.0.1:8025:8025 -p 127.0.0.1:2525:2525 \
  -e POSTMARKD_SERVER_TOKEN=your-postmark-server-token \
  -e POSTMARKD_DEFAULT_FROM=noreply@yourdomain.com \
  ghcr.io/rossedman/postmarkd

curl localhost:8025/email -d '{"To":"you@example.com","Subject":"Hi","TextBody":"Hello"}'
```

As a sidecar, see [Containers](docs/containers.md) and the
[examples](examples/): Docker Compose, Kubernetes native sidecar, GitHub
Actions service.

### For tests

```sh
docker run -d -p 8025:8025 -p 2525:2525 -e POSTMARKD_BLACKHOLE=true ghcr.io/rossedman/postmarkd
# … run your test suite against localhost:8025 or SMTP localhost:2525 …
curl -s localhost:8025/blackhole/messages | jq '.Messages[].Message.Subject'
```

## Before you send: make your domain trusted

Postmark signs your mail with **DKIM** once you add DNS records for your
domain; a custom **Return-Path** makes **SPF** line up with your domain; and a
**DMARC** record tells receivers what to do with mail that fails both.
Gmail and Yahoo require these for bulk senders. Check them in one command:

```sh
postmarkd check-domain yourdomain.com -account-token "$POSTMARK_ACCOUNT_TOKEN"
```

It prints each record's status and the exact record to add when one is
missing. Read [Email authentication](docs/email-authentication.md) for what
these mean.

## Documentation

- [Sending mail](docs/sending.md): CLI, sendmail, SMTP, HTTP, SDKs, streams, batches
- [Testing with the black hole](docs/testing.md)
- [Email authentication (DKIM, SPF, DMARC)](docs/email-authentication.md)
- [Containers and sidecars](docs/containers.md)
- [Configuration](docs/configuration.md): every option, secrets
- [HTTP API reference](docs/api.md)
- [Deployment and operations](docs/deployment.md): systemd, sockets, hardening, scaling, metrics

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md). To report a
security issue, see [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE). postmarkd is not affiliated with or endorsed by Postmark or
ActiveCampaign.
