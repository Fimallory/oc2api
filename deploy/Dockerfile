FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S oc2api \
    && adduser -S -G oc2api -h /app oc2api \
    && mkdir -p /app \
    && chown -R oc2api:oc2api /app

COPY --chmod=0755 oc2api /usr/local/bin/oc2api
COPY --chmod=0644 config.json /app/config.json

ENV TZ=Asia/Shanghai
WORKDIR /app
USER oc2api

EXPOSE 3334

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:3334/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/oc2api", "-config", "/app/config.json"]
