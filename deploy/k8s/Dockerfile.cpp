# ────────────────────────────────────────────────────────────────
# Multi-stage Dockerfile for C++ game nodes (gate + scene).
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
# ────────────────────────────────────────────────────────────────

# ── Stage 1: Build dependencies (cached unless third_party changes) ──────
FROM gcc:13 AS deps

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
#   git -C third_party/grpc describe --tags   -> v1.80.0
#   git submodule status third_party/grpc     -> (v1.80.0)
#   .gitmodules                               -> branch = v1.80.x
# This line used to say v1.78.x -- the only place in the repo that disagreed,
# so the Docker image would have been built against a different gRPC major
# than the code was developed on.
RUN git clone --depth 1 --branch v1.80.x --recurse-submodules --shallow-submodules \
    https://github.com/grpc/grpc.git third_party/grpc \
    && mkdir -p third_party/grpc/.build_linux \
    && cmake -S third_party/grpc -B third_party/grpc/.build_linux \
    -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr/local \
    -DCMAKE_CXX_STANDARD=23 -DgRPC_INSTALL=ON -DgRPC_BUILD_TESTS=OFF \
    -DgRPC_BUILD_CSHARP_EXT=OFF -DgRPC_BUILD_GRPC_CSHARP_PLUGIN=OFF \
    -DgRPC_BUILD_GRPC_NODE_PLUGIN=OFF -DgRPC_BUILD_GRPC_OBJECTIVE_C_PLUGIN=OFF \
    -DgRPC_BUILD_GRPC_PHP_PLUGIN=OFF -DgRPC_BUILD_GRPC_PYTHON_PLUGIN=OFF \
    -DgRPC_BUILD_GRPC_RUBY_PLUGIN=OFF -DABSL_PROPAGATE_CXX_STD=TRUE \
    && cmake --build third_party/grpc/.build_linux -j "$(nproc)" \
    && cmake --install third_party/grpc/.build_linux \
    && ldconfig \
    && rm -rf third_party/grpc

# Copy the remaining (small) third-party sources and build them.
# Changes here do NOT invalidate the gRPC layer above.
COPY third_party/muduo-linux/ third_party/muduo-linux/
COPY third_party/redis/       third_party/redis/
COPY third_party/librdkafka/  third_party/librdkafka/
COPY third_party/yaml-cpp/    third_party/yaml-cpp/
COPY third_party/zlib/        third_party/zlib/

COPY tools/archived/setup_dependencies.sh tools/archived/setup_dependencies.sh

# muduo-linux puts contrib/ at the tree root, but C++ includes reference
# muduo/contrib/.  Create a symlink so both paths resolve.
RUN ln -s ../contrib third_party/muduo-linux/muduo/contrib

# setup_dependencies.sh detects gRPC is already installed and skips it.
RUN bash tools/archived/setup_dependencies.sh

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
COPY third_party/boost/             third_party/boost/
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
RUN bash tools/scripts/build_linux.sh --skip-deps --relwithdebinfo --split-debug

# ── Stage 2.5: Debug symbols archive (extract with --target=symbols) ────
FROM scratch AS symbols
COPY --from=builder /src/bin/symbols/ /symbols/

# ── Stage 3: Minimal runtime image ──────────────────────────────────────
FROM ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    libgcc-s1 \
    libstdc++6 \
    libcurl4 \
    libssl3t64 \
    libzstd1 \
    tzdata \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Shared libraries built in deps stage (only .so files — skip .a static libs)
COPY --from=builder /usr/local/lib/*.so*  /usr/local/lib/
RUN ldconfig

# Compiled C++ node binaries
COPY --from=builder /src/bin/gate  /app/bin/gate
COPY --from=builder /src/bin/scene /app/bin/scene

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

RUN mkdir -p /app/bin/etc /app/bin/logs \
    && chmod +x /app/bin/gate /app/bin/scene
