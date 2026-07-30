# syntax=docker/dockerfile:1.7

# DuckDB uses CGO and its platform-specific bindings. Build each target image
# on that target platform (Buildx uses QEMU for the non-native architecture),
# rather than attempting a host-side cross build.
ARG GO_VERSION=1.24.13
FROM golang:${GO_VERSION}-bookworm AS build

WORKDIR /src
COPY . .

ARG VERSION=dev
ENV CGO_ENABLED=1
RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -mod=vendor -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/ai-proxy ./cmd/ai-proxy

FROM debian:bookworm-slim AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gosu tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 ai-proxy \
    && useradd --uid 10001 --gid ai-proxy --home-dir /nonexistent --shell /usr/sbin/nologin ai-proxy \
    && mkdir -p /etc/ai-proxy /var/lib/ai-proxy \
    && chown -R ai-proxy:ai-proxy /var/lib/ai-proxy

COPY --from=build /out/ai-proxy /usr/local/bin/ai-proxy
COPY config.example.yaml /usr/share/ai-proxy/config.example.yaml
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

RUN chmod 0755 /usr/local/bin/ai-proxy /usr/local/bin/docker-entrypoint.sh

ENV AI_PROXY_CONFIG=/etc/ai-proxy/config.yaml
WORKDIR /var/lib/ai-proxy
EXPOSE 8080
VOLUME ["/var/lib/ai-proxy"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["curl", "--fail", "--silent", "--show-error", "http://127.0.0.1:8080/healthz"]

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["ai-proxy"]
