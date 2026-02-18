# Build stage — repo root is the build context so pkg/ is reachable.
FROM golang:1.23-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 produces a fully static binary, safe for distroless/static.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /app/collector ./edge/collector/...

# Runtime stage — distroless/static has no shell, libc, or package manager.
FROM gcr.io/distroless/static-debian12

WORKDIR /app

COPY --from=builder /app/collector /app/collector

USER nonroot:nonroot

EXPOSE 9090

HEALTHCHECK --interval=10s --timeout=3s --start-period=15s --retries=3 \
  CMD ["/app/collector", "-healthcheck"]

ENTRYPOINT ["/app/collector"]
