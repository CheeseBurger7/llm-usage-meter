.PHONY: build test lint run

build:
	go build -o bin/server ./cmd/server

test:
	go test -race ./...

lint:
	golangci-lint run ./...

run:
	go run ./cmd/server
