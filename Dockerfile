# =============================================================================
# Blogger Proxy — Docker 多阶段构建
# =============================================================================
#
# 构建命令：
#   docker build -t blogger-proxy .
#
# 运行命令：
#   docker run -d -p 8000:8000 \
#     -v $(pwd)/static_cache:/app/static_cache \
#     -v $(pwd)/config.yaml:/app/config.yaml \
#     blogger-proxy
#
# 多阶段构建说明：
#   阶段 1（builder）：在完整的 Go 编译环境中编译二进制文件
#   阶段 2（runtime）：在最小化的 Alpine 镜像中运行，减小最终镜像体积
# =============================================================================

# ---- 阶段 1：编译阶段 ----
# 使用 Go 官方镜像（Alpine 变体以减小体积）
FROM golang:1.26-alpine AS builder

WORKDIR /app

# 先复制依赖文件，利用 Docker 缓存层
# 这样只有 go.mod/go.sum 变化时才重新下载依赖，源码变化不会触发
COPY go.mod go.sum ./
RUN go mod download

# 复制全部源码并编译
COPY . .

# 静态编译：
#   CGO_ENABLED=0  — 禁用 CGO，生成纯静态二进制（不依赖 libc）
#   GOOS=linux     — 目标操作系统
#   -ldflags="-w -s" — 去掉调试信息和符号表，减小二进制体积
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o blogger-proxy .

# ---- 阶段 2：运行阶段 ----
# 使用最小化的 Alpine 镜像
FROM alpine:latest

# 安装运行时依赖：
#   ca-certificates — SSL/TLS 证书（HTTPS 请求需要）
#   tzdata          — 时区数据（日志时间戳需要）
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# 从编译阶段拷贝二进制文件
COPY --from=builder /app/blogger-proxy .

# 拷贝配置文件模板（实际部署时建议通过 volume 挂载覆盖）
COPY config.yaml .

# 创建缓存目录并设置权限
# 777 权限确保容器内任意用户都能写入（生产环境建议根据实际运行用户调整）
RUN mkdir -p /app/static_cache && chmod 777 /app/static_cache

# 暴露默认端口
EXPOSE 8000

# 启动命令：运行二进制文件，指定配置文件路径
CMD ["/app/blogger-proxy", "./config.yaml"]