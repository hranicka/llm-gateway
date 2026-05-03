.PHONY: build test lint vet docker docker-down all tools

build:
	go build -o llm-gateway .

test: vet lint
	go test ./...

lint:
	golangci-lint run

vet:
	go vet ./...

docker:
	docker-compose up --build

docker-down:
	docker-compose down

all: vet lint test build

tools:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
