FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata sqlite3 && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY apsthira /app/apsthira

EXPOSE 8080

CMD ["/app/apsthira"]
