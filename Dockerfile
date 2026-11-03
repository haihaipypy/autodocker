# ---- 构建阶段 ----
FROM golang:1.27-alpine AS builder

WORKDIR /src

# 纯 Go 依赖（含 SQLite），无需 CGO，便于交叉编译到 arm64
ENV CGO_ENABLED=0

# 先只拷依赖清单：go mod download 单独成层，改业务代码时不必重下依赖
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 版本号由 CI 按 tag 传进来，编进二进制里给「自检更新」用。
# 分支构建传 dev —— 拿 sha 当版本号会被当成「1.4.1 这种数字版本」去比大小，结论全是错的。
ARG VERSION=dev
RUN go build -trimpath \
    -ldflags="-s -w -X github.com/haihaipypy/autodocker/internal/selfcheck.Version=${VERSION}" \
    -o /out/autodocker ./cmd/server

# ---- 运行阶段 ----
FROM alpine:3.24

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /out/autodocker /app/autodocker

# 前端资源已由 web/embed.go 的 go:embed 编进二进制，
# 镜像里不需要 web 目录（STATIC_DIR 指向的目录不存在时会自动回退到内嵌资源）。

ENV TZ=Asia/Shanghai \
    PORT=17777 \
    DB_PATH=/data/autodocker.db

# 刻意用 root 跑：要读写 /var/run/docker.sock，而该 socket 的属组 GID
# 每台宿主机都不一样（常见 0 / 998 / 999），写死一个 GID 会让一部分机器直接起不来。
# 安全边界靠「端口只在内网 + 设置 ADMIN_PASSWORD」，见 README「安全须知」。

EXPOSE 17777
VOLUME ["/data"]

# 探针打静态首页，不打 /api/health —— 后者在配了 ADMIN_PASSWORD 时需要鉴权，
# 一旦用户设了密码就会一直 401，容器被误判成不健康。
# 用 ${PORT} 而不是写死端口，否则用户改了端口健康检查必然失败。
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${PORT:-17777}/" || exit 1

ENTRYPOINT ["/app/autodocker"]

# OCI 标签：GHCR 据此把镜像包自动关联到源码仓库，包页面能直接看到 README 与提交
LABEL org.opencontainers.image.title="AutoDocker" \
      org.opencontainers.image.description="Docker 镜像更新监测：发现新版本推送到飞书，点卡片按钮即可按原配置重建容器，失败自动回滚" \
      org.opencontainers.image.source="https://github.com/haihaipypy/autodocker" \
      org.opencontainers.image.url="https://github.com/haihaipypy/autodocker" \
      org.opencontainers.image.licenses="MIT"
