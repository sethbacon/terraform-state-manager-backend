VERSION ?= dev
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

.PHONY: build test vet tidy run docker

# Build the server binary (standard crypto).
build:
	@echo "Building backend..."
	cd backend && CGO_ENABLED=0 go build \
		-ldflags="-w -s -X main.Version=$(VERSION) -X main.BuildDate=$(BUILD_DATE)" \
		-o terraform-state-manager ./cmd/server

# Run unit tests.
test:
	cd backend && go test ./...

# Static checks.
vet:
	cd backend && go vet ./...

# Resolve and tidy module dependencies.
tidy:
	cd backend && go mod tidy

# Run the server locally (expects a reachable Postgres; see config.example.yaml).
run:
	cd backend && CONFIG_PATH=../config.example.yaml go run ./cmd/server serve

# Build the Docker image.
docker:
	docker build -f backend/Dockerfile -t terraform-state-manager-backend:$(VERSION) backend/
