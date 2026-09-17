APP := pulse-monitor
AIR_VERSION := v1.61.1

.PHONY: dev build test vet fmt tidy docker-build docker-run

dev:
	go run github.com/air-verse/air@$(AIR_VERSION)

build:
	CGO_ENABLED=0 go build -o bin/$(APP) ./cmd/server

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

docker-build:
	docker build --target prod -t $(APP):prod .

docker-run:
	docker run --rm -p 8080:8080 --env-file .env $(APP):prod
