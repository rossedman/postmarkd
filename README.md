# postmarkd

[![CI](https://github.com/rossedman/postmarkd/actions/workflows/ci.yml/badge.svg)](https://github.com/rossedman/postmarkd/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**postmarkd** is a small, fast systemd daemon that gives every application on a
host a local mail API, and delivers the mail through [Postmark](https://postmarkapp.com).

Applications talk to a unix socket (or a TCP port). postmarkd holds the Postmark
credentials, applies defaults, limits and retries upstream calls, and can
queue mail so callers never wait on the network.

```sh
curl --unix-socket /run/postmarkd/postmarkd.sock http://localhost/email \
  -d '{"To": "ada@example.com", "Subject": "Hello", "TextBody": "Sent via postmarkd"}'
```

## Features

- **Postmark-compatible API.** Serves `/email`, `/email/batch`,
  `/email/withTemplate` and `/email/batchWithTemplates` with Postmark's request
  and response formats. Existing Postmark SDKs work by changing their base URL.
- **Unix socket or TCP**, including **systemd socket activation**.
- **One place for the secret.** Apps never see the Postmark server token;
  it can be loaded with systemd `LoadCredential=`.
- **Built for volume.** Pooled keep-alive connections to Postmark, a
  configurable concurrency cap, safe retries with backoff, and an optional
  bounded async queue with back-pressure.
- **Proper systemd citizen.** `Type=notify` readiness, watchdog, graceful
  drain on stop, journald-friendly logs and a hardened unit file.
- **Observable.** Prometheus `/metrics`, `/healthz` and `/readyz`.
- **No dependencies.** A single static Go binary using only the standard library.

## Quick start

### Install

From a [release](https://github.com/rossedman/postmarkd/releases) (`.deb`,
`.rpm` or tarball), or from source with Go 1.23+:

```sh
git clone https://github.com/rossedman/postmarkd && cd postmarkd
sudo make install
```

This installs `/usr/bin/postmarkd`, the systemd units, a `postmarkd` system
user and a commented config file at `/etc/postmarkd/postmarkd.conf`.

### Configure

```sh
# Your Postmark *server* token (Servers → API Tokens in Postmark).
sudo install -m 0600 /dev/stdin /etc/postmarkd/server-token <<< 'your-server-token'

# Optional: set a default sender in /etc/postmarkd/postmarkd.conf
#   POSTMARKD_DEFAULT_FROM=noreply@example.com
```

### Run

```sh
sudo systemctl enable --now postmarkd
# or start on demand via socket activation:
sudo systemctl enable --now postmarkd.socket
```

Allow a local user (for example your web app) to use the socket:

```sh
sudo usermod -aG postmarkd www-data
```

### Send

```sh
curl --unix-socket /run/postmarkd/postmarkd.sock http://localhost/email \
  -H 'Content-Type: application/json' \
  -d '{
        "From": "noreply@example.com",
        "To": "ada@example.com",
        "Subject": "Welcome!",
        "HtmlBody": "<p>Thanks for signing up.</p>",
        "TextBody": "Thanks for signing up.",
        "MessageStream": "outbound"
      }'
```

Don't wait for Postmark; queue it and return immediately:

```sh
curl --unix-socket /run/postmarkd/postmarkd.sock 'http://localhost/email?async=true' \
  -d '{"To": "ada@example.com", "Subject": "Hi", "TextBody": "Queued"}'
# {"ErrorCode":0,"Message":"Queued","Messages":1,"RequestID":"…"}
```

### Run without systemd

```sh
POSTMARKD_SERVER_TOKEN=your-server-token \
POSTMARKD_LISTEN=127.0.0.1:8025 \
POSTMARKD_DEFAULT_FROM=noreply@example.com \
  postmarkd
```

For local development you can use Postmark's test token `POSTMARK_API_TEST`,
which accepts mail without delivering it.

## Using a Postmark SDK

Point the SDK at postmarkd and give it any value as the server token (or one of
your `POSTMARKD_AUTH_TOKENS` if you enabled client authentication):

```js
// Node.js (postmark.js)
const postmark = require("postmark");
const client = new postmark.ServerClient("client-token", {
  requestHost: "127.0.0.1:8025",
  useHttps: false,
});
```

```go
// Go: any HTTP client works; over a unix socket:
tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
    return (&net.Dialer{}).DialContext(ctx, "unix", "/run/postmarkd/postmarkd.sock")
}}
client := &http.Client{Transport: tr}
resp, err := client.Post("http://postmarkd/email", "application/json", body)
```

## Documentation

- [Configuration](docs/configuration.md): every option, secrets, precedence
- [API reference](docs/api.md): endpoints, async mode, errors
- [Deployment & operations](docs/deployment.md): systemd, sockets, hardening, scaling, metrics

## How it works

```
 app ──HTTP──▶ unix socket / TCP ──▶ postmarkd ──HTTPS (pooled)──▶ api.postmarkapp.com
                                       │
                                       ├─ auth, validation, defaults
                                       ├─ concurrency cap + retries
                                       └─ optional in-memory queue + workers
```

Synchronous requests are proxied to Postmark and Postmark's response is
returned unchanged. Async requests are validated, placed on a bounded queue and
acknowledged with `202 Accepted`; a pool of workers delivers them.

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md). To report a
security issue, see [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE). postmarkd is not affiliated with or endorsed by Postmark or
ActiveCampaign.
