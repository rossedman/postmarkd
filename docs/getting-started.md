# Getting started

This guide takes you from nothing to your first message sent through Postmark,
in about five minutes. If you only want to try postmarkd without a Postmark
account, jump to [Try it without Postmark](#try-it-without-postmark).

## What you need

- A Linux host with systemd, or anywhere you can run a container.
- A [Postmark](https://postmarkapp.com) account with a **server API token**
  (Postmark → Servers → your server → API Tokens).
- A verified sender: an address or domain you've confirmed in Postmark.
  Postmark only sends from verified senders.

## 1. Install

Pick one. Packages are the easiest way to run postmarkd under systemd.
Every release is on the [releases page](https://github.com/rossedman/postmarkd/releases),
for `amd64` and `arm64`.

**Debian, Ubuntu** (`.deb`):

```sh
VERSION=0.1.0
ARCH=$(dpkg --print-architecture)   # amd64 or arm64
REL=https://github.com/rossedman/postmarkd/releases/download/v$VERSION
curl -fsSLO $REL/postmarkd_${VERSION}_linux_$ARCH.deb
sudo apt install ./postmarkd_${VERSION}_linux_$ARCH.deb
```

**Fedora, RHEL, Rocky, Amazon Linux** (`.rpm`):

```sh
VERSION=0.1.0
ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
REL=https://github.com/rossedman/postmarkd/releases/download/v$VERSION
sudo dnf install $REL/postmarkd_${VERSION}_linux_$ARCH.rpm
```

Both packages install `/usr/bin/postmarkd`, the systemd units, a
`postmarkd` system user and a commented config file at
`/etc/postmarkd/postmarkd.conf`. They don't start the service; you do that
in step 4 once a token is in place.

**Just the binary**, for example to use `postmarkd send` on a workstation or
to run it without systemd:

```sh
VERSION=0.1.0
ARCH=amd64   # or arm64
REL=https://github.com/rossedman/postmarkd/releases/download/v$VERSION
curl -fsSL $REL/postmarkd_${VERSION}_linux_$ARCH.tar.gz | tar xz postmarkd
sudo install -m 0755 postmarkd /usr/local/bin/postmarkd
```

**As a container:**

```sh
docker pull ghcr.io/rossedman/postmarkd:0.1
```

Containers are configured entirely with environment variables. See
[Containers and sidecars](containers.md); the rest of this guide uses systemd.

**From source** (Go 1.23 or newer), which installs the same files as the
packages:

```sh
git clone https://github.com/rossedman/postmarkd && cd postmarkd
sudo make install
```

<details>
<summary>Verifying a download</summary>

Each release has a `checksums.txt` with SHA-256 sums of every file, signed
keylessly with [cosign](https://docs.sigstore.dev/) by the release workflow.
Check the signature, then the files you downloaded:

```sh
REL=https://github.com/rossedman/postmarkd/releases/download/v$VERSION
curl -fsSLO $REL/checksums.txt
curl -fsSLO $REL/checksums.txt.sigstore.json
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/rossedman/postmarkd/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

Every archive, package and container image also has a build provenance
attestation you can check with the GitHub CLI:

```sh
gh attestation verify postmarkd_${VERSION}_linux_amd64.tar.gz --repo rossedman/postmarkd
gh attestation verify oci://ghcr.io/rossedman/postmarkd:$VERSION --repo rossedman/postmarkd
```

Container images are signed too:

```sh
cosign verify ghcr.io/rossedman/postmarkd:$VERSION \
  --certificate-identity-regexp '^https://github\.com/rossedman/postmarkd/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

</details>

## 2. Add your Postmark token

Store the server token where only root can read it. systemd hands it to the
daemon privately as a credential, so it never appears in a config file or
the environment:

```sh
sudo install -m 0600 /dev/stdin /etc/postmarkd/server-token <<< 'your-server-token'
```

## 3. Set a default sender

Open `/etc/postmarkd/postmarkd.conf` and set the address mail comes from when
a message doesn't say:

```ini
POSTMARKD_DEFAULT_FROM=noreply@yourdomain.com
```

Every option is listed in [Configuration](configuration.md). The defaults
are sensible: the HTTP API listens on the unix socket
`/run/postmarkd/postmarkd.sock`, and SMTP is off.

## 4. Start the daemon

```sh
sudo systemctl enable --now postmarkd
systemctl status postmarkd
```

`systemctl status` shows `Accepting mail on /run/postmarkd/postmarkd.sock`
once it's ready. If it isn't running, `journalctl -u postmarkd` says why, and
`postmarkd -config /etc/postmarkd/postmarkd.conf -check-config` validates
the configuration.

## 5. Send your first message

The socket belongs to the `postmarkd` group. Add yourself (and any app user
that will send mail), then log in again:

```sh
sudo usermod -aG postmarkd "$USER"
```

Now send:

```sh
echo "Hello from $(hostname)" | postmarkd send -to you@example.com -subject "It works"
```

It prints the Postmark `MessageID`. The same message over HTTP:

```sh
curl --unix-socket /run/postmarkd/postmarkd.sock http://localhost/email \
  -d '{"To": "you@example.com", "Subject": "It works", "TextBody": "Hello"}'
```

## 6. Connect your software

| Your software | Do this |
| --- | --- |
| cron, `mail`, PHP `mail()`, anything calling sendmail | `sudo ln -s /usr/bin/postmarkd /usr/sbin/sendmail` |
| Apps that speak SMTP | Set `POSTMARKD_SMTP_LISTEN=127.0.0.1:2525`, restart, and point the app at it |
| Apps using a Postmark SDK | Point the SDK's base URL at postmarkd |
| Scripts | `postmarkd send` |

[Sending mail](sending.md) covers each of these in detail.

## 7. Make your domain trusted

Before sending real volume, check that your domain's DKIM, Return-Path and
DMARC records are in place, or your mail may land in spam:

```sh
postmarkd check-domain yourdomain.com
```

See [Email authentication](email-authentication.md).

## Try it without Postmark

Blackhole mode accepts mail exactly as Postmark would, sends nothing, and
keeps what was "sent" so you can look at it. No account or token needed:

```sh
docker run --rm -p 8025:8025 -p 2525:2525 -e POSTMARKD_BLACKHOLE=true ghcr.io/rossedman/postmarkd:0.1
```

In another terminal:

```sh
curl localhost:8025/email -d '{"From":"me@example.com","To":"you@example.com","Subject":"Hi","TextBody":"Hello"}'
curl localhost:8025/blackhole/messages
```

More in [Testing](testing.md).

## Next steps

- [Sending mail](sending.md): every way to send, streams and batches
- [Containers and sidecars](containers.md): Docker Compose, Kubernetes, CI
- [Deployment and operations](deployment.md): hardening, scaling and monitoring
