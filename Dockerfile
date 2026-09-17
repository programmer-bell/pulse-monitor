# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# dev — live reload via air, used by `docker compose up`.
# air is never installed into the image: `go run ...@version` resolves it into
# the module cache on first use, exactly as the README describes.
# ---------------------------------------------------------------------------
FROM golang:1.23-bookworm AS dev
WORKDIR /app
COPY go.mod ./
COPY go.sum* ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
EXPOSE 8080
CMD ["go", "run", "github.com/air-verse/air@v1.61.1", "-c", ".air.toml"]

# ---------------------------------------------------------------------------
# builder — compile a static binary for the production image.
# ---------------------------------------------------------------------------
FROM golang:1.23-bookworm AS builder
WORKDIR /app
COPY go.mod ./
COPY go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" \
    -o /out/server ./cmd/server

# ---------------------------------------------------------------------------
# prod — distroless: no shell, no package manager, no Go toolchain.
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS prod
COPY --from=builder /out/server /server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
