.PHONY: build test lint run generate

build:
	go build -o bin/server ./cmd/server

test:
	go test -race ./...

lint:
	golangci-lint run ./...

run:
	go run ./cmd/server

# Regenerates internal/storage/gen from the migrations and queries.
generate:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0 generate
