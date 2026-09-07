default:
    @just --list

build:
    go build -o bin/whatstui ./cmd/whatstui

run:
    go run ./cmd/whatstui

run-sync:
    go run ./cmd/whatstui --sync

test:
    go test ./...

test-v:
    go test -v ./...

cover:
    go test -cover ./...

fmt:
    go fmt ./...

vet:
    go vet ./...

tidy:
    go mod tidy

lint: fmt vet

check: fmt vet test

clean:
    rm -rf bin
