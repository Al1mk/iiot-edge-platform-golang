# Build stage — repo root is the build context so pkg/ is reachable.
FROM golang:1.23-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 produces a fully static binary, safe for distroless/static.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /app/mqtt-bridge ./cloud/mqtt-bridge/...

# Runtime stage — distroless/static has no shell, libc, or package manager.
FROM gcr.io/distroless/static-debian12

WORKDIR /app

COPY --from=builder /app/mqtt-bridge /app/mqtt-bridge

USER nonroot:nonroot

EXPOSE 9092

HEALTHCHECK --interval=10s --timeout=3s --start-period=15s --retries=3 \
  CMD ["/app/mqtt-bridge", "-healthcheck"]

ENTRYPOINT ["/app/mqtt-bridge"]
