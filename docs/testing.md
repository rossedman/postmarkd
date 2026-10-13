# Testing with the black hole

Two ways to send mail that never reaches a real inbox.

## 1. postmarkd blackhole mode (no Postmark at all)

```sh
POSTMARKD_BLACKHOLE=true postmarkd
# or
docker run -p 8025:8025 -p 2525:2525 -e POSTMARKD_BLACKHOLE=true ghcr.io/rossedman/postmarkd:0.1
```

In blackhole mode postmarkd:

- needs no Postmark token and makes no network calls,
- still validates every message and applies your defaults, exactly as in
  production,
- answers HTTP, SMTP and sendmail clients exactly as Postmark would, with a
  generated `MessageID`,
- keeps the most recent messages (`POSTMARKD_BLACKHOLE_KEEP`, default 1000)
  so tests can check what was "sent".

```sh
# What was sent? Oldest first, as the JSON Postmark would have received.
curl -s localhost:8025/blackhole/messages
```

```json
{
  "Messages": [
    {
      "MessageID": "0f4c…",
      "ReceivedAt": "2026-10-07T12:00:00Z",
      "Path": "/email",
      "Message": { "From": "noreply@example.com", "To": "ada@example.com", "Subject": "Welcome!", "TextBody": "…" }
    }
  ]
}
```

```sh
# Clear between tests
curl -X DELETE localhost:8025/blackhole/messages
```

The startup log, `systemctl status` and `/metrics` all say when blackhole
mode is on, so it is hard to leave enabled by accident.

A test, for example in Python:

```python
import requests, smtplib
from email.message import EmailMessage

def test_signup_sends_welcome_email(app):
    requests.delete("http://localhost:8025/blackhole/messages")
    app.signup("ada@example.com")       # your code sends via SMTP or HTTP
    sent = requests.get("http://localhost:8025/blackhole/messages").json()["Messages"]
    assert [m["Message"]["Subject"] for m in sent] == ["Welcome!"]
```

See [`examples/github-actions`](../examples/github-actions/integration-tests.yml)
for running it as a CI service container.

## 2. Postmark's own test options

Use these when you want Postmark itself to validate the request (sender
signatures, templates, stream names) without delivering:

- **Test token.** Set `POSTMARKD_SERVER_TOKEN=POSTMARK_API_TEST`. Postmark
  validates the request and returns success but sends nothing.
- **Black hole address.** Send to any address at `blackhole.postmarkapp.com`
  (e.g. `test@blackhole.postmarkapp.com`) with your real token. Postmark
  processes the message fully, it shows in your activity feed, and it is
  dropped instead of delivered, without affecting your sender reputation.
  This is the best way to load-test the full path.

| | postmarkd blackhole | `POSTMARK_API_TEST` | `@blackhole.postmarkapp.com` |
| --- | --- | --- | --- |
| Needs network / Postmark account | no | yes | yes |
| Postmark validates sender, template, stream | no | partly | yes |
| Shows in Postmark activity | no | no | yes |
| Inspect sent messages from tests | yes | no | via Postmark API |
