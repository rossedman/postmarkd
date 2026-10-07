# API reference

postmarkd speaks a subset of the [Postmark Email API](https://postmarkapp.com/developer/api/email-api).
Request bodies use Postmark's JSON format and synchronous responses are
Postmark's own, so Postmark's documentation and SDKs apply directly.

Base URL: `http://localhost` over the unix socket, or `http://<POSTMARKD_LISTEN>`
over TCP.

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/email` | Send one message. |
| `POST` | `/email/batch` | Send up to 500 messages. |
| `POST` | `/email/withTemplate` | Send one message using a Postmark template. |
| `POST` | `/email/batchWithTemplates` | Send up to 500 templated messages. |
| `GET` | `/healthz` | Liveness: `200 ok` while the process is serving. |
| `GET` | `/readyz` | Readiness: `200` when accepting mail, `503` while starting or stopping. |
| `GET` | `/metrics` | Prometheus metrics. |

Health and metrics endpoints never require authentication.

## Authentication

Only when `POSTMARKD_AUTH_TOKENS` is configured. Send either header:

```
Authorization: Bearer <token>
X-Postmark-Server-Token: <token>
```

The value is a postmarkd client token, **not** your Postmark server token;
postmarkd adds the real server token when it calls Postmark.

## Sending

### `POST /email`

```json
{
  "From": "Acme <noreply@example.com>",
  "To": "ada@example.com, grace@example.com",
  "Cc": "",
  "Bcc": "",
  "Subject": "Your receipt",
  "Tag": "receipt",
  "HtmlBody": "<p>Thanks!</p>",
  "TextBody": "Thanks!",
  "ReplyTo": "support@example.com",
  "Headers": [{ "Name": "X-Order", "Value": "1234" }],
  "TrackOpens": true,
  "TrackLinks": "HtmlOnly",
  "Metadata": { "order": "1234" },
  "Attachments": [
    { "Name": "receipt.pdf", "Content": "<base64>", "ContentType": "application/pdf" }
  ],
  "MessageStream": "outbound"
}
```

Any field Postmark accepts can be sent; postmarkd forwards fields it does not
know about unchanged. Before forwarding, postmarkd:

- fills in `From` and `MessageStream` from `POSTMARKD_DEFAULT_FROM` /
  `POSTMARKD_DEFAULT_MESSAGE_STREAM` when the message has none, and
- rejects messages with no `From`, no recipient (`To`, `Cc` or `Bcc`), or no
  `HtmlBody`/`TextBody`.

Postmark's response is returned as-is:

```json
{
  "To": "ada@example.com",
  "SubmittedAt": "2026-10-07T12:00:00.0000000Z",
  "MessageID": "b7bc2f4a-e38e-4336-af7d-e6c392c2f817",
  "ErrorCode": 0,
  "Message": "OK"
}
```

### `POST /email/batch`

A JSON array of up to 500 messages in the `/email` format. Postmark returns
`200` with one result per message; check each result's `ErrorCode`.

### `POST /email/withTemplate`

```json
{
  "From": "noreply@example.com",
  "To": "ada@example.com",
  "TemplateAlias": "welcome",
  "TemplateModel": { "name": "Ada" }
}
```

`TemplateId` or `TemplateAlias` is required; `HtmlBody`/`TextBody` are not.

### `POST /email/batchWithTemplates`

```json
{ "Messages": [ { "To": "…", "TemplateAlias": "welcome", "TemplateModel": {} } ] }
```

## Async mode

Add `?async=true` to any send URL, or the header `X-Postmarkd-Async: true`.
postmarkd validates the request, places it on an in-memory queue and replies
immediately:

```http
HTTP/1.1 202 Accepted
X-Request-Id: 1f0c6e2a9d3b4c5e6f7a8b9c

{"ErrorCode":0,"Message":"Queued","Messages":1,"RequestID":"1f0c6e2a9d3b4c5e6f7a8b9c"}
```

Workers deliver queued requests in the background with the same retry rules as
synchronous sends. Results are logged (with the request ID) and counted in
metrics; they are not reported back to the caller.

When the queue is full postmarkd answers `503` with `Retry-After: 1`, so callers
get back-pressure instead of unbounded memory growth.

> **Durability:** the queue is in memory. On a clean stop postmarkd drains it
> (up to `POSTMARKD_SHUTDOWN_TIMEOUT`), but a crash or `SIGKILL` loses queued
> mail. Use synchronous sends when every message must be confirmed.

## Request IDs

Every send response includes `X-Request-Id`. Send your own `X-Request-Id`
header (up to 128 characters) to correlate postmarkd's logs with your
application's.

## Retries

postmarkd retries a request only when Postmark cannot have accepted it, so
retries never produce duplicate mail:

- connection failures (DNS, connection refused, dial timeout),
- `429 Too Many Requests` (honouring `Retry-After`),
- `502 Bad Gateway` and `503 Service Unavailable`.

Retries use exponential backoff with jitter (≈250 ms, 500 ms, 1 s, … up to
10 s), up to `POSTMARKD_MAX_RETRIES` times. Other errors, such as `500` or a
timeout after the request was sent, are returned to the caller, because the
message may already have been sent.

## Errors

Errors use Postmark's format:

```json
{ "ErrorCode": 300, "Message": "at least one recipient ('To', 'Cc' or 'Bcc') is required" }
```

Errors produced by postmarkd itself:

| HTTP | ErrorCode | Meaning |
| --- | --- | --- |
| 401 | 10 | Missing or invalid client token. |
| 413 | 300 | Request body larger than `POSTMARKD_MAX_BODY_BYTES`. |
| 422 | 402 | Body is not valid JSON of the expected shape. |
| 422 | 300 | Message failed validation (missing From, recipient, body, template; empty or oversized batch). |
| 400 | 9002 | Async requested but the queue is disabled. |
| 502 | 9000 | Postmark could not be reached after retries. |
| 503 | 9001 | Async queue is full; retry later. |

Any other error response comes straight from Postmark; see Postmark's
[API error codes](https://postmarkapp.com/developer/api/overview#error-codes).
