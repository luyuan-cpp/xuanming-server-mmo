# 两个镜像引用由调用方核对精确 ID 并固定；依赖变化必须换 CPP_CACHE_KEY。
# source_meta 为本轮源码清单的命名构建上下文，不能放运行配置或密钥。
ARG CPP_DEPS_IMAGE
ARG CPP_DEBUG_BASE_IMAGE

FROM ${CPP_DEPS_IMAGE} AS builder

USER root
WORKDIR /src
ARG BUILD_JOBS=1
ARG CPP_CACHE_KEY

RUN python3 --version
RUN python3 -c 'import os,re,sys; key=os.environ.get("CPP_CACHE_KEY", ""); jobs=os.environ.get("BUILD_JOBS", ""); sys.exit(0 if re.fullmatch("[0-9a-f]{64}", key) and re.fullmatch("[0-9]+", jobs) and int(jobs)>0 else "CPP_CACHE_KEY must be the 64-character lowercase hex part of the exact deps SHA, and BUILD_JOBS must be a positive integer")'

# SDK 镜像可能带有验证时的旧业务源；COPY 不会删除旧文件，先清空明确的应用输入。
# 已安装 SDK 位于 /usr/local 与 lib，保留；本轮对象缓存另由下方挂载提供。
RUN rm -rf cpp generated proto data/scene_nav_bin \
       third_party/boost/boost third_party/boost/libs/cobalt/src third_party/entt third_party/fmt third_party/spdlog \
       third_party/xxhash third_party/sol2 third_party/lua third_party/cppcodec \
       third_party/exprtk third_party/hexagons_grids third_party/ue5navmesh third_party/recastnavigation \
       third_party/librdkafka/src-cpp \
    && rm -f bin/gate bin/scene bin/battle bin/symbols/gate.debug bin/symbols/scene.debug bin/symbols/battle.debug

# 与正规 builder 使用同一套完整应用及头文件输入，依赖 SDK 留在匹配的 deps 层。
COPY cpp/ cpp/
COPY generated/ generated/
COPY proto/ proto/
COPY third_party/boost/boost/ third_party/boost/boost/
COPY third_party/boost/libs/cobalt/src/detail/exception.cpp third_party/boost/libs/cobalt/src/detail/util.cpp third_party/boost/libs/cobalt/src/detail/
COPY third_party/boost/libs/cobalt/src/error.cpp third_party/boost/libs/cobalt/src/this_thread.cpp third_party/boost/libs/cobalt/src/
COPY third_party/entt/ third_party/entt/
COPY third_party/fmt/ third_party/fmt/
COPY third_party/spdlog/ third_party/spdlog/
COPY third_party/xxhash/ third_party/xxhash/
COPY third_party/sol2/ third_party/sol2/
COPY third_party/lua/ third_party/lua/
COPY third_party/cppcodec/ third_party/cppcodec/
COPY third_party/exprtk/ third_party/exprtk/
COPY third_party/hexagons_grids/ third_party/hexagons_grids/
COPY third_party/ue5navmesh/ third_party/ue5navmesh/
COPY third_party/recastnavigation/ third_party/recastnavigation/
COPY third_party/librdkafka/src-cpp/ third_party/librdkafka/src-cpp/
COPY third_party/third_party.vcxproj third_party/third_party.vcxproj
COPY third_party/releases.lock.json third_party/releases.lock.json
COPY tools/archived/vcxproj2cmake.py tools/archived/vcxproj2cmake.py
COPY tools/scripts/build_linux.sh tools/scripts/build_linux.sh
COPY tools/scripts/docker_debug_manifest.py tools/scripts/docker_debug_manifest.py
COPY data/scene_nav_bin/ data/scene_nav_bin/
COPY --from=source_meta /source-manifest.json /src/DEBUG_SOURCE.input.json

# 构建类型也进入缓存名，避免把 RelWithDebInfo 或其它 SDK 的对象带入 Debug。
# 缓存仅存对象目录；各库、节点及分离符号仍在本轮 builder 中重新落地。
RUN --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-third-party,target=/src/third_party/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-proto,target=/src/cpp/generated/proto/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-rpc,target=/src/cpp/generated/rpc/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-proto-helpers,target=/src/cpp/generated/proto_helpers/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-table,target=/src/cpp/generated/table/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-grpc-client,target=/src/cpp/generated/grpc_client/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-core,target=/src/cpp/libs/engine/core/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-config,target=/src/cpp/libs/engine/config/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-session,target=/src/cpp/libs/engine/session/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-infra,target=/src/cpp/libs/engine/infra/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-thread-context,target=/src/cpp/libs/engine/thread_context/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-muduo-grpc,target=/src/cpp/libs/engine/muduo_grpc/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-modules,target=/src/cpp/libs/modules/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-battle-service,target=/src/cpp/libs/services/battle/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-scene-service,target=/src/cpp/libs/services/scene/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-gate-service,target=/src/cpp/libs/services/gate/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-gate,target=/src/cpp/nodes/gate/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-scene,target=/src/cpp/nodes/scene/.build,sharing=locked \
    --mount=type=cache,id=cpp-${CPP_CACHE_KEY}-Debug-battle,target=/src/cpp/nodes/battle/.build,sharing=locked \
    python3 tools/scripts/docker_debug_manifest.py --manifest /src/DEBUG_SOURCE.input.json \
       --cache-state /src/third_party/.build/debug-source-hashes.json \
    && MMORPG_STAMP_BUILD=0 bash tools/scripts/build_linux.sh --skip-deps --debug --split-debug --jobs "$BUILD_JOBS" \
    && python3 tools/scripts/docker_debug_manifest.py --manifest /src/DEBUG_SOURCE.input.json \
       --complete --output /src/DEBUG_SOURCE.json \
       --cache-state /src/third_party/.build/debug-source-hashes.json

FROM ${CPP_DEBUG_BASE_IMAGE} AS debug

USER root
WORKDIR /app/bin

# 只复用旧镜像的 GDB 与系统层，替换完整业务批次及当前 SDK 的共享库闭包。
# 配置与日志目录保持原位置，已有 Compose 卷不在镜像构建中触碰。
RUN rm -f /usr/local/lib/*.so* \
    && rm -rf /app/generated/generated_tables /app/data/scene_nav_bin \
    && mkdir -p /app/generated/generated_tables /app/data/scene_nav_bin /app/bin/logs/cpp_nodes
COPY --from=builder /usr/local/lib/*.so* /usr/local/lib/
COPY --from=builder /src/bin/gate /app/bin/gate
COPY --from=builder /src/bin/scene /app/bin/scene
COPY --from=builder /src/bin/battle /app/bin/battle
COPY --from=builder /src/bin/symbols/gate.debug /app/bin/gate.debug
COPY --from=builder /src/bin/symbols/scene.debug /app/bin/scene.debug
COPY --from=builder /src/bin/symbols/battle.debug /app/bin/battle.debug
COPY --from=builder /src/generated/tables/ /app/generated/generated_tables/
COPY --from=builder /src/data/scene_nav_bin/ /app/data/scene_nav_bin/
COPY --from=builder /src/DEBUG_SOURCE.json /app/DEBUG_SOURCE.json
RUN ldconfig \
    && cd /app/generated/generated_tables \
    && for f in *.json; do lc="$(printf '%s' "$f" | tr '[:upper:]' '[:lower:]')"; \
       if [ "$f" != "$lc" ]; then mv "$f" "$lc"; fi; done \
    && chmod 0755 /app/bin/gate /app/bin/scene /app/bin/battle \
    && chown -R 10001:10001 /app/bin/logs

ARG BUILD_VERSION=debug
ARG BUILD_COMMIT=unknown
ARG BUILD_TIME=unknown
ENV MMORPG_BUILD_VERSION="${BUILD_VERSION}" \
    MMORPG_BUILD_COMMIT="${BUILD_COMMIT}" \
    GATE_BUILD_VERSION="${BUILD_VERSION}" \
    GATE_BUILD_COMMIT="${BUILD_COMMIT}"
RUN printf 'version=%s\ncommit=%s\nbuilt_at=%s\nbuild_type=Debug\n' \
    "$BUILD_VERSION" "$BUILD_COMMIT" "$BUILD_TIME" > /app/BUILD_INFO

USER 10001:10001
