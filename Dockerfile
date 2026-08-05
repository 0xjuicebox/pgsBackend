# Stage 1: Build stage using Alpine-based Go 1.26
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Install git and SSL certificates using Alpine's apk
RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o main .

# -----------------------------------------------------------------------------

# Stage 2: Minimal Runtime Stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /root/

COPY --from=builder /app/main .

EXPOSE 3000

CMD ["./main"]
