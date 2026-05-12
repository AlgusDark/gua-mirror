FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
WORKDIR /src

COPY go.mod go.sum* ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/gua-mirror \
    ./cmd/gua-mirror

FROM alpine:3.20
RUN apk add --no-cache iproute2 ca-certificates tzdata \
    && addgroup -S app && adduser -S -G app app

COPY --from=build /out/gua-mirror /usr/local/bin/gua-mirror

# The daemon updates the mtime of /run/gua-mirror/healthy on every successful
# reconcile. If the file is missing or older than 2 hours, the daemon is
# either stuck or has never managed a successful detection -- both are
# "unhealthy" states the operator wants surfaced. The default threshold
# matches 2x the default SAFETY_POLL_INTERVAL (1h); operators using a
# different interval should override this block in docker-compose.
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD [ -n "$(find /run/gua-mirror/healthy -mmin -120 2>/dev/null)" ] \
        || exit 1

# Runs as root because `ip addr replace` requires CAP_NET_ADMIN. The
# capability is granted via docker-compose `cap_add: [NET_ADMIN]`; we
# inherit no other privileges.
USER 0
ENTRYPOINT ["/usr/local/bin/gua-mirror"]
