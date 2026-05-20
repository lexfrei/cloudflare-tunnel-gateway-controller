FROM docker.io/library/golang:1.26-alpine@sha256:91eda9776261207ea25fd06b5b7fed8d397dd2c0a283e77f2ab6e91bfa71079d AS builder

ARG VERSION=development
ARG REVISION=development

# hadolint ignore=DL3018
RUN echo 'nobody:x:65534:65534:Nobody:/:' > /tmp/passwd && \
    apk add --no-cache ca-certificates

WORKDIR /build
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.Version=${VERSION} -X main.Gitsha=${REVISION}" -trimpath ./cmd/controller

FROM scratch

COPY --from=builder /tmp/passwd /etc/passwd
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder --chmod=555 /build/controller /cloudflare-tunnel-gateway-controller

USER 65534
EXPOSE 8080/tcp 8081/tcp
ENTRYPOINT ["/cloudflare-tunnel-gateway-controller"]
