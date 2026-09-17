APP := pulse-monitor
AIR_VERSION := v1.61.1

.PHONY: dev build test vet fmt tidy docker-build docker-run

## dev: run locally with live reload (air fetched on demand, never installed)
dev:
	go run github.com/air-verse/air@$(AIR_VERSION)

## build: compile the server binary
build:
	CGO_ENABLED=0 go build -o bin/$(APP) ./cmd/server

## test: the bar — race detector on, everything
test:
	go test -race ./...

## vet: static checks
vet:
	go vet ./...

## fmt: format all Go source
fmt:
	gofmt -w .

## tidy: resolve and lock dependencies
tidy:
	go mod tidy

## docker-build: build the production (distroless) image
docker-build:
	docker build --target prod -t $(APP):prod .

## docker-run: run the production image against .env
docker-run:
	docker run --rm -p 8080:8080 --env-file .env $(APP):prod
