# ─── builder ───────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# Static build so the image only needs ansible + git + bash at runtime.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags='-s -w' -o /out/keeppio-runner .

# ─── runtime ───────────────────────────────────────────────────────────
# Alpine + ansible (community.general & posix already shipped). git for
# repo fetch, bash for ansible internals, openssh-client for deploy
# SSH key based connections to tenant boxes.
FROM alpine:3.20
RUN apk add --no-cache \
      ansible-core \
      ansible \
      git \
      bash \
      openssh-client \
      python3 \
      py3-pip \
      ca-certificates \
      rsync \
      tini \
 && ansible-galaxy collection install community.general community.docker ansible.posix 2>&1 | tail -3 \
 && rm -rf /root/.ansible/tmp /var/cache/apk/*

# Non-root user so logs/db files land with predictable ownership.
RUN addgroup -S -g 1000 runner && adduser -S -u 1000 -G runner runner
COPY --from=builder /out/keeppio-runner /usr/local/bin/keeppio-runner

USER runner
WORKDIR /home/runner
EXPOSE 3000

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/usr/local/bin/keeppio-runner"]
