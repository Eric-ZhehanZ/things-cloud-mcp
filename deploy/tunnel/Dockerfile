# cloudflared wrapped in a guard that keeps it connected only while the
# node's app is healthy (see guard.sh).
ARG CLOUDFLARED_VERSION=latest
FROM cloudflare/cloudflared:${CLOUDFLARED_VERSION} AS cloudflared

FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=cloudflared /usr/local/bin/cloudflared /usr/local/bin/cloudflared
COPY guard.sh /usr/local/bin/tunnel-guard
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/tunnel-guard"]
