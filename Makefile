BINARY=seo-agent
BUILD_DIR=./bin

.PHONY: build run dev tidy lint clean

## Seed MongoDB with mock GSC data for testing
seed:
	go run ./cmd/seed

## Manually trigger a job (usage: make trigger task=ingest)
trigger:
	go run ./cmd/trigger $(task)

## Build the binary
build:
	go build -o $(BUILD_DIR)/$(BINARY) ./cmd/server

## Run in development (with .env)
dev:
	go run ./cmd/server

## Run built binary
run:
	$(BUILD_DIR)/$(BINARY)

## Tidy and vendor dependencies
tidy:
	go mod tidy

## Lint (requires golangci-lint)
lint:
	golangci-lint run ./...

## Clean build artifacts
clean:
	rm -rf $(BUILD_DIR)

## Install dependencies
deps:
	go mod download

## Run with PM2 (production)
pm2-start:
	pm2 start $(BUILD_DIR)/$(BINARY) --name 91astro-seo --restart-delay=5000

pm2-stop:
	pm2 stop 91astro-seo

pm2-logs:
	pm2 logs 91astro-seo

## Build for Linux (deploy to VPS)
build-linux:
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY)-linux ./cmd/server
