# syntax=docker/dockerfile:experimental

FROM golang:1.21-alpine AS builder
RUN apk add -U --no-cache \
    build-base \
    ca-certificates \
    sqlite
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=linux go build -o lastfm-gonic-sync .

FROM alpine:3.18
COPY --from=builder /src/lastfm-gonic-sync /bin/
VOLUME ["/data"]
ENV GONIC_DB_PATH /data/gonic.db
CMD ["sh", "-c", "while true; do lastfm-gonic-sync; sleep 3600; done"]
