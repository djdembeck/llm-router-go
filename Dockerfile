FROM golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS builder
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -o /router main.go

FROM alpine:3.24
COPY --from=builder /router /router
ENTRYPOINT ["/router"]
