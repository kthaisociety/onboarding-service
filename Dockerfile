FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 works here because the sqlite driver (glebarez/sqlite, over
# modernc.org/sqlite) is pure Go, unlike the more common mattn/go-sqlite3.
RUN CGO_ENABLED=0 GOOS=linux go build -o onboarding-service ./cmd/api

FROM alpine:latest

RUN apk --no-cache add ca-certificates \
    && adduser -D -u 10001 app \
    && mkdir -p /data && chown -R app:app /data

WORKDIR /app
COPY --from=builder /app/onboarding-service .

ENV DB_PATH=/data/onboarding.db \
    HOST=0.0.0.0 \
    PORT=8000

USER app
VOLUME ["/data"]
EXPOSE 8000

CMD ["./onboarding-service"]
