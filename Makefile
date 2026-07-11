.PHONY: build test check race

build:
	mkdir -p bin
	go build -trimpath -ldflags "-s -w -X main.version=dev" -o bin/ccusage-hub ./cmd/ccusage-hub

test:
	go test ./...

check:
	gofmt -d cmd internal
	go vet ./...
	go test ./...

race:
	go test -race ./...
