FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/bun.lock* ./
RUN npm install --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder
WORKDIR /src
COPY go.mod go.sum* ./
COPY main.go main_test.go ./
COPY web/embed.go web/embed_stub.go ./
COPY --from=web /web/build ./web/build
RUN CGO_ENABLED=0 go build -tags webui -o /router main.go

FROM alpine:3.20
COPY --from=builder /router /router
ENTRYPOINT ["/router"]
