# Stage 1: Build binary using Go with Alpine
FROM golang:1.23-alpine AS builder

# Install Git (needed for go mod) and CA certificates
RUN apk add --no-cache git ca-certificates

# Set working directory
WORKDIR /app

# Copy go.mod and download dependencies
COPY go.mod ./
RUN go mod download

# Copy source and build
COPY . .
RUN go build -o app .

# Stage 2: Final minimal image
FROM alpine:3.18

# Install CA certs for HTTPS
RUN apk add --no-cache ca-certificates

# Copy binary
COPY --from=builder /app/app /app/app

# Set working directory
WORKDIR /app

# Default command
CMD ["./app"]
