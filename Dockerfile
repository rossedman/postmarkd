FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/postmarkd ./cmd/postmarkd \
 && mkdir -p /out/run/postmarkd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/postmarkd /usr/bin/postmarkd
# Owned by the nonroot user so a volume mounted here can hold the socket.
COPY --from=build --chown=65532:65532 /out/run/postmarkd /run/postmarkd
ENV POSTMARKD_LISTEN=:8025 \
    POSTMARKD_SMTP_LISTEN=:2525 \
    POSTMARKD_LOG_FORMAT=json
EXPOSE 8025 2525
USER nonroot:nonroot
HEALTHCHECK --interval=10s --timeout=3s CMD ["/usr/bin/postmarkd", "healthcheck"]
ENTRYPOINT ["/usr/bin/postmarkd"]
