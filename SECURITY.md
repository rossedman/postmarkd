# Security policy

## Reporting a vulnerability

Please **do not** open a public issue. Use GitHub's
[private vulnerability reporting](https://github.com/rossedman/postmarkd/security/advisories/new)
for this repository. You should get a response within a few days. Once a fix
is released you will be credited in the advisory unless you prefer otherwise.

## Supported versions

Security fixes go into the latest release.

## Deployment notes

- postmarkd can send mail as your domain. Treat access to its socket or port
  like access to your Postmark server token.
- Keep the default unix socket, or enable `POSTMARKD_AUTH_TOKENS` and put TLS
  in front of postmarkd before exposing it on a network.
- Load the Postmark token with a systemd credential rather than the config
  file or environment (see `docs/configuration.md`).
