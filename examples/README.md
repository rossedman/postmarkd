# Examples

| Directory | What it shows |
| --- | --- |
| [`docker-compose/`](docker-compose/compose.yaml) | postmarkd next to an app container, sharing a unix socket volume and offering SMTP on the compose network. |
| [`kubernetes/postmarkd-sidecar.yaml`](kubernetes/postmarkd-sidecar.yaml) | Native sidecar (Kubernetes 1.29+), socket in a pod-private `emptyDir`, nothing exposed outside the pod. |
| [`kubernetes/postmarkd-service.yaml`](kubernetes/postmarkd-service.yaml) | A shared postmarkd `Service` for many apps, with client tokens. |
| [`github-actions/`](github-actions/integration-tests.yml) | Blackhole mode as a CI service container; tests assert on captured mail. |
| [`app-image/`](app-image/Dockerfile) | Copy the binary into an app image as `/usr/sbin/sendmail`. |

The container image is `ghcr.io/rossedman/postmarkd`. It runs as a non-root
user, listens on `:8025` (HTTP) and `:2525` (SMTP) by default, logs JSON and
has a built-in `HEALTHCHECK` (`postmarkd healthcheck`). Build it locally with
`docker build -t postmarkd .`.
