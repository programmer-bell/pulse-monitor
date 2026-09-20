# ---------------------------------------------------------
# Development Stage
# Used by docker-compose for local development with live-reload
# ---------------------------------------------------------
FROM golang:1.25-bookworm AS dev
WORKDIR /app
# Download dependencies first to leverage Docker cache
COPY go.mod ./
RUN go mod download && go install github.com/air-verse/air@v1.61.1
# The host .git is bind-mounted and owned by a non-root uid; mark it safe so
# `go build` VCS stamping does not fail with "dubious ownership" under air.
RUN git config --global --add safe.directory /app
# Copy the rest of the source code
COPY . .
# Disable CGO for static binaries (standard Go practice)
ENV CGO_ENABLED=0
# Expose the default HTTP port
EXPOSE 8080
# Run air directly as PID 1 so a SIGTERM from `docker compose kill` reaches
# the server and it shuts down gracefully, instead of `go run`'s supervisor
# chain swallowing the signal. See the phase-5 exit criteria.
CMD ["air", "-c", ".air.toml"]

# ---------------------------------------------------------
# Builder Stage
# Compiles the Go application into a standalone static binary
# ---------------------------------------------------------
FROM golang:1.25-bookworm AS builder
WORKDIR /app
# Download dependencies
COPY go.mod ./
RUN go mod download
# Copy the rest of the source code
COPY . .
# Build the binary with optimizations (-trimpath, -s -w) to reduce size
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" \
    -o /out/server ./cmd/server

# ---------------------------------------------------------
# Target Stage — throwaway local upstream for the Phase 6 load test
# ---------------------------------------------------------
FROM golang:1.25-bookworm AS target-builder
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" \
    -o /out/targetsrv ./cmd/targetsrv

FROM gcr.io/distroless/static-debian12:nonroot AS target
COPY --from=target-builder /out/targetsrv /targetsrv
EXPOSE 8099
USER nonroot:nonroot
ENTRYPOINT ["/targetsrv"]

# ---------------------------------------------------------
# Production Stage
# Minimal image containing only the compiled binary
# ---------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS prod
# Copy the compiled binary from the builder stage
COPY --from=builder /out/server /server
# Expose the HTTP port
EXPOSE 8080
# Run as a non-root user for better security
USER nonroot:nonroot
# Execute the binary
ENTRYPOINT ["/server"]
