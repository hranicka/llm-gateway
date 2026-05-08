.PHONY: build test lint docker docker-down all tools

build:
	go build -o llm-gateway ./cmd/gateway

test: vet lint
	go test ./...

lint:
	golangci-lint run

vet:
	go vet ./...

docker:
	docker-compose -f compose.yml up --build

docker-down:
	docker-compose -f compose.yml down

all: vet lint test build

tools:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
