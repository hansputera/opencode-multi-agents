.PHONY: build run test clean docker-build docker-up docker-down

# Build the gateway binary
build:
	@echo "Building gateway..."
	@go build -o bin/gateway ./cmd/gateway
	@echo "Build complete: bin/gateway"

# Run the gateway
run: build
	@echo "Starting gateway..."
	@./bin/gateway

# Run tests
test:
	@echo "Running tests..."
	@go test -v -race -cover ./...

# Clean build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf bin/
	@docker ps -a | grep warp-gateway | awk '{print $$1}' | xargs -r docker rm -f
	@echo "Clean complete"

# Build Docker image
docker-build:
	@echo "Building Docker image..."
	@docker build -t opencode-gateway:latest .

# Start with Docker Compose
docker-up:
	@echo "Starting services..."
	@docker-compose up -d
	@echo "Services started. Gateway available at http://localhost:8080"

# Stop Docker Compose
docker-down:
	@echo "Stopping services..."
	@docker-compose down
	@echo "Services stopped"

# View logs
logs:
	@docker-compose logs -f gateway

# Check health
health:
	@curl -s http://localhost:8080/health | jq

# Check stats
stats:
	@curl -s http://localhost:8080/stats | jq

# Format code
fmt:
	@go fmt ./...

# Install dependencies
deps:
	@go mod download
	@go mod tidy

# Help
help:
	@echo "Available targets:"
	@echo "  build        - Build the gateway binary"
	@echo "  run          - Run the gateway"
	@echo "  test         - Run tests"
	@echo "  clean        - Clean build artifacts"
	@echo "  docker-build - Build Docker image"
	@echo "  docker-up    - Start with Docker Compose"
	@echo "  docker-down  - Stop Docker Compose"
	@echo "  logs         - View gateway logs"
	@echo "  health       - Check health status"
	@echo "  stats        - Check pool statistics"
	@echo "  fmt          - Format code"
	@echo "  deps         - Install dependencies"
