# ---------------------------------------------------------
# Development Stage
# Used by docker-compose for local development with live-reload
# ---------------------------------------------------------
FROM golang:1.25-bookworm AS dev
WORKDIR /app
# Download dependencies first to leverage Docker cache
COPY go.mod ./
RUN go mod download
# Copy the rest of the source code
COPY . .
# Disable CGO for static binaries (standard Go practice)
ENV CGO_ENABLED=0
# Expose the default HTTP port
EXPOSE 8080
# Run the application using 'air' for live reloading
CMD ["go", "run", "github.com/air-verse/air@v1.61.1", "-c", ".air.toml"]

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
