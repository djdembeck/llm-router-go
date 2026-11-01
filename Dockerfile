# Digest-pinned (the x86_64 digest; the go stage pins its own per-arch digests).
FROM node:22-alpine@sha256:025b8e975a61d94e33abeafaad1255ef44e4898cf1a1b6446860009c72e6d284 AS web
WORKDIR /web
COPY web/package.json web/bun.lock* ./
# node:22-alpine is musl, so pin the musl build of bun and verify it; the
# committed bun.lock is authoritative -- no registry resolution happens here.
RUN apk add --no-cache curl unzip \
    && curl -fsSL -o /tmp/bun.zip \
        https://github.com/oven-sh/bun/releases/download/bun-v1.4.0/bun-linux-x64-musl.zip \
    && echo "83b5f12fd258dd8d4fdcaea65ede954366aa717dab399e20093ecab280d54e7a  /tmp/bun.zip" | sha256sum -c - \
    && unzip -j /tmp/bun.zip bun-linux-x64-musl/bun -d /usr/local/bin \
    && rm /tmp/bun.zip
ENV PATH="/usr/local/bin:$PATH"
RUN bun install --frozen-lockfile
COPY web/ ./
RUN bun run build

FROM golang:1.27-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS builder
WORKDIR /src
COPY go.mod go.sum* ./
COPY main.go main_test.go ./
COPY web/embed.go web/embed_stub.go web/
COPY --from=web /web/build ./web/build
RUN CGO_ENABLED=0 go build -tags webui -o /router main.go

FROM alpine:3.24
RUN addgroup -g 10001 router \
    && adduser -D -H -u 10001 -G router router \
    && mkdir /data && chown router:router /data
COPY --from=builder /router /router
ENV LISTEN_HOST=0.0.0.0
WORKDIR /data
USER 10001:10001
ENTRYPOINT ["/router"]
