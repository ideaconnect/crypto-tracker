# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod .
COPY main.go .

# Produce a statically linked binary so we can drop it into a minimal image.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o crypto-tracker .

# ── Stage 2: runtime ──────────────────────────────────────────────────────────
FROM alpine:3.21

# ca-certificates is required for TLS connections to the Binance HTTPS API.
RUN apk add --no-cache ca-certificates

# All runtime files (binary + generated prices.json) live here.
WORKDIR /data

COPY --from=builder /src/crypto-tracker /usr/local/bin/crypto-tracker

EXPOSE 8080

# Flags can be overridden at `docker run` time, e.g.:
#   docker run -e ... crypto-tracker -interval 30s -addr :9000
ENTRYPOINT ["crypto-tracker", "-output", "/data/prices.json", "-addr", ":8080"]
