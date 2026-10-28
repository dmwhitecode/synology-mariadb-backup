# syntax=docker/dockerfile:1

# ---- build -----------------------------------------------------------------
FROM golang:1.27-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/mariadb-backup ./cmd/server

# ---- runtime ---------------------------------------------------------------
FROM alpine:3.22
RUN apk add --no-cache mariadb-client tzdata ca-certificates \
 && adduser -D -H -u 1000 app \
 && mkdir -p /data /backups \
 && chown app:app /data /backups \
 && chmod 700 /data \
 && chmod 750 /backups

COPY --from=build /out/mariadb-backup /usr/local/bin/mariadb-backup

ENV DATA_DIR=/data \
    BACKUP_DIR=/backups \
    LISTEN=:8080 \
    MARIADB_DUMP=mariadb-dump \
    LOG_LEVEL=info

VOLUME ["/data", "/backups"]
EXPOSE 8080
USER app
HEALTHCHECK --interval=60s --timeout=5s --start-period=10s \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/mariadb-backup"]
