# Digest-pinned base images (CONFIG-2). Dependabot proposes digest bumps.
FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder
WORKDIR /src/backend
COPY runnerprotocol /src/runnerprotocol
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend .
RUN CGO_ENABLED=0 go build -trimpath -o /out/server ./cmd/server \
    && CGO_ENABLED=0 go build -trimpath -o /out/control-plane-db ./cmd/control-plane-db

FROM alpine:3.19@sha256:6baf43584bcb78f2e5847d1de515f23499913ac9f12bdf834811a3145eb11ca1 AS runtime-base
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
