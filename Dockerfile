FROM golang:1.23-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY api/ ./api/

RUN mkdir -p /out/data && CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/litebase ./cmd/litebase

FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=builder /out/litebase /usr/local/bin/litebase
COPY --from=builder --chown=nonroot:nonroot /out/data /var/lib/litebase

USER nonroot:nonroot
VOLUME ["/var/lib/litebase"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/litebase"]
CMD ["serve", "--addr", "0.0.0.0:8080", "--data-dir", "/var/lib/litebase"]

