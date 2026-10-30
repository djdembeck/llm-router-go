FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder
WORKDIR /src
COPY main.go .
ARG VERSION=dev
ARG GIT_COMMIT=unknown
RUN CGO_ENABLED=0 go build -ldflags="-X main.version=${VERSION} -X main.gitCommit=${GIT_COMMIT}" -o /router main.go

FROM alpine:3.20
COPY --from=builder /router /router
ARG VERSION=dev
ARG GIT_COMMIT=unknown
LABEL org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${GIT_COMMIT}"
ENTRYPOINT ["/router"]
