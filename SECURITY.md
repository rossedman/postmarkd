# Security policy

## Reporting a vulnerability

Please **do not** open a public issue. Use GitHub's
[private vulnerability reporting](https://github.com/rossedman/postmarkd/security/advisories/new)
for this repository. You should get a response within a few days. Once a fix
is released you will be credited in the advisory unless you prefer otherwise.

## Supported versions

Security fixes go into the latest release.

## Verifying releases

Release archives and packages are covered by `checksums.txt`, which the
release workflow signs keylessly with [cosign](https://docs.sigstore.dev/)
using its GitHub OIDC identity. No signing keys exist to leak.

```sh
VERSION=0.2.0
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

Container images on `ghcr.io/rossedman/postmarkd` are signed the same way
(`cosign verify` with the same identity flags), and archives, packages and
images carry build provenance attestations
(`gh attestation verify <file or oci://image> --repo rossedman/postmarkd`).
Releases up to v0.1.0 were published before signing was added.

## Deployment notes

- postmarkd can send mail as your domain. Treat access to its socket or port
  like access to your Postmark server token.
- Keep the default unix socket, or enable `POSTMARKD_AUTH_TOKENS` and put TLS
  in front of postmarkd before exposing it on a network.
- Load the Postmark token with a systemd credential rather than the config
  file or environment (see `docs/configuration.md`).
