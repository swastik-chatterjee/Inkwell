# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Stage 1 — build the Go binary.
# ---------------------------------------------------------------------------
FROM golang:1.23-alpine AS build

WORKDIR /src

# Copy updated module manifests and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source tree
COPY . .

# Compile binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -buildvcs=false -trimpath -ldflags="-s -w" \
    -o /out/markdown-social ./cmd/web

# ---------------------------------------------------------------------------
# Stage 2 — minimal runtime image.
# ---------------------------------------------------------------------------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 app \
    && adduser -S -D -u 10001 -G app app

WORKDIR /app

COPY --from=build /out/markdown-social /app/markdown-social
COPY web/ /app/web/

USER app

ENV PORT=8080
EXPOSE 8080

ENTRYPOINT ["/app/markdown-social"]
