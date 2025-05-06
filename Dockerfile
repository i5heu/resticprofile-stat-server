# ──────────────────────────────
# Stage 1 – build Go server
# ──────────────────────────────
FROM golang:1.24-alpine AS builder

WORKDIR /src
COPY go.* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /tmp/resticprofile-stat-server ./main.go

# ──────────────────────────────
# Stage 2 – fetch resticprofile & slim image
# ──────────────────────────────
FROM alpine:3.20

# need curl + ca‑certs for download
RUN apk add --no-cache curl ca-certificates restic

# Download resticprofile (script places it in ./bin)
WORKDIR /tmp
RUN curl -sfL https://raw.githubusercontent.com/creativeprojects/resticprofile/master/install.sh | sh
RUN mv /tmp/bin/resticprofile /usr/local/bin/ 
RUN rmdir /tmp/bin

# Add our stat server
COPY --from=builder /tmp/resticprofile-stat-server /usr/local/bin/

# Optional non‑root user
RUN adduser -D -u 10001 restic && mkdir -p /data && chown restic:restic /data
USER restic

ENV DATA_ROOT=/data
EXPOSE 8080
WORKDIR /data

ENTRYPOINT ["/usr/local/bin/resticprofile-stat-server"]
