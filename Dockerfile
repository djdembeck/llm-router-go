FROM golang:1.27-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS builder
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -o /router main.go

FROM alpine:3.24
COPY --from=builder /router /router
ENTRYPOINT ["/router"]
