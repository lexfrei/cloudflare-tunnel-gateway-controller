FROM docker.io/library/golang:1.26-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS builder

ARG VERSION=development
ARG REVISION=development

# hadolint ignore=DL3018
RUN echo 'nobody:x:65534:65534:Nobody:/:' > /tmp/passwd && \
    apk add --no-cache ca-certificates

WORKDIR /build

# Copy module manifests first so `go mod download` lands in its own
# layer; subsequent source-only changes keep the deps layer cached.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Mount the BuildKit cache for the Go build cache and module cache so
# successive CI builds reuse compiled artifacts (issue #257). The mount
# targets map to the toolchain defaults in the upstream golang image:
# GOMODCACHE=/go/pkg/mod, GOCACHE=/root/.cache/go-build.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 go build -ldflags "-s -w -X main.Version=${VERSION} -X main.Gitsha=${REVISION}" -trimpath ./cmd/controller

FROM scratch

COPY --from=builder /tmp/passwd /etc/passwd
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder --chmod=555 /build/controller /cloudflare-tunnel-gateway-controller

USER 65534
EXPOSE 8080/tcp 8081/tcp
ENTRYPOINT ["/cloudflare-tunnel-gateway-controller"]
