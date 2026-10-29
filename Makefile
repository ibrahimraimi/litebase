.PHONY: build test race bench vet fmt clean

BINARY := bin/litebase

build:
	CGO_ENABLED=1 go build -trimpath -o $(BINARY) ./cmd/litebase

test:
	go test ./...

race:
	go test -race ./...

bench:
	go test -bench=. -benchmem ./internal/engine/...

vet:
	go vet ./...

fmt:
	gofmt -s -w cmd/ internal/ test/

clean:
	rm -rf bin/

