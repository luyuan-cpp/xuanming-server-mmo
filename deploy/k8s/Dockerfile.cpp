# ────────────────────────────────────────────────────────────────
# Multi-stage Dockerfile for C++ game nodes (gate + scene + battle).
#
# Build context MUST be the repo root.
#
# Usage:
#   docker build -f deploy/k8s/Dockerfile.cpp -t mmorpg-node:v1 .
#
# Extract debug symbols (for crash analysis):
#   docker build -f deploy/k8s/Dockerfile.cpp --target=symbols -o ./debug-symbols .
#   # → debug-symbols/gate.debug, debug-symbols/scene.debug
#
# First build is slow (~30–60 min) because gRPC is compiled from source.
# Docker layer caching speeds up subsequent builds significantly —
# as long as third_party/ hasn't changed, deps are reused.
#
# To skip the Docker multi-stage and build directly on a Linux host:
#   bash tools/scripts/build_linux.sh --release
#
# 版本注入(docs/design/release-packaging-standard-20260914.md 接口 D):
#   运行阶段接收 --build-arg BUILD_VERSION / BUILD_COMMIT / BUILD_TIME,写 OCI LABEL、
#   /app/BUILD_INFO 和环境变量 MMORPG_BUILD_* / GATE_BUILD_*。这些 ARG 只在最后几层声明,
#   不会击穿 deps / builder 的缓存。
#   builder 阶段另有显式开关 MMORPG_STAMP_BUILD=1 + MMORPG_BUILD_COMMIT=<12位sha>,把 commit
#   编进二进制(tools/scripts/build_linux.sh);默认关,因为它换一个 commit 就要全量重编 C++。
#
# 基础镜像钉 index digest,2026-09-16 用 `docker buildx imagetools inspect <image:tag>` 查得。
# 更新方法:重跑该命令取 "Digest:" 行替换 @sha256;运行阶段与 Dockerfile.runtime 必须同一 digest;
# 换 gcc 小版本等于换编译器,要单独提交并跑完 cpp-build-ci 的 compile-nodes。
# ────────────────────────────────────────────────────────────────

# ── Stage 1: Build dependencies (cached unless third_party changes) ──────
# gcc:13 在 2026-09-15 已滚到 13.5.0(arm/v7 仍是 13.4.0,正在滚动中);此前所有构建用的都是
# 13.4.0,这里钉住原版本,升级编译器另起一次改动。
FROM gcc:13.4.0@sha256:3617a214e52a25bde5375dc9503b5e67f01b6c7322a30137e2790aa8e6db5d1f AS deps

# 可按构建机内存覆盖；CPU数量不等于可同时容纳的C++编译器数量。
ARG BUILD_JOBS=4

RUN apt-get update && apt-get install -y --no-install-recommends \
    cmake \
    git \
    pkg-config \
    libssl-dev \
    libboost-dev \
    libzstd-dev \
    libcurl4-openssl-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Clone and build gRPC FIRST so that this heavy layer (~10 min) is cached
# independently of the other third-party sources.
# gRPC version MUST match the checked-out third_party/grpc submodule, which is
# what the Windows build (and therefore the source tree) actually compiles
# against. Evidence, not preference:
#   git -C third_party/grpc describe --tags   -> v1.83.0
#   git submodule status third_party/grpc     -> (v1.83.0)
#   .gitmodules                               -> branch = v1.83.x
# This line used to say v1.78.x -- the only place in the repo that disagreed,
# so the Docker image would have been built against a different gRPC major
# than the code was developed on.
# 2026-07-30: v1.80.x -> v1.83.x。gRPC 1.83 自带 protobuf v35.1（C++
# 版本 7.35.1），当前 cpp/generated 的硬校验必须是 7035001；旧 1.80/v31.1
# 生成物是 6031001。两套不能混用，否则依赖层能过、编译游戏节点时才失败。
COPY tools/patches/grpc/v1.83.0-boringssl-windows-x509-name.patch \
    /tmp/v1.83.0-boringssl-windows-x509-name.patch

# The clone is split from the submodule fetch on purpose:
#   * bloaty is gRPC's binary-size CI tool. Nothing under CMakeLists.txt/cmake/
#     references it, and it drags in its own capstone / demumble / googletest /
#     protobuf / re2 / zlib nest -- hundreds of MB fetched only to be ignored.
#     `submodule.<name>.update none` skips it; every target built here is
#     byte-identical without it.
#   * GitHub reachability from this network is unreliable ("GnuTLS recv error",
#     "early EOF" mid-pack). Both steps get a bounded retry so one bad packet
#     does not throw away a 5-minute layer. The revision pins below still gate
#     what we actually compile, so a retry cannot smuggle in a different tree.
RUN set -eux; \
    for attempt in 1 2 3; do \
        rm -rf third_party/grpc; \
        if git clone --depth 1 --branch v1.83.0 \
            https://github.com/grpc/grpc.git third_party/grpc; then \
            break; \
        fi; \
        if [ "$attempt" = 3 ]; then echo "gRPC clone failed after 3 attempts" >&2; exit 1; fi; \
        sleep 5; \
    done; \
    git -C third_party/grpc config submodule.third_party/bloaty.update none; \
    for attempt in 1 2 3; do \
        if git -C third_party/grpc submodule update --init --recursive --depth 1; then \
            break; \
        fi; \
        if [ "$attempt" = 3 ]; then echo "gRPC submodule fetch failed after 3 attempts" >&2; exit 1; fi; \
        sleep 5; \
    done; \
    grpc_sha="$(git -C third_party/grpc rev-parse HEAD)"; \
    if [ "$grpc_sha" != "c876f4da50f7da2f331888b88b2a7243514139fe" ]; then \
        echo "Unexpected gRPC revision: $grpc_sha" >&2; \
        exit 1; \
    fi; \
    protobuf_sha="$(git -C third_party/grpc/third_party/protobuf rev-parse HEAD)"; \
    if [ "$protobuf_sha" != "35cd01f9fe9afbeea38cc7b979a3b6bfcde82c03" ]; then \
        echo "Unexpected protobuf revision: $protobuf_sha" >&2; \
        exit 1; \
    fi; \
    boringssl_root="third_party/grpc/third_party/boringssl-with-bazel"; \
    boringssl_sha="$(git -C "$boringssl_root" rev-parse HEAD)"; \
    if [ "$boringssl_sha" != "2b44a3701a4788e1ef866ddc7f143060a3d196c9" ]; then \
        echo "Unexpected BoringSSL revision: $boringssl_sha" >&2; \
        exit 1; \
    fi; \
    if git -C "$boringssl_root" apply --check /tmp/v1.83.0-boringssl-windows-x509-name.patch; then \
        git -C "$boringssl_root" apply /tmp/v1.83.0-boringssl-windows-x509-name.patch; \
    elif git -C "$boringssl_root" apply --reverse --check /tmp/v1.83.0-boringssl-windows-x509-name.patch; then \
        echo "BoringSSL Windows X509_NAME compatibility patch is already applied."; \
    else \
        echo "BoringSSL patch is neither applicable nor already applied." >&2; \
        exit 1; \
    fi; \
    mkdir -p third_party/grpc/.build_linux; \
    cmake -S third_party/grpc -B third_party/grpc/.build_linux \
    -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr/local \
    -DCMAKE_CXX_STANDARD=23 -DgRPC_INSTALL=ON -DgRPC_BUILD_TESTS=OFF \
    -DgRPC_BUILD_CSHARP_EXT=OFF -DgRPC_BUILD_GRPC_CSHARP_PLUGIN=OFF \
    -DgRPC_BUILD_GRPC_NODE_PLUGIN=OFF -DgRPC_BUILD_GRPC_OBJECTIVE_C_PLUGIN=OFF \
    -DgRPC_BUILD_GRPC_PHP_PLUGIN=OFF -DgRPC_BUILD_GRPC_PYTHON_PLUGIN=OFF \
    -DgRPC_BUILD_GRPC_RUBY_PLUGIN=OFF -DABSL_PROPAGATE_CXX_STD=TRUE; \
    cmake --build third_party/grpc/.build_linux -j "$BUILD_JOBS"; \
    cmake --install third_party/grpc/.build_linux; \
    ldconfig; \
    mkdir -p /usr/local/share/mmorpg-deps; \
    printf 'grpc-linux-v1:grpc=%s:protobuf=%s\n' "$grpc_sha" "$protobuf_sha" \
        > /usr/local/share/mmorpg-deps/grpc-linux.stamp; \
    rm -rf third_party/grpc

# Copy the remaining (small) third-party sources and build them.
# Changes here do NOT invalidate the gRPC layer above.
COPY third_party/muduo-linux/ third_party/muduo-linux/
COPY third_party/redis/       third_party/redis/
COPY third_party/librdkafka/  third_party/librdkafka/
COPY third_party/yaml-cpp/    third_party/yaml-cpp/
COPY third_party/zlib/        third_party/zlib/

COPY tools/archived/setup_dependencies.sh tools/archived/setup_dependencies.sh
# Local deltas applied over the muduo submodule (see the overlay header for
# why they cannot live in the submodule itself).
COPY tools/archived/muduo_linux_overlay/ tools/archived/muduo_linux_overlay/

# muduo-linux puts contrib/ at the tree root, but C++ includes reference
# muduo/contrib/.  Create a symlink so both paths resolve.
RUN ln -s ../contrib third_party/muduo-linux/muduo/contrib

# setup_dependencies.sh detects gRPC is already installed and skips it.
RUN BUILD_JOBS="$BUILD_JOBS" bash tools/archived/setup_dependencies.sh

# ── Stage 2: Build game nodes ────────────────────────────────────────────
FROM deps AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 \
    && rm -rf /var/lib/apt/lists/*

# Copy project sources (changes more often than third_party)
COPY cpp/       cpp/
COPY generated/ generated/
COPY proto/     proto/

# Header-only / small third-party dirs needed for compilation
# (grpc/ and openssl/ are excluded via .dockerignore; grpc was cloned in deps stage)
# Boost仅作为头文件依赖；避免传输libs/中的测试与示例（约5.7万文件/504MiB）。
COPY third_party/boost/boost/       third_party/boost/boost/
COPY third_party/entt/              third_party/entt/
COPY third_party/fmt/               third_party/fmt/
COPY third_party/spdlog/            third_party/spdlog/
COPY third_party/xxhash/            third_party/xxhash/
COPY third_party/sol2/              third_party/sol2/
COPY third_party/lua/               third_party/lua/
COPY third_party/cppcodec/          third_party/cppcodec/
COPY third_party/exprtk/            third_party/exprtk/
COPY third_party/hexagons_grids/    third_party/hexagons_grids/
COPY third_party/ue5navmesh/        third_party/ue5navmesh/
COPY third_party/recastnavigation/  third_party/recastnavigation/
COPY third_party/third_party.vcxproj third_party/third_party.vcxproj

# Copy build tooling
COPY tools/archived/vcxproj2cmake.py tools/archived/vcxproj2cmake.py
COPY tools/scripts/build_linux.sh    tools/scripts/build_linux.sh

# Generate CMakeLists.txt from vcxproj, then build all libs + executables.
# --skip-deps: deps already built in previous stage.
# --relwithdebinfo: Release optimizations + debug info for crash analysis.
# --split-debug: extract .debug symbol files, then strip the binaries.
# MMORPG_STAMP_BUILD / MMORPG_BUILD_COMMIT:发布流水线才传(k8s_image.ps1 在带 -Version 时自动传)。
# 默认值固定,不传时这层缓存照常命中;一旦传入新 commit,这层必然重编 —— 这是把 commit 编进
# 二进制的代价,所以刻意不复用运行阶段每次都变的 BUILD_COMMIT。builder 阶段没有 .git,
# 开关打开时 MMORPG_BUILD_COMMIT 必须给,否则 build_linux.sh 以 exit 2 失败。
ARG MMORPG_STAMP_BUILD=0
ARG MMORPG_BUILD_COMMIT=
RUN MMORPG_STAMP_BUILD="$MMORPG_STAMP_BUILD" MMORPG_BUILD_COMMIT="$MMORPG_BUILD_COMMIT" \
    bash tools/scripts/build_linux.sh --skip-deps --relwithdebinfo --split-debug --jobs "$BUILD_JOBS"

# ── Stage 2.5: Debug symbols archive (extract with --target=symbols) ────
FROM scratch AS symbols
COPY --from=builder /src/bin/symbols/ /symbols/

# ── Stage 3: Minimal runtime image ──────────────────────────────────────
# 与 Dockerfile.runtime 同一 digest(见文件头的更新方法)。
FROM ubuntu:24.04@sha256:69cecf4bbf72d2d44a9eef1b71fb98c7fb973d78af11399deccef19beb008ad9

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    libgcc-s1 \
    libstdc++6 \
    libcurl4 \
    libssl3t64 \
    libzstd1 \
    tzdata \
    && rm -rf /var/lib/apt/lists/*

# 非 root 运行(uid/gid 10001),理由与写路径审计见 Dockerfile.runtime 同名段落。
RUN groupadd --system --gid 10001 mmorpg \
    && useradd --system --uid 10001 --gid 10001 --home-dir /app --no-create-home \
       --shell /usr/sbin/nologin mmorpg

WORKDIR /app

# Shared libraries built in deps stage (only .so files — skip .a static libs)
COPY --from=builder /usr/local/lib/*.so*  /usr/local/lib/
RUN ldconfig

# Compiled C++ node binaries
COPY --from=builder /src/bin/gate  /app/bin/gate
COPY --from=builder /src/bin/scene /app/bin/scene
COPY --from=builder /src/bin/battle /app/bin/battle

# Generated data tables (checked-in under generated/tables/)
COPY generated/tables/ /app/generated/generated_tables/

# Navigation mesh binary data
COPY data/scene_nav_bin/ /app/data/scene_nav_bin/

# Table filenames are PascalCase on disk (Windows); C++ code expects lowercase.
RUN cd /app/generated/generated_tables/ \
    && for f in *.json; do lc="$(echo "$f" | tr '[:upper:]' '[:lower:]')"; \
    [ "$f" != "$lc" ] && mv "$f" "$lc" || true; done

# Timezone data — symlink system zoneinfo so nodes find it at bin/zoneinfo/
RUN ln -s /usr/share/zoneinfo /app/bin/zoneinfo

# 运行期只有 bin/logs(含 muduo 不会自建的 logs/cpp_nodes)需要写;其余保持 root 所有、只读。
RUN mkdir -p /app/bin/etc /app/bin/logs/cpp_nodes \
    && chmod +x /app/bin/gate /app/bin/scene /app/bin/battle \
    && chown -R 10001:10001 /app/bin/logs

# 每次构建都变的 ARG 放在最后,只让下面几层随 commit 失效。
ARG BUILD_VERSION=unknown
ARG BUILD_COMMIT=unknown
ARG BUILD_TIME=unknown

LABEL org.opencontainers.image.title="mmorpg-node" \
      org.opencontainers.image.version="${BUILD_VERSION}" \
      org.opencontainers.image.revision="${BUILD_COMMIT}" \
      org.opencontainers.image.created="${BUILD_TIME}" \
      org.opencontainers.image.source="https://github.com/luyuan-cpp/xuanming-server-mmo"

ENV MMORPG_BUILD_VERSION="${BUILD_VERSION}" \
    MMORPG_BUILD_COMMIT="${BUILD_COMMIT}" \
    GATE_BUILD_VERSION="${BUILD_VERSION}" \
    GATE_BUILD_COMMIT="${BUILD_COMMIT}"

# 一行一个键,与 Dockerfile.go-svc / Dockerfile.java-svc 同格式:`kubectl exec <pod> -- cat /app/BUILD_INFO`。
RUN printf 'version=%s\ncommit=%s\nbuilt_at=%s\n' "$BUILD_VERSION" "$BUILD_COMMIT" "$BUILD_TIME" > /app/BUILD_INFO

USER 10001:10001
