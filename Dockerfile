# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Install dependencies
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o gateway ./cmd/gateway

# Runtime stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates docker-cli

WORKDIR /root/

# Copy binary from builder
COPY --from=builder /app/gateway .

# Expose port (informational: docker-compose runs with host networking)
EXPOSE 8082

# Run
CMD ["./gateway"]
