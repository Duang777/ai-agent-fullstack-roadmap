# syntax=docker/dockerfile:1

# ---------- 第一阶段：编译 ----------
FROM golang:1.23-alpine AS build
WORKDIR /src

# 先只拷依赖清单再下载：依赖不变时这一层走缓存，改代码不用重新下载
COPY go.mod ./
RUN go mod download

COPY . .
ARG VERSION=dev
# CGO_ENABLED=0：纯静态二进制，不依赖 libc，才能放进 distroless/static
# -s -w：去掉调试符号，体积小一半
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/server .

# ---------- 第二阶段：运行 ----------
# distroless：没有 shell、没有包管理器，攻击面最小；:nonroot 默认以 uid 65532 运行
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 8080
USER nonroot:nonroot
# 用 exec 形式（JSON 数组）：server 就是 PID 1，能直接收到 SIGTERM
ENTRYPOINT ["/server"]
