# Digest-pinned base images (CONFIG-2). Dependabot proposes digest bumps.
FROM golang:1.25-alpine@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587 AS builder
WORKDIR /src/backend
COPY runnerprotocol /src/runnerprotocol
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend .
RUN CGO_ENABLED=0 go build -trimpath -o /out/server ./cmd/server \
    && CGO_ENABLED=0 go build -trimpath -o /out/control-plane-db ./cmd/control-plane-db

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS runtime-base
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 k8squiz \
    && adduser -S -D -H -u 10001 -G k8squiz k8squiz

# Local Docker development needs access to the host daemon and is therefore an
# explicit root-only target. It contains no migration/provisioning binary.
FROM runtime-base AS server-dev
COPY --from=builder /out/server /server
EXPOSE 8080
USER 0:0
CMD ["/server"]

# Database authority is isolated in a separate one-shot image. Deployments must
# mount only the narrowly required owner-only URL file for each invocation.
FROM runtime-base AS dbtool
COPY --from=builder --chown=10001:10001 /out/control-plane-db /control-plane-db
USER 10001:10001
ENTRYPOINT ["/control-plane-db"]

# Default/public Control Plane image: non-root and server-only. Writable CAS and
# temporary paths must be mounted explicitly by the deployment manifest.
FROM runtime-base AS server
COPY --from=builder --chown=10001:10001 /out/server /server
EXPOSE 8080
USER 10001:10001
CMD ["/server"]
