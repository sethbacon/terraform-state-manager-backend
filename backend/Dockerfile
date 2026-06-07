# Build stage
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Version metadata injected at build time (passed by the release workflow).
ARG VERSION=dev
ARG BUILD_DATE=unknown
RUN CLEAN_VERSION="${VERSION#v}" && \
    CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.Version=${CLEAN_VERSION} -X main.BuildDate=${BUILD_DATE}" \
    -o /app/server ./cmd/server

# Runtime stage
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/server .
COPY --from=builder /app/internal/db/migrations ./migrations

EXPOSE 8080

ENTRYPOINT ["./server"]
