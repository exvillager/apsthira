# ---- Build stage ----
FROM golang:1.26-alpine AS builder

# gcc + musl-dev required because mattn/go-sqlite3 uses cgo
RUN apk add --no-cache gcc musl-dev

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=1 go build -ldflags="-s -w" -o apsthira ./cmd/apsthira

# ---- Run stage ----
FROM alpine:latest AS runner

# ca-certificates for TLS to Cloudflare R2 / Supabase
RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /app/apsthira ./

# SQLite database lives here; mounted as a volume in compose
RUN mkdir -p /data
ENV DB_PATH=/data/resumes.db

EXPOSE 8080

ENTRYPOINT ["./apsthira"]
