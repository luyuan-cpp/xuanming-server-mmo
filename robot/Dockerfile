# ────────────────────────────────────────────────────────────────
# robot 独立构建镜像(build context = robot/ 目录本身)。
#
#   docker build -f robot/Dockerfile \
#     --build-arg BUILD_VERSION=<版本或 tag> --build-arg BUILD_COMMIT=<12 位 sha> \
#     --build-arg BUILD_TIME=<UTC yyyy-MM-ddTHH:mm:ssZ> \
#     --label org.opencontainers.image.revision=<12 位 sha> \
#     -t mmorpg-robot:<tag> robot/
#
# 与 deploy/k8s/Dockerfile.robot 产出同构镜像;K8s 压测脚本 robot_stress.ps1 用的是那一份
# (以仓库根为 context)。本文件仓库内无脚本调用,保留给"只拿到 robot/ 目录"的场景。
# 注意:以 robot/ 为 context 时根 .dockerignore 不生效,robot.exe、__debug_bin*、logs/
# 会整包传进构建上下文(只进 builder 层,不进运行镜像)。
#
# 只能走 vendor:go.mod 的 replace 指向 ../go/proto、../go/shared,不在本 context 内;
# robot/vendor/ 已包含它们。go/ 下这两个模块改动后要重跑 `go mod vendor` 并提交。
#
# 基础镜像钉 digest(2026-09-16 查询,均为多平台 index digest):
#   docker buildx imagetools inspect golang:1.24.5-alpine3.22   → 取顶部 Digest
#   docker buildx imagetools inspect alpine:3.20.10             → 同上
#   升级时 tag 与 digest 同批改,并与 deploy/k8s/Dockerfile.robot、robot/go.mod 的 go 指令保持一致。
# ────────────────────────────────────────────────────────────────

# ── Build stage ──────────────────────────────────────────────────
FROM golang:1.24.5-alpine3.22@sha256:daae04ebad0c21149979cd8e9db38f565ecefd8547cf4a591240dc1972cf1399 AS builder

# local:禁止按 go.mod 自动下载别的工具链,编译器版本只由上面的基础镜像决定。
ENV GOTOOLCHAIN=local
RUN test "$(go env GOVERSION)" = "go1.24.5"

WORKDIR /src
COPY . .
# 只读权限在 builder 里定好(COPY --from 保留权限位);运行阶段再 chmod 会让二进制在镜像里存两份。
RUN CGO_ENABLED=0 go build -mod=vendor -trimpath -ldflags="-s -w" -o /out/robot . \
    && chmod 0555 /out/robot

# ── Runtime stage ────────────────────────────────────────────────
# 3.20 分支已停止维护(2026-04);这里只收紧到精确补丁版本,换分支与其它 alpine 运行镜像同批做。
FROM alpine:3.20.10@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc

RUN apk add --no-cache ca-certificates

WORKDIR /app

# 非 root(10001)。结果 csv / jsonl 写在工作目录 /app,currency-crash 快照默认写 logs/,
# 所以 /app 目录与 /app/logs 归 10001 可写;二进制保持 root 所有且只读。/app/etc 是配置挂载点。
RUN mkdir -p /app/etc /app/logs \
    && chown 10001:10001 /app /app/logs

COPY --from=builder /out/robot /app/robot

# 每次构建都变的 ARG 放在最后,避免击穿上面几层缓存。
ARG BUILD_VERSION=unknown
ARG BUILD_COMMIT=unknown
ARG BUILD_TIME=unknown

LABEL org.opencontainers.image.title="mmorpg-robot" \
      org.opencontainers.image.version="${BUILD_VERSION}" \
      org.opencontainers.image.revision="${BUILD_COMMIT}" \
      org.opencontainers.image.created="${BUILD_TIME}" \
      org.opencontainers.image.source="https://github.com/luyuan-cpp/xuanming-server-mmo"

RUN printf 'version=%s\ncommit=%s\nbuilt_at=%s\n' \
      "${BUILD_VERSION}" "${BUILD_COMMIT}" "${BUILD_TIME}" > /app/BUILD_INFO \
    && chmod 0444 /app/BUILD_INFO

USER 10001:10001

ENTRYPOINT ["/app/robot"]
CMD ["-c", "/app/etc/robot.yaml"]
