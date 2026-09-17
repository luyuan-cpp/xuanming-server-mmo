# ────────────────────────────────────────────────────────────────
# robot 压测 / 冒烟机器人镜像(K8s Job 用,deploy/k8s/robot_stress.ps1 构建)。
#
# Build context MUST be the repo root(受根 .dockerignore 约束):
#   docker build -f deploy/k8s/Dockerfile.robot \
#     --build-arg BUILD_VERSION=<版本或 tag> --build-arg BUILD_COMMIT=<12 位 sha> \
#     --build-arg BUILD_TIME=<UTC yyyy-MM-ddTHH:mm:ssZ> \
#     --label org.opencontainers.image.revision=<12 位 sha> \
#     -t mmorpg-robot:<tag> .
#   注意:robot_stress.ps1 目前只传 -f / -t,不传 BUILD_* 与 revision label,
#   经它构建的镜像 OCI label 与 /app/BUILD_INFO 均为 unknown(脚本侧待补,见发布打包标准 P3)。
#
# 为什么走 vendor 而不是 go mod download:
#   robot/go.mod 把 proto / shared replace 到 ../go/proto、../go/shared。旧写法
#   `COPY robot/go.mod ./` + `go mod download` 在 /src 下解析 replace 会落到不存在的
#   /go/proto;再加上根 .dockerignore 以前整个排除了 robot/,这个镜像两处都构建不出来。
#   robot/vendor/ 已入库且包含 replace 的本地模块,-mod=vendor 不需要 go/ 目录、不联网。
#   代价:go/proto、go/shared 改动后必须在 robot/ 下重跑 `go mod vendor` 并提交,
#   否则镜像里是旧快照 —— vendor 一致性检查只比对 go.mod 与 vendor/modules.txt,查不出内容过期。
#
# 基础镜像钉 digest(2026-09-16 查询,均为多平台 index digest):
#   docker buildx imagetools inspect golang:1.24.5-alpine3.22   → 取顶部 Digest
#   docker buildx imagetools inspect alpine:3.20.10             → 同上
#   升级时 tag 与 digest 必须来自同一次查询一起改;Go 版本还要同步 robot/go.mod 的
#   go 指令与下方 GOVERSION 断言,三处不一致构建直接失败。
# ────────────────────────────────────────────────────────────────

# ── Build stage ──────────────────────────────────────────────────
FROM golang:1.24.5-alpine3.22@sha256:daae04ebad0c21149979cd8e9db38f565ecefd8547cf4a591240dc1972cf1399 AS builder

# local:禁止按 go.mod 自动下载别的工具链,编译器版本只由上面的基础镜像决定。
ENV GOTOOLCHAIN=local
RUN test "$(go env GOVERSION)" = "go1.24.5"

WORKDIR /src/robot
COPY robot/ ./
# 只读权限在 builder 里定好:COPY --from 会保留权限位。若在运行阶段再 chmod,overlay 会把整个
# 二进制复制进新层(镜像里存两份),且那层排在 BUILD_* ARG 之后,每次构建都重生成。
RUN CGO_ENABLED=0 go build -mod=vendor -trimpath -ldflags="-s -w" -o /out/robot . \
    && chmod 0555 /out/robot

# ── Runtime stage ────────────────────────────────────────────────
# alpine 3.20 分支已停止维护(2026-04),这里只把原先的 3.20 收紧到精确补丁版本,不顺手换分支;
# 换新分支要所有 alpine 运行镜像同批进行,并重跑 apk 包可用性验证。
FROM alpine:3.20.10@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc

RUN apk add --no-cache ca-certificates

WORKDIR /app

# 非 root(10001)运行。写路径审计(robot/*.go):结果文件 behavior_test_results.csv / .jsonl、
# login_test_results.csv 写在工作目录 /app,currency-crash 快照默认写 logs/currency_crash_window/。
# 所以 /app 目录本身与 /app/logs 必须归 10001 可写;二进制保持 root 所有且只读。
# /app/etc 只是 ConfigMap 挂载点(robot_stress.ps1 以只读挂载 robot.yaml),不需要可写。
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
