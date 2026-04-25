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

# Runs as root because `ip addr replace` requires CAP_NET_ADMIN. The
# capability is granted via docker-compose `cap_add: [NET_ADMIN]`; we
# inherit no other privileges.
USER 0
ENTRYPOINT ["/usr/local/bin/gua-mirror"]
