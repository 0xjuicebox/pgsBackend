# Stage 1: Build the Go binary using the official Go 1.22 image
FROM golang:1.22-alpine AS builder

# Set the Working Directory inside the container
WORKDIR /app

# Install git and SSL certificates (needed for downloading private Go modules if any)
RUN apk add --no-cache git ca-certificates

# Copy go.mod and go.sum files first to leverage Docker layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the Go application into a static binary
# CGO_ENABLED=0 creates a fully statically linked binary
# -ldflags="-w -s" strips debug information to drastically reduce binary size
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o main .

# -----------------------------------------------------------------------------

# Stage 2: Minimal Runtime Stage (Scratch / Minimal Alpine)
FROM alpine:latest

# Install CA certificates so Go can make secure HTTPS requests (e.g. Supabase, Twilio)
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /root/

# Copy the compiled binary from the builder stage
COPY --from=builder /app/main .

# Expose the default container port
EXPOSE 8080

# Run the binary
CMD ["./main"]
