# Sending mail

postmarkd accepts mail five ways. Pick whatever your program already speaks.

- [The `postmarkd send` command](#the-postmarkd-send-command)
- [`sendmail`](#sendmail): cron, `mail`, PHP and anything else that pipes into sendmail
- [SMTP](#smtp)
- [HTTP and Postmark SDKs](#http-and-postmark-sdks)
- [Message streams](#message-streams)
- [Sending a lot of mail](#sending-a-lot-of-mail)

## Where the daemon is

The client commands (`send`, `sendmail`, `healthcheck`) find the daemon via:

1. `-addr` (for `send` and `healthcheck`)
2. `$POSTMARKD_ADDR`: a socket path (`/run/postmarkd/postmarkd.sock`),
   `host:port`, or an `http(s)://` URL
3. The first address in `$POSTMARKD_LISTEN` (handy inside the container)
4. The default socket, `/run/postmarkd/postmarkd.sock`

If the daemon requires client tokens, set `$POSTMARKD_TOKEN` (or `-token`).

Using the unix socket requires permission on it: add the user to the
`postmarkd` group, or relax `SocketMode=` if every local user may send mail.

## The `postmarkd send` command

```sh
# Body from stdin
df -h | postmarkd send -to ops@example.com -subject "Disk usage on $(hostname)"

# Inline text, HTML from a file, attachments
postmarkd send -to ada@example.com -cc grace@example.com \
  -subject "Monthly report" -text "Report attached." -html @report.html \
  -attach report.pdf -attach data.csv

# Postmark template
postmarkd send -to ada@example.com -template welcome -model '{"name": "Ada"}'

# Pick a stream, tag and metadata; queue instead of waiting
postmarkd send -to ada@example.com -subject Hi -text Hi \
  -stream broadcast -tag newsletter -metadata campaign=oct -async
```

It prints the Postmark `MessageID` (or `queued <request id>` with `-async`)
and exits non-zero with Postmark's error message on failure. Run
`postmarkd send -h` for all flags. `@file` reads a value from a file and `-`
from stdin.

## sendmail

Most Unix software sends mail by piping a message into `/usr/sbin/sendmail`.
When the postmarkd binary is invoked under the name `sendmail` it behaves like
sendmail, so:

```sh
sudo ln -s /usr/bin/postmarkd /usr/sbin/sendmail   # or: sudo make install-sendmail
```

and then, with no further configuration:

```sh
printf 'Subject: Hello\n\nSent with sendmail.\n' | sendmail ada@example.com
echo "body" | mail -s "Hello" ada@example.com      # mailx / bsd-mailx
# cron mails job output to MAILTO automatically
# PHP's mail() calls "sendmail -t -i"
```

Don't do this on a host that already runs Postfix, Exim or similar; they own
`/usr/sbin/sendmail`. You can still call `postmarkd sendmail` explicitly.

Supported options:

| Option | Meaning |
| --- | --- |
| `addr…` | Recipients. |
| `-t` | Read recipients from the `To`, `Cc` and `Bcc` headers (plus any given as arguments). |
| `-i`, `-oi` | A line with a single `.` does not end the message. |
| `-f addr`, `-r addr` | Sender, used as `From` when the message has no `From:` header. |
| `-F name` | Display name for that sender. |
| `-o…`, `-B`, `-N`, `-R`, `-V`, `-v`, `-bm`, … | Accepted and ignored. |
| `-bs`, `-bp`, `-q` | Not supported (postmarkd has no local queue). |

Environment: `POSTMARKD_ADDR`, `POSTMARKD_TOKEN`, and `POSTMARKD_STREAM` (the
default message stream for these messages).

Exit codes follow `sysexits.h`: `0` sent, `65` the message was rejected,
`75` temporary failure (daemon or Postmark unreachable; try again), `77`
not authorized.

If the message has no `From:` and no `-f`, the daemon's
`POSTMARKD_DEFAULT_FROM` is used. Postmark only sends from verified senders,
so set that default when using cron or other system mail.

## SMTP

Enable the SMTP listener:

```ini
POSTMARKD_SMTP_LISTEN=127.0.0.1:2525
```

(The container image listens on `:2525` by default.) Then configure your app:

| Setting | Value |
| --- | --- |
| Host / port | `127.0.0.1` / `2525` (or the sidecar's address) |
| Encryption | none (no STARTTLS) |
| Username / password | none, unless client tokens are configured (then any username, token as password) |

Examples:

```ruby
# Rails: config/environments/production.rb
config.action_mailer.delivery_method = :smtp
config.action_mailer.smtp_settings = { address: "127.0.0.1", port: 2525 }
```

```python
# Django: settings.py
EMAIL_HOST, EMAIL_PORT = "127.0.0.1", 2525
```

```sh
# Laravel: .env
MAIL_MAILER=smtp
MAIL_HOST=127.0.0.1
MAIL_PORT=2525
```

postmarkd converts each message (text, HTML, attachments, inline images,
`Reply-To`, threading headers) into a Postmark API call and answers the SMTP
client according to Postmark's result: `250` when accepted, `451` (try again
later) when Postmark is unreachable or rate limiting, `554` when the message
was rejected. Postmark's SMTP headers work too:

| Header | Effect |
| --- | --- |
| `X-PM-Message-Stream: broadcast` | Send on that stream. |
| `X-PM-Tag: welcome` | Postmark tag. |
| `X-PM-Metadata-<key>: value` | Postmark metadata. |

Up to 50 recipients are accepted per message (Postmark's limit); SMTP
clients send the rest in a further transaction automatically.

**Port 25.** To be a drop-in local relay on port 25 without running
postmarkd as root, use systemd socket activation: enable
`postmarkd-smtp.socket` (see the comments in that file).

**Security.** SMTP has no TLS here. Use it on loopback, a unix socket, or a
private container network. For remote clients use the HTTP API with client
tokens behind TLS, or Postmark's own SMTP service.

## HTTP and Postmark SDKs

The HTTP API is Postmark's own (`/email`, `/email/batch`,
`/email/withTemplate`, `/email/batchWithTemplates`), so existing code and
SDKs work by changing the base URL. postmarkd adds the real server token. See
the [API reference](api.md).

```sh
curl --unix-socket /run/postmarkd/postmarkd.sock http://localhost/email \
  -d '{"From":"noreply@example.com","To":"ada@example.com","Subject":"Hi","TextBody":"Hello"}'
```

```js
// Node.js (postmark.js)
const postmark = require("postmark");
const client = new postmark.ServerClient("unused-or-client-token", {
  requestHost: "127.0.0.1:8025",
  useHttps: false,
});
await client.sendEmail({ To: "ada@example.com", Subject: "Hi", TextBody: "Hello" });
```

```go
// Go, over the unix socket
tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", "/run/postmarkd/postmarkd.sock")
}}
client := &http.Client{Transport: tr}
resp, err := client.Post("http://postmarkd/email", "application/json", body)
```

## Message streams

Postmark separates **transactional** mail (receipts, password resets: the
default `outbound` stream) from **broadcast** mail (newsletters,
announcements), so that bulk sends never hurt the delivery of important mail.
You can create more streams in Postmark, for example one per product or per
kind of notification.

postmarkd chooses a message's stream, first match wins:

1. The message's own `MessageStream` (or `X-PM-Message-Stream` header over
   SMTP/sendmail, or `-stream` for `postmarkd send`)
2. The request's `X-Postmarkd-Stream` header or `?stream=` parameter
3. `POSTMARKD_DEFAULT_MESSAGE_STREAM`
4. Postmark's default, `outbound`

Because the stream is per message, one batch can mix streams:

```sh
curl --unix-socket /run/postmarkd/postmarkd.sock http://localhost/email/batch -d '[
  {"To":"ada@example.com",   "Subject":"Your receipt", "TextBody":"…", "MessageStream":"outbound"},
  {"To":"grace@example.com", "Subject":"October news", "HtmlBody":"…", "MessageStream":"broadcast"}
]'
```

Broadcast streams require an unsubscribe link; Postmark can add it for you.

## Sending a lot of mail

- **Batch.** `/email/batch` takes up to 500 messages per request and is by
  far the fastest path. Each message gets its own result.
- **Parallelism.** postmarkd sends up to `POSTMARKD_MAX_CONCURRENCY`
  (default 128) requests to Postmark at once over reused connections; extra
  requests wait their turn instead of failing. Many clients can call
  postmarkd concurrently.
- **Async.** Add `?async=true` (or `-async`) and postmarkd replies `202`
  immediately and delivers in the background; a full queue answers `503` so
  you can back off.

```bash
# Every user in users.json, in batches of 500, 20 batches at a time
jq -c '[.[] | {To: .email, Subject: "Hello", TextBody: "Hi \(.name)"}] | _nwise(500)' users.json |
{
  while read -r batch; do
    printf '%s' "$batch" | curl -s --unix-socket /run/postmarkd/postmarkd.sock \
      http://localhost/email/batch -d @- >/dev/null &
    while [ "$(jobs -rp | wc -l)" -ge 20 ]; do wait -n; done
  done
  wait
}
```

See [Scaling and tuning](deployment.md#scaling-and-tuning).
