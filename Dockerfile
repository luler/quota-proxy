# 基于 Golang 官方镜像构建
FROM golang:1.21.0-alpine3.18 AS builder

# 设置工作目录
WORKDIR /app

# 将本地应用代码复制到容器内的工作目录
COPY . .

# 安装CA证书、设置代理、安装依赖、构建二进制文件
RUN apk add --no-cache ca-certificates upx && \
    go env -w GOPROXY=https://goproxy.cn,direct && \
    go mod download && \
    CGO_ENABLED=0 go build -ldflags="-s -w" -o /app/main . && \
    upx --best --lzma /app/main

# 运行阶段
FROM scratch
WORKDIR /app

# 复制CA证书
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# 复制主程序
COPY --from=builder /app/main .

# 复制配置文件
COPY config.yaml .

# 设置容器暴露端口
EXPOSE 3000

CMD ["./main","serve"]