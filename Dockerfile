FROM golang:1.26-alpine AS builder

# BuildKit supplies these on multi-arch builds: cross-compile the Go binary
# natively on the builder arch instead of running the whole compile under QEMU.
ARG TARGETOS
ARG TARGETARCH

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o cline-proxy .

# 会话 ID 改为本地铸造（internal/app/zen_session.go + internal/kit/zenid.go）
# 后不再需要内嵌 opencode CLI：镜像不再含 node 构建段、libstdc++/libgcc 与
# 约 185MB 的 Bun 二进制。

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S app && adduser -S app -G app

WORKDIR /app
COPY --from=builder /build/cline-proxy .

RUN mkdir -p /app/data && chown -R app:app /app

USER app

EXPOSE 3457

VOLUME ["/app/data"]

# 容器内数据目录固定为 /app/data（可用 DATA_DIR 覆盖）
ENV DATA_DIR=/app/data
ENV PORT=3457

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:${PORT}/health || exit 1

ENTRYPOINT ["/app/cline-proxy"]
CMD []
