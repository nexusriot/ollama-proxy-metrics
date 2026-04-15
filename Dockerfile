FROM golang:1.25-alpine AS builder

WORKDIR /build

# Download dependencies first (layer-cached until go.mod/go.sum change)
COPY go.mod go.sum ./
RUN go mod download

# Build
COPY . .
RUN CGO_ENABLED=0 go build \
      -ldflags="-s -w" \
      -o /bin/ollama-proxy \
      ./cmd/ollama-proxy-metrics/

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /bin/ollama-proxy /bin/ollama-proxy

# Data dir: SQLite DB + logs live here. Mount as a volume in production.
VOLUME ["/data"]

EXPOSE 8080

ENV LISTEN_ADDR=":8080" \
    OLLAMA_UPSTREAM="http://ollama:11434" \
    DB_PATH="/data/db.sqlite" \
    LOG_PATH="/data/logs/proxy.log"

ENTRYPOINT ["/bin/ollama-proxy"]
