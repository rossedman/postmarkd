# Deployment and operations

## Files

| Path | Purpose |
| --- | --- |
| `/usr/bin/postmarkd` | The daemon. |
| `/usr/lib/systemd/system/postmarkd.service` | Service unit (`Type=notify`, hardened). |
| `/usr/lib/systemd/system/postmarkd.socket` | Optional socket-activation unit (HTTP API). |
| `/usr/lib/systemd/system/postmarkd-smtp.socket` | Optional socket-activation unit for SMTP (e.g. port 25). |
| `/usr/lib/sysusers.d/postmarkd.conf` | Creates the `postmarkd` user and group. |
| `/etc/postmarkd/postmarkd.conf` | Configuration (`root:postmarkd 0640`). |
| `/etc/postmarkd/server-token` | Postmark server token (`root:root 0600`), loaded as a systemd credential. |
| `/run/postmarkd/postmarkd.sock` | Default API socket. |

## Installing from source

```sh
make build            # bin/postmarkd
sudo make install     # binary, units, sysusers entry, example config
```

`make install` honours `PREFIX`, `SYSCONFDIR`, `UNITDIR` and `DESTDIR`. When
`DESTDIR` is empty it also creates the `postmarkd` user and reloads systemd.

## Running under systemd

```sh
sudo install -m 0600 /dev/stdin /etc/postmarkd/server-token <<< 'your-server-token'
sudo systemctl enable --now postmarkd
systemctl status postmarkd
journalctl -u postmarkd -f
```

The unit uses `Type=notify`: systemd considers postmarkd started only once it
is listening, and `systemctl status` shows the listening addresses. A 30 s
`WatchdogSec=` restarts the daemon if it stops responding.

On `systemctl stop` (SIGTERM) postmarkd stops accepting connections, finishes
in-flight requests, drains the async queue and exits, all within
`POSTMARKD_SHUTDOWN_TIMEOUT`. Configuration changes take effect with
`systemctl restart postmarkd`.

### Socket activation

```sh
sudo systemctl enable --now postmarkd.socket
```

systemd creates `/run/postmarkd/postmarkd.sock` and starts postmarkd on the
first connection. Because systemd holds the socket, restarts and upgrades
don't refuse connections: they queue in the kernel until the new process is
ready. Add more `ListenStream=` lines (for example `127.0.0.1:8025`) to serve
several addresses. `POSTMARKD_LISTEN` is ignored when sockets are passed in.

To change socket ownership or permissions, use a drop-in:

```sh
sudo systemctl edit postmarkd.socket
```

```ini
[Socket]
SocketGroup=www-data
SocketMode=0660
```

For SMTP on port 25, enable `postmarkd-smtp.socket` and tell the service
about both sockets (`systemctl edit postmarkd.service`):

```ini
[Service]
Sockets=postmarkd.socket postmarkd-smtp.socket
```

systemd binds port 25 itself, so postmarkd still runs unprivileged.

### Granting access to the socket

The socket is `postmarkd:postmarkd 0660`. Add each client's user to the
`postmarkd` group:

```sh
sudo usermod -aG postmarkd www-data
```

### TCP

Set `POSTMARKD_LISTEN=127.0.0.1:8025` (or `ListenStream=` in the socket unit).
If clients connect from other hosts, also set `POSTMARKD_AUTH_TOKENS`, and
terminate TLS in front of postmarkd (nginx, Caddy, HAProxy, a service mesh):
postmarkd itself serves plain HTTP.

### Hardening

The shipped unit runs postmarkd as an unprivileged user with no capabilities,
a read-only file system (`ProtectSystem=strict`), private `/tmp` and devices,
a restricted system-call filter and only `AF_UNIX`/`AF_INET`/`AF_INET6`
sockets. Check the result with `systemd-analyze security postmarkd`.

If you listen only on a unix socket you can tighten further with a drop-in:

```ini
[Service]
IPAddressDeny=any
IPAddressAllow=<Postmark API addresses or your egress proxy>
```

## Scaling and tuning

postmarkd is mostly network-bound: each send costs one HTTPS round trip to
Postmark (typically 100–500 ms). Throughput is therefore roughly

```
messages/second ≈ POSTMARKD_MAX_CONCURRENCY × messages per request ÷ Postmark latency
```

With the default concurrency of 128 and 250 ms latency that is about 500
single sends per second, and far more with batches, at which point your
Postmark account's limits matter more than postmarkd. The daemon itself adds
well under a millisecond per request.

- **Use batches** (`/email/batch`, up to 500 messages) for bulk sends. It is
  by far the biggest lever.
- **Raise `POSTMARKD_MAX_CONCURRENCY`** if `postmarkd_upstream_in_flight`
  sits at the limit and Postmark latency is healthy. Requests above the limit
  wait rather than fail, so it is also your protection against overwhelming
  Postmark: if you see `429`s in `postmarkd_upstream_requests_total`, lower it.
- **Use async mode** when callers shouldn't wait on Postmark (web requests).
  Size `POSTMARKD_QUEUE_SIZE` for your largest burst. Memory use is about the
  size of the queued request bodies. Set `POSTMARKD_WORKERS` near
  `POSTMARKD_MAX_CONCURRENCY` if async is your main path.
- **Prefer the unix socket** for local clients: no TCP handshake or port
  exhaustion, and access is controlled by file permissions.
- **File descriptors:** the unit sets `LimitNOFILE=65536`; each client
  connection and each upstream connection uses one.
- **Horizontal scale:** postmarkd is stateless apart from the in-memory
  queue, so run one per host, or several behind a load balancer.

## Monitoring

Scrape `/metrics` (Prometheus text format):

| Metric | Type | Description |
| --- | --- | --- |
| `postmarkd_http_requests_total{endpoint,code}` | counter | API requests by endpoint and status. |
| `postmarkd_upstream_requests_total{endpoint,code}` | counter | Requests to Postmark by status (`error` = no response). |
| `postmarkd_upstream_request_duration_seconds{endpoint}` | histogram | Postmark latency including retries. |
| `postmarkd_upstream_in_flight` | gauge | Requests currently in flight to Postmark. |
| `postmarkd_upstream_retries_total` | counter | Retried upstream attempts. |
| `postmarkd_messages_sent_total` | counter | Messages accepted by Postmark. |
| `postmarkd_messages_failed_total` | counter | Messages rejected by Postmark or not delivered to it. |
| `postmarkd_async_enqueued_total` | counter | Requests accepted onto the queue. |
| `postmarkd_async_rejected_total` | counter | Requests refused because the queue was full. |
| `postmarkd_async_dropped_total` | counter | Queued requests dropped because shutdown timed out. |
| `postmarkd_async_queue_depth` / `_capacity` | gauge | Queue usage. |
| `postmarkd_start_time_seconds` | gauge | Process start time. |

Suggested alerts: a rising `postmarkd_messages_failed_total` rate, any
`postmarkd_async_rejected_total` or `postmarkd_async_dropped_total`, and
queue depth staying above ~80% of capacity.

With only a unix socket, scrape with a local agent, e.g. by giving the agent's
user access to the socket, or add a loopback `ListenStream=127.0.0.1:8025`.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Service fails to start | `journalctl -u postmarkd`; run `postmarkd -config /etc/postmarkd/postmarkd.conf -check-config`. |
| `Permission denied` on the socket | Is the client user in the `postmarkd` group (log out/in after `usermod`)? |
| `401` from postmarkd | The client token does not match `POSTMARKD_AUTH_TOKENS`. |
| `401`/`ErrorCode 10` from Postmark | The server token is wrong; check `/etc/postmarkd/server-token`. |
| `422` with `ErrorCode 400`-`412` | Postmark rejected the message (unverified sender, inactive recipient, …). See the `Message` field. |
| `502`, `ErrorCode 9000` | postmarkd cannot reach `api.postmarkapp.com` (DNS, firewall, proxy). `HTTPS_PROXY` is honoured. |
