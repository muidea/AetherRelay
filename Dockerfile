# syntax=docker/dockerfile:1.7

# DuckDB uses CGO and its platform-specific bindings. Build each target image
# on that target platform (Buildx uses QEMU for the non-native architecture),
# rather than attempting a host-side cross build.
ARG GO_VERSION=1.26.7
FROM golang:${GO_VERSION}-bookworm AS build

WORKDIR /src

# 依赖层单独成层：vendor 有数百 MB（DuckDB 预编译静态库），与业务源码分开后，
# 只有 go.mod / go.sum / vendor 变化时才会重新传输并重建这一层。
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY . .

ARG VERSION=dev
ENV CGO_ENABLED=1
RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -mod=vendor -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/AetherRelay ./cmd/aetherrelay

FROM debian:bookworm-slim AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gosu tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 aetherrelay \
    && useradd --uid 10001 --gid aetherrelay --home-dir /nonexistent --shell /usr/sbin/nologin aetherrelay \
    && mkdir -p /etc/aetherrelay /var/lib/aetherrelay \
    && chown -R aetherrelay:aetherrelay /var/lib/aetherrelay

COPY --from=build /out/AetherRelay /usr/local/bin/AetherRelay
COPY config.example.yaml /usr/share/aetherrelay/config.example.yaml
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

RUN chmod 0755 /usr/local/bin/AetherRelay /usr/local/bin/docker-entrypoint.sh

ENV AETHERRELAY_CONFIG=/etc/aetherrelay/config.yaml
WORKDIR /var/lib/aetherrelay
EXPOSE 8080
VOLUME ["/var/lib/aetherrelay"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["curl", "--fail", "--silent", "--show-error", "http://127.0.0.1:8080/healthz"]

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["AetherRelay"]
