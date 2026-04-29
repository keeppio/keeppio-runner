# ─── frontend builder ──────────────────────────────────────────────────
# Compiles frontend/app.css → static/app.css using the Tailwind v4
# standalone CLI (no Node toolchain). Also vendors htmx so the runtime
# image carries no third-party CDN dependencies.
#
# Debian-slim instead of alpine: the v4 standalone binary (compiled with
# Bun) needs libstdc++ + libgcc_s symbols that alpine's musl base lacks
# even with libstdc++ apk'd. Frontend stage doesn't ship to runtime so
# the extra ~70MB here costs nothing in the deployed image.
FROM debian:12-slim AS frontend
WORKDIR /src
ARG TAILWIND_VERSION=v4.2.4
ARG HTMX_VERSION=2.0.4
ARG TARGETARCH=amd64
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && case "$TARGETARCH" in \
        amd64) asset=tailwindcss-linux-x64 ;; \
        arm64) asset=tailwindcss-linux-arm64 ;; \
        *) echo "unsupported TARGETARCH: $TARGETARCH"; exit 1 ;; \
    esac \
 && curl -fsSL -o /usr/local/bin/tailwindcss \
      "https://github.com/tailwindlabs/tailwindcss/releases/download/${TAILWIND_VERSION}/${asset}" \
 && chmod +x /usr/local/bin/tailwindcss
COPY frontend ./frontend
COPY templates ./templates
COPY static ./static
RUN tailwindcss -i frontend/app.css -o static/app.css --minify
RUN mkdir -p static/vendor \
 && curl -fsSL -o static/vendor/htmx.min.js \
      "https://unpkg.com/htmx.org@${HTMX_VERSION}/dist/htmx.min.js"

# ─── builder ───────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# Overlay the generated frontend assets so go:embed picks them up.
COPY --from=frontend /src/static/app.css ./static/app.css
COPY --from=frontend /src/static/vendor ./static/vendor
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
 && ansible-galaxy collection install --upgrade community.general community.docker ansible.posix 2>&1 | tail -3 \
 && rm -rf /root/.ansible/tmp /var/cache/apk/*

# Non-root user so logs/db files land with predictable ownership.
RUN addgroup -S -g 1000 runner && adduser -S -u 1000 -G runner runner
COPY --from=builder /out/keeppio-runner /usr/local/bin/keeppio-runner

USER runner
WORKDIR /home/runner
EXPOSE 3000

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/usr/local/bin/keeppio-runner"]
