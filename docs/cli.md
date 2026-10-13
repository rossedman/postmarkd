# CLI reference

One binary provides the daemon and its client commands:

```text
postmarkd [serve] [flags]        run the daemon (default)
postmarkd send [flags]           send a message from the command line
postmarkd sendmail [flags] [to…] sendmail(8)-compatible: read a message on stdin
postmarkd check-domain <domain>  check DKIM, Return-Path and DMARC DNS records
postmarkd healthcheck            exit 0 if the daemon is ready
postmarkd version                print the version
postmarkd help                   print usage
```

When the binary is invoked under the name `sendmail` (for example through a
`/usr/sbin/sendmail` symlink) it always behaves as `postmarkd sendmail`.

## How client commands find the daemon

`send`, `sendmail` and `healthcheck` connect to the daemon using, in order:

1. the `-addr` flag (`send` and `healthcheck`)
2. `$POSTMARKD_ADDR`
3. the first address in `$POSTMARKD_LISTEN`, with `0.0.0.0` and `::`
   replaced by `127.0.0.1` (useful inside the container)
4. `/run/postmarkd/postmarkd.sock`

An address can be a socket path (`/run/postmarkd/postmarkd.sock` or
`unix:/path`), `host:port`, or an `http://` / `https://` URL. If the daemon
requires client tokens, set `$POSTMARKD_TOKEN`.

## postmarkd serve

Runs the daemon. `serve` is optional: `postmarkd` with no command, or with
only flags, does the same.

| Flag | Description |
| --- | --- |
| `-config PATH` | Load a `KEY=VALUE` config file. Environment variables override it. |
| `-check-config` | Validate the configuration and exit. |
| `-version` | Print the version and exit. |

All other settings are `POSTMARKD_*` variables; see [Configuration](configuration.md).

Exit status: `0` after a clean shutdown, `1` on a runtime error (for example
a listen failure, or the async queue not draining in time), `2` on invalid
configuration.

## postmarkd send

Sends one message through a running daemon and prints the Postmark
`MessageID` (or `queued <request id>` with `-async`).

```sh
echo "Backup finished" | postmarkd send -to ops@example.com -subject "Backup OK"
postmarkd send -to ada@example.com -subject Report -html @report.html -attach report.pdf
postmarkd send -to ada@example.com -template welcome -model '{"name":"Ada"}'
```

| Flag | Description |
| --- | --- |
| `-to ADDR` | Recipient. Repeat the flag or separate with commas. |
| `-cc ADDR` | Cc recipient. Repeatable. |
| `-bcc ADDR` | Bcc recipient. Repeatable. |
| `-from ADDR` | Sender. Defaults to the daemon's `POSTMARKD_DEFAULT_FROM`. |
| `-subject TEXT` | Subject. |
| `-text VALUE` | Plain text body. |
| `-html VALUE` | HTML body. |
| `-attach FILE` | Attach a file; the content type comes from its extension. Repeatable. |
| `-reply-to ADDR` | Reply-To address. |
| `-tag NAME` | Postmark tag. |
| `-stream ID` | Postmark message stream, such as `outbound` or `broadcast`. |
| `-metadata KEY=VALUE` | Postmark metadata. Repeatable. |
| `-header NAME:VALUE` | Custom header. Repeatable. |
| `-template ALIAS\|ID` | Send with a Postmark template. A number is a template ID, anything else an alias. |
| `-model JSON` | Template model as JSON. Defaults to `{}`. |
| `-async` | Queue the message in the daemon and return immediately. |
| `-addr ADDR` | Daemon address (see above). |
| `-token TOKEN` | Client token. Defaults to `$POSTMARKD_TOKEN`. |

Values for `-text`, `-html` and `-model` can be `@file` to read a file or `-`
to read stdin. If no body or template is given and stdin is not a terminal,
stdin becomes the plain text body.

At least one of `-to`, `-cc` or `-bcc` is required. Exit status: `0` sent,
`1` failed (Postmark's error message is printed), `2` invalid usage.

## postmarkd sendmail

A sendmail(8)-compatible interface. It reads an RFC 5322 message on stdin,
converts it (text, HTML, attachments, inline images, `X-PM-*` headers) and
sends it through the daemon. See [sendmail](sending.md#sendmail) for setup.

```sh
printf 'Subject: Hello\n\nSent with sendmail.\n' | postmarkd sendmail ada@example.com
```

| Option | Meaning |
| --- | --- |
| `addr…` | Recipients. |
| `-t` | Take recipients from the `To`, `Cc` and `Bcc` headers, plus any given as arguments. |
| `-i`, `-oi` | Don't treat a line with a single `.` as the end of the message. |
| `-f ADDR`, `-r ADDR` | Envelope sender; used as `From` when the message has no `From:` header. |
| `-F NAME` | Display name for that sender. |
| `--` | End of options; everything after is a recipient. |
| `-B`, `-N`, `-R`, `-V`, `-X`, `-L`, `-O`, `-C`, `-h`, `-A` | Accepted with their value and ignored. |
| `-o…`, `-bm`, `-v`, `-U`, `-G` and other flags | Accepted and ignored. |
| `-bs`, `-bp`, `-bv`, `-bd`, `-bi`, `-q…` | Not supported: postmarkd has no local queue. Exits `64`. |

Environment: `POSTMARKD_ADDR`, `POSTMARKD_TOKEN`, and `POSTMARKD_STREAM` to
choose the message stream for messages that don't set `X-PM-Message-Stream`.

Exit status follows `sysexits.h`, which programs that call sendmail understand:

| Code | Meaning |
| --- | --- |
| `0` | Sent. |
| `64` | Usage error (no recipients, unsupported option). |
| `65` | The message couldn't be parsed, or Postmark rejected it. |
| `69` | The daemon doesn't offer the API. |
| `70` | Unexpected response. |
| `75` | Temporary failure: daemon or Postmark unreachable, rate limited. Try again later. |
| `77` | Not authorized: set `POSTMARKD_TOKEN`. |

## postmarkd check-domain

Checks the DNS records that make mail from a domain authenticate when sent
through Postmark. See [Email authentication](email-authentication.md).

```sh
postmarkd check-domain yourdomain.com
POSTMARK_ACCOUNT_TOKEN=… postmarkd check-domain yourdomain.com
```

| Flag | Description |
| --- | --- |
| `-account-token TOKEN` | Postmark **account** token (not a server token). Looks up the domain in Postmark to find the DKIM selector and the exact records to create. Defaults to `$POSTMARK_ACCOUNT_TOKEN`. |
| `-dkim-selector NAME` | DKIM selector to check, if you don't pass an account token. |
| `-return-path HOST` | Custom Return-Path host. Defaults to `pm-bounces.<domain>`, or the one configured in Postmark. |

Flags may come before or after the domain. Each check prints `PASS`, `INFO`,
`WARN` or `FAIL` with a fix. Exit status: `0` if nothing failed, `1` if any
check failed, `2` on invalid usage.

## postmarkd healthcheck

Exits `0` if the daemon answers `GET /readyz` with `200`, and `1` otherwise.
It is the container image's `HEALTHCHECK`.

| Flag | Description |
| --- | --- |
| `-addr ADDR` | Daemon address (see above). |

## postmarkd version

Prints the version, for example `postmarkd 1.2.0`. Same as `postmarkd -version`.
