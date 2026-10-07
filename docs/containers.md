# Containers and sidecars

The image `ghcr.io/rossedman/postmarkd` is a ~16 MB distroless image: one
static binary, CA certificates, a non-root user (65532), and nothing else.

| Default | Value |
| --- | --- |
| HTTP API | `:8025` (`POSTMARKD_LISTEN`) |
| SMTP | `:2525` (`POSTMARKD_SMTP_LISTEN`) |
| Logs | JSON on stderr |
| Health check | `postmarkd healthcheck` (Docker `HEALTHCHECK` built in) |
| Socket directory | `/run/postmarkd`, owned by the container user so a volume can hold the socket |

All configuration is through `POSTMARKD_*` environment variables (see
[configuration](configuration.md)). `systemd` features turn themselves off
outside systemd.

## Sidecar patterns

A sidecar runs next to your app and is reachable only by it. Three ways to
connect them:

| Pattern | App uses | Good for |
| --- | --- | --- |
| **Shared localhost** (Kubernetes pod, `network_mode: service:…` in Compose) | `127.0.0.1:8025` / `127.0.0.1:2525` | Simplest; nothing exposed outside the pod. |
| **Shared unix socket** (volume mounted in both containers) | `/run/postmarkd/postmarkd.sock` | No TCP at all; access controlled by the volume. |
| **Private network** (Compose network) | `postmarkd:8025` / `postmarkd:2525` | Several app containers sharing one postmarkd. |

When the socket is shared through a volume and the app runs as a different
user, set `POSTMARKD_SOCKET_MODE=0666`; the volume itself is the access boundary.

### Docker Compose

[`examples/docker-compose/compose.yaml`](../examples/docker-compose/compose.yaml):

```yaml
services:
  postmarkd:
    image: ghcr.io/rossedman/postmarkd:latest
    environment:
      POSTMARKD_SERVER_TOKEN: ${POSTMARK_SERVER_TOKEN}
      POSTMARKD_DEFAULT_FROM: noreply@example.com
      POSTMARKD_LISTEN: unix:/run/postmarkd/postmarkd.sock,:8025
      POSTMARKD_SOCKET_MODE: "0666"
    volumes: [postmarkd-socket:/run/postmarkd]

  app:
    image: your-app
    depends_on:
      postmarkd: { condition: service_healthy }
    volumes: [postmarkd-socket:/run/postmarkd]
    environment:
      SMTP_HOST: postmarkd
      SMTP_PORT: "2525"

volumes:
  postmarkd-socket:
```

### Kubernetes

[`examples/kubernetes/postmarkd-sidecar.yaml`](../examples/kubernetes/postmarkd-sidecar.yaml)
uses a **native sidecar** (an init container with `restartPolicy: Always`,
Kubernetes 1.29+). It starts and becomes ready before your app, and stops
after it, so mail sent during shutdown still goes out. It listens on
`127.0.0.1` and a socket in a pod-private in-memory `emptyDir`, so nothing is
exposed outside the pod. The token comes from a Secret.

For many services, run one shared deployment with client tokens instead:
[`examples/kubernetes/postmarkd-service.yaml`](../examples/kubernetes/postmarkd-service.yaml).

Set `terminationGracePeriodSeconds` above `POSTMARKD_SHUTDOWN_TIMEOUT`
(default 30s) so the async queue can drain on shutdown.

### sendmail inside your app image

Programs that call `/usr/sbin/sendmail` (PHP's `mail()`, cron, many CMSs)
can use the sidecar by copying the binary into your image under that name:

```dockerfile
COPY --from=ghcr.io/rossedman/postmarkd:latest /usr/bin/postmarkd /usr/sbin/sendmail
ENV POSTMARKD_ADDR=/run/postmarkd/postmarkd.sock   # or postmarkd:8025
```

### CI service container

Run it in blackhole mode as a service in GitHub Actions (or GitLab CI) and
assert on the mail your tests sent: see
[`examples/github-actions`](../examples/github-actions/integration-tests.yml)
and [Testing](testing.md).
