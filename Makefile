# Variables for application name and tooling versions
APP := pulse-monitor
AIR_VERSION := v1.61.1

# Mark these targets as phony to avoid conflicts with files of the same name
.PHONY: dev build test vet fmt tidy docker-build docker-run

# dev: Starts the application in development mode with live reloading using 'air'
dev:
	go run github.com/air-verse/air@$(AIR_VERSION)

# build: Compiles a static binary of the application into the 'bin/' directory
build:
	CGO_ENABLED=0 go build -o bin/$(APP) ./cmd/server

# test: Runs all unit tests with the race detector enabled for concurrency safety
test:
	go test -race ./...

# vet: Analyzes the Go source code for potential errors
vet:
	go vet ./...

# fmt: Formats the Go source code according to the standard gofmt style
fmt:
	gofmt -w .

# tidy: Ensures the go.mod file matches the source code imports
tidy:
	go mod tidy

# docker-build: Builds a production-ready Docker image using the 'prod' stage
docker-build:
	docker build --target prod -t $(APP):prod .

# docker-run: Runs the locally built production Docker image on port 8080
docker-run:
	docker run --rm -p 8080:8080 --env-file .env $(APP):prod
