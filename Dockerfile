# Stage 1: Build binary
FROM golang:alpine AS builder

RUN apk add --no-cache gcc musl-dev git

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=1 GOOS=linux go build -ldflags="-w -s" -o apsthira ./cmd/apsthira

# Stage 2: Runtime image
FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata sqlite

WORKDIR /app

COPY --from=builder /app/apsthira /app/apsthira

EXPOSE 8080

CMD ["/app/apsthira"]
