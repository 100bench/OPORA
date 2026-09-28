FROM golang:1.26.3-alpine AS build

WORKDIR /src

# COPY is deliberately limited to module metadata and Go source. The matching
# .dockerignore prevents presentations, media, secrets and corpora entering the
# build context.
COPY go.* ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/opora \
    ./cmd/service

FROM alpine:3.22 AS runtime

RUN apk add --no-cache ca-certificates \
    && addgroup -S opora \
    && adduser -S -D -H -u 10001 -G opora opora

COPY --from=build --chown=opora:opora /out/opora /usr/local/bin/opora

USER opora:opora
EXPOSE 8080
ENV LISTEN_ADDR=0.0.0.0:8080

ENTRYPOINT ["/usr/local/bin/opora"]
