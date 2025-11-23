FROM docker.io/library/golang:1.25-alpine AS builder

ARG VERSION=development
ARG REVISION=development

# hadolint ignore=DL3018 // this is fine not to pin versions in this case
RUN echo 'nobody:x:65534:65534:Nobody:/:' > /tmp/passwd && \
    apk add --no-cache upx ca-certificates

WORKDIR /build
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.Version=${VERSION} -X main.Gitsha=${REVISION}" -trimpath ./cmd/controller && \
    upx --best --lzma controller

FROM scratch

COPY --from=builder /tmp/passwd /etc/passwd
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder --chmod=555 /build/controller /cloudflare-tunnel-gateway-controller

USER 65534
EXPOSE 8080/tcp 8081/tcp
ENTRYPOINT ["/cloudflare-tunnel-gateway-controller"]
