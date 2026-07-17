# Multi-stage Dockerfile for the Terraform State Manager backend.

# Stage 1: build the Go binary.
FROM golang:1.26.4-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata && update-ca-certificates || true

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG BUILD_DATE=unknown
ARG TARGETARCH=amd64
RUN CLEAN_VERSION="${VERSION#v}" && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags="-w -s -X main.Version=${CLEAN_VERSION} -X main.BuildDate=${BUILD_DATE}" \
    -o terraform-state-manager \
    ./cmd/server

# Stage 2: minimal runtime image.
FROM alpine:3.23

# apk upgrade: apply the latest Alpine 3.23 security patches at build time (the
# base is tag-pinned, so a rebuild picks up backports).
# git: go-git shells out to git-upload-pack for file-path remotes (http/ssh
# remotes are pure-Go); needed so local-path git sources work in-container.
RUN apk upgrade --no-cache && \
    apk add --no-cache ca-certificates tzdata wget git && \
    addgroup -g 1000 tsm && \
    adduser -D -u 1000 -G tsm tsm

WORKDIR /app
COPY --from=builder /build/terraform-state-manager /app/terraform-state-manager
COPY config.example.yaml /app/config.example.yaml

USER tsm

EXPOSE 8080 9090

# busybox `wget --spider` reports "broken link" (exit 1) even on HTTP 200 for
# dynamic responses, so fetch the body to /dev/null instead — exits 0 on 2xx.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 -O /dev/null http://localhost:8080/health || exit 1

ENTRYPOINT ["/app/terraform-state-manager"]
CMD ["serve"]
