FROM golang:1.25-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY api/ ./api/

RUN CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/litebase ./cmd/litebase

FROM gcr.io/distroless/base-debian12:nonroot

VOLUME ["/var/lib/litebase"]
EXPOSE 8080

COPY --from=builder /out/litebase /usr/local/bin/litebase

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/litebase"]
CMD ["serve", "--addr", "127.0.0.1:8080", "--data-dir", "/var/lib/litebase"]

