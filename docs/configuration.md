# Configuration

postmarkd is configured entirely with `POSTMARKD_*` settings. They can come
from three places, lowest precedence first:

1. Built-in defaults
2. A config file passed with `-config /etc/postmarkd/postmarkd.conf`
3. Environment variables

The config file uses simple `KEY=VALUE` lines, the same syntax as systemd's
`EnvironmentFile=`. Blank lines and lines starting with `#` or `;` are ignored,
a leading `export ` is allowed and values may be quoted. Unknown keys in the
file are an error, so typos are caught at startup.

Validate a configuration without starting the daemon:

```sh
postmarkd -config /etc/postmarkd/postmarkd.conf -check-config
```

## Options

### Listening

| Key | Default | Description |
| --- | --- | --- |
| `POSTMARKD_LISTEN` | `127.0.0.1:8025` | HTTP API addresses, comma separated: TCP (`host:port`, `:port`) or unix sockets (`unix:/path` or an absolute path). Ignored when systemd passes HTTP sockets. |
| `POSTMARKD_SMTP_LISTEN` | — | SMTP addresses, same forms. Empty disables SMTP. See [SMTP](sending.md#smtp). |
| `POSTMARKD_SMTP_HOSTNAME` | system hostname | Name announced in the SMTP greeting. |
| `POSTMARKD_SOCKET_MODE` | `0660` | Octal permissions for unix sockets created by postmarkd. |

With socket activation, sockets whose `FileDescriptorName=` contains `smtp`
serve SMTP and all others serve HTTP (see `postmarkd-smtp.socket`).

When postmarkd creates a unix socket it removes a stale socket file left by an
unclean exit, but refuses to start if another process is still listening on it.
The socket is removed on shutdown.

### Postmark

| Key | Default | Description |
| --- | --- | --- |
| `POSTMARKD_SERVER_TOKEN` | — | Postmark **server** API token. Required (directly, via file or via credential). |
| `POSTMARKD_SERVER_TOKEN_FILE` | — | Read the server token from this file. |
| `POSTMARKD_API_URL` | `https://api.postmarkapp.com` | Postmark API base URL. Useful for testing against a mock. |
| `POSTMARKD_DEFAULT_FROM` | — | `From` used when a message has none. |
| `POSTMARKD_DEFAULT_MESSAGE_STREAM` | — | `MessageStream` used when a message has none. Postmark itself defaults to `outbound`. |

### Testing

| Key | Default | Description |
| --- | --- | --- |
| `POSTMARKD_BLACKHOLE` | `false` | Accept mail but never send it; no server token needed. See [Testing](testing.md). |
| `POSTMARKD_BLACKHOLE_KEEP` | `1000` | Messages kept for `GET /blackhole/messages`. |

### Client authentication

| Key | Default | Description |
| --- | --- | --- |
| `POSTMARKD_AUTH_TOKENS` | — | Comma or whitespace separated list of tokens clients must present. Empty disables authentication. |
| `POSTMARKD_AUTH_TOKENS_FILE` | — | Read tokens from a file: one or more per line, `#` starts a comment. |

Clients send a token as `Authorization: Bearer <token>` or as
`X-Postmark-Server-Token: <token>` (so Postmark SDKs work unchanged). SMTP
clients use `AUTH PLAIN` or `AUTH LOGIN` with any username and the token as
password (not required on unix sockets). The CLI reads `$POSTMARKD_TOKEN`.
Giving each application its own token makes revocation easy.

Leaving authentication off is reasonable for a unix socket, where file
permissions control access. **Always enable it when listening on TCP** on
anything other than loopback.

### Performance

| Key | Default | Description |
| --- | --- | --- |
| `POSTMARKD_MAX_CONCURRENCY` | `128` | Maximum simultaneous requests to Postmark. Also the size of the upstream connection pool. Requests beyond this wait for a free slot. |
| `POSTMARKD_WORKERS` | `32` | Goroutines delivering async (queued) requests. |
| `POSTMARKD_QUEUE_SIZE` | `10000` | Capacity of the async queue, in requests. `0` disables async sending. |
| `POSTMARKD_MAX_RETRIES` | `3` | Retries for failures that are safe to retry (see [API](api.md#retries)). |
| `POSTMARKD_UPSTREAM_TIMEOUT` | `30s` | Timeout for one request attempt to Postmark. |
| `POSTMARKD_MAX_BODY_BYTES` | `52428800` | Maximum request body (50 MiB, Postmark's batch limit). |
| `POSTMARKD_SHUTDOWN_TIMEOUT` | `30s` | Time allowed on stop to finish in-flight requests and drain the queue. |

See [Deployment → Scaling](deployment.md#scaling-and-tuning) for guidance.

### Logging

| Key | Default | Description |
| --- | --- | --- |
| `POSTMARKD_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. `debug` logs every request. |
| `POSTMARKD_LOG_FORMAT` | `text` | `text` (logfmt) or `json`. |

Logs go to stderr. Under systemd (journald) timestamps are omitted because the
journal records them.

## Secrets

Avoid putting tokens in the config file or environment where possible. In
order of preference:

1. **systemd credentials.** The shipped unit loads
   `/etc/postmarkd/server-token` with `LoadCredential=`. postmarkd reads
   `$CREDENTIALS_DIRECTORY/postmark-server-token` and, if present,
   `$CREDENTIALS_DIRECTORY/auth-tokens`. The files can stay `root:root 0600`;
   systemd passes them to the service privately. Encrypted credentials
   (`LoadCredentialEncrypted=`, `systemd-creds`) work the same way.
2. **`*_FILE` options** pointing to files readable by the `postmarkd` user.
3. **Plain values** in the config file (`0640 root:postmarkd`) or environment.

An explicit value wins over a `*_FILE` option, which wins over a credential.

## Example

```ini
# /etc/postmarkd/postmarkd.conf
POSTMARKD_LISTEN=unix:/run/postmarkd/postmarkd.sock
POSTMARKD_DEFAULT_FROM=noreply@example.com
POSTMARKD_DEFAULT_MESSAGE_STREAM=outbound
POSTMARKD_MAX_CONCURRENCY=256
POSTMARKD_LOG_FORMAT=json
```

## Command-line flags

`postmarkd` (or `postmarkd serve`) runs the daemon:

| Flag | Description |
| --- | --- |
| `-config PATH` | Load a config file. |
| `-check-config` | Validate configuration and exit (status 2 if invalid). |
| `-version` | Print the version and exit. |

Client commands (`send`, `sendmail`, `healthcheck`) are configured with
`POSTMARKD_ADDR`, `POSTMARKD_TOKEN` and `POSTMARKD_STREAM`; see
[Sending mail](sending.md#where-the-daemon-is).
