FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -o /router main.go

FROM alpine:3.20
COPY --from=builder /router /router
ENTRYPOINT ["/router"]
