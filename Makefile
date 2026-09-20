# Variables for application name and tooling versions
APP := pulse-monitor
AIR_VERSION := v1.61.1

# Mark these targets as phony to avoid conflicts with files of the same name
.PHONY: dev build test vet fmt tidy docker-build docker-run test-db-up test-db-down test-integration load-up load-down load

# dev: Starts the application in development mode with live reloading using 'air'
dev:
	go run github.com/air-verse/air@$(AIR_VERSION)

# build: Compiles a static binary of the application into the 'bin/' directory
build:
	CGO_ENABLED=0 go build -o bin/$(APP) ./cmd/server

# test: Runs all unit tests with the race detector enabled for concurrency safety
test:
	go test -race ./...

# test-db-up: Starts the throwaway Postgres used by the integration test.
#             Isolated to port 5433 so it never collides with the production
#             DATABASE_URL in .env (that stays pointing at Neon).
test-db-up:
	docker compose -f compose.test.yaml up -d

# test-db-down: Stops and wipes the integration Postgres (-v drops its volume).
test-db-down:
	docker compose -f compose.test.yaml down -v

# test-integration: Runs the real-HTTP-stack integration test against the
#                   dockerized Postgres (skipped automatically when
#                   TEST_DATABASE_URL is unset, so CI's plain `make test`
#                   stays green without a database).
test-integration:
	TEST_DATABASE_URL=postgres://pulse_test:pulse_test@localhost:5433/pulse_test \
		go test -race ./integration/...

# load-up: Builds and starts the Phase 6 load-test stack: the prod app
#          pointed at DATABASE_URL (Neon) plus an internal target server
#          serving the monitored URLs. See compose.load.yaml.
load-up:
	docker compose -f compose.load.yaml up -d --build

# load-down: Stops the load-test stack.
load-down:
	docker compose -f compose.load.yaml down

# load: Drives one load run from the host: seeds N targets, samples /metrics
#       for the duration, reports checks/sec + p50/p99, then cleans up its
#       own rows. See README > Performance.
load:
	go run ./cmd/loadtest

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
