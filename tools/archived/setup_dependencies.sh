#!/bin/bash
#
# setup_dependencies.sh — Build third-party C++ dependencies for Linux
#
# Builds: gRPC (+ protobuf, abseil, re2, c-ares, utf8_range),
#         muduo-linux, librdkafka, yaml-cpp, hiredis, zlib
#
# Run from repo root:  ./tools/archived/setup_dependencies.sh
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$REPO_ROOT"

CPU=$(nproc 2>/dev/null || echo 4)
echo "Build parallelism: $CPU"

LIB_DIR="$REPO_ROOT/lib"
mkdir -p "$LIB_DIR"

# 只有依赖归档无法证明它由哪个源码版本生成。保存显式 stamp，使 submodule/gitlink
# 或 overlay 变化时，持久 Linux 开发机与全新 Docker 层一样可靠地失效重建。
hash_source_tree() {
    local root="$1"
    (
        cd "$root"
        while IFS= read -r -d '' rel; do
            rel="${rel#./}"
            printf '%s\0' "$rel"
            sha256sum "$rel"
        done < <(LC_ALL=C find . -type f \
            ! -name '.git' \
            ! -path './.git/*' \
            ! -path './.build_linux/*' \
            -print0 | LC_ALL=C sort -z)
    ) | sha256sum | awk '{print $1}'
}

is_git_worktree_root() {
    local root="$1"
    local top
    local root_real
    local top_real
    top="$(git -C "$root" rev-parse --show-toplevel 2>/dev/null)" || return 1
    root_real="$(cd "$root" && pwd -P)"
    top_real="$(cd "$top" && pwd -P)"
    [ "$root_real" = "$top_real" ]
}

source_identity() {
    local name="$1"
    local root="$2"
    if is_git_worktree_root "$root"; then
        printf '%s-git-v1:%s' "$name" "$(git -C "$root" rev-parse HEAD)"
    else
        printf '%s-tree-v1:%s' "$name" "$(hash_source_tree "$root")"
    fi
}

read_stamp() {
    local stamp="$1"
    if [ -f "$stamp" ]; then
        tr -d '\r\n' < "$stamp"
    fi
}

write_stamp() {
    local stamp="$1"
    local value="$2"
    local tmp="${stamp}.tmp.$$"
    mkdir -p "$(dirname "$stamp")"
    printf '%s\n' "$value" > "$tmp"
    mv -f "$tmp" "$stamp"
}

have_all_files() {
    local path
    for path in "$@"; do
        [ -f "$path" ] || return 1
    done
}

# ── 1. CMake (skip if >= 3.22) ──────────────────────────────────────────────

CMAKE_MIN="3.22"
CMAKE_VER=$(cmake --version 2>/dev/null | head -1 | grep -oP '\d+\.\d+' || echo "0.0")
if [ "$(printf '%s\n' "$CMAKE_MIN" "$CMAKE_VER" | sort -V | head -1)" != "$CMAKE_MIN" ]; then
    echo "=== Installing CMake >= $CMAKE_MIN ==="
    apt-get update && apt-get install -y cmake
fi
echo "cmake: $(cmake --version | head -1)"

# ── 2. gRPC + protobuf + abseil (from third_party/grpc) ────────────────────

GRPC_DIR="$REPO_ROOT/third_party/grpc"
GRPC_BUILD="$GRPC_DIR/.build_linux"
GRPC_STAMP_FILE="/usr/local/share/mmorpg-deps/grpc-linux.stamp"
EXPECTED_GRPC_GIT_SHA="c876f4da50f7da2f331888b88b2a7243514139fe"
EXPECTED_PROTOBUF_GIT_SHA="35cd01f9fe9afbeea38cc7b979a3b6bfcde82c03"
GRPC_REPO_STAMP="grpc-linux-v1:grpc=$EXPECTED_GRPC_GIT_SHA:protobuf=$EXPECTED_PROTOBUF_GIT_SHA"
GRPC_ARTIFACTS=(
    /usr/local/lib/libgrpc++.a
    /usr/local/lib/libgrpc.a
    /usr/local/lib/libprotobuf.a
    /usr/local/include/google/protobuf/runtime_version.h
    /usr/local/lib/cmake/grpc/gRPCConfig.cmake
    /usr/local/lib/cmake/protobuf/protobuf-config.cmake
)

if [ -f "$GRPC_DIR/CMakeLists.txt" ]; then
    if ! is_git_worktree_root "$GRPC_DIR"; then
        GRPC_EXPECTED_STAMP="grpc-linux-v1:tree=$(hash_source_tree "$GRPC_DIR")"
    else
        PROTOBUF_DIR="$GRPC_DIR/third_party/protobuf"
        if ! is_git_worktree_root "$PROTOBUF_DIR"; then
            echo "ERROR: protobuf is not checked out as its own git worktree under $GRPC_DIR." >&2
            echo "Run: git submodule update --init --recursive" >&2
            exit 1
        fi
        ACTUAL_GRPC_GIT_SHA="$(git -C "$GRPC_DIR" rev-parse HEAD)"
        ACTUAL_PROTOBUF_GIT_SHA="$(git -C "$PROTOBUF_DIR" rev-parse HEAD)"
        if [ "$ACTUAL_GRPC_GIT_SHA" != "$EXPECTED_GRPC_GIT_SHA" ] \
            || [ "$ACTUAL_PROTOBUF_GIT_SHA" != "$EXPECTED_PROTOBUF_GIT_SHA" ]; then
            echo "ERROR: unexpected gRPC/protobuf source revision." >&2
            echo "  expected: grpc=$EXPECTED_GRPC_GIT_SHA protobuf=$EXPECTED_PROTOBUF_GIT_SHA" >&2
            echo "  actual  : grpc=$ACTUAL_GRPC_GIT_SHA protobuf=$ACTUAL_PROTOBUF_GIT_SHA" >&2
            exit 1
        fi
        GRPC_EXPECTED_STAMP="$GRPC_REPO_STAMP"
    fi
    GRPC_INSTALLED_STAMP="$(read_stamp "$GRPC_STAMP_FILE")"

    if have_all_files "${GRPC_ARTIFACTS[@]}" \
        && [ "$GRPC_INSTALLED_STAMP" = "$GRPC_EXPECTED_STAMP" ]; then
        echo "gRPC/protobuf source stamp matches, skipping"
    else
        if have_all_files "${GRPC_ARTIFACTS[@]}"; then
            echo "=== gRPC/protobuf source stamp changed; rebuilding ==="
        else
            echo "=== gRPC/protobuf install incomplete; rebuilding ==="
        fi

        # 版本升级可能删除 target 或生成文件；复用旧 CMake 目录会保留残留产物。
        # 因此 stamp 失配时清掉整个构建目录，不依赖旧目录里的文件时间戳。
        rm -f "$GRPC_STAMP_FILE"
        rm -rf "$GRPC_BUILD"
        mkdir -p "$GRPC_BUILD"
        cmake -S "$GRPC_DIR" -B "$GRPC_BUILD" \
            -DCMAKE_BUILD_TYPE=Release \
            -DCMAKE_INSTALL_PREFIX=/usr/local \
            -DCMAKE_CXX_STANDARD=23 \
            -DgRPC_INSTALL=ON \
            -DgRPC_BUILD_TESTS=OFF \
            -DgRPC_BUILD_CSHARP_EXT=OFF \
            -DgRPC_BUILD_GRPC_CSHARP_PLUGIN=OFF \
            -DgRPC_BUILD_GRPC_NODE_PLUGIN=OFF \
            -DgRPC_BUILD_GRPC_OBJECTIVE_C_PLUGIN=OFF \
            -DgRPC_BUILD_GRPC_PHP_PLUGIN=OFF \
            -DgRPC_BUILD_GRPC_PYTHON_PLUGIN=OFF \
            -DgRPC_BUILD_GRPC_RUBY_PLUGIN=OFF \
            -DABSL_PROPAGATE_CXX_STD=TRUE

        cmake --build "$GRPC_BUILD" -j "$CPU"
        cmake --install "$GRPC_BUILD"
        ldconfig
        if ! have_all_files "${GRPC_ARTIFACTS[@]}"; then
            echo "ERROR: gRPC/protobuf install completed without all required artifacts." >&2
            exit 1
        fi
        write_stamp "$GRPC_STAMP_FILE" "$GRPC_EXPECTED_STAMP"
        echo "gRPC/protobuf install ok ($GRPC_EXPECTED_STAMP)"
    fi
else
    # Dockerfile.cpp 构建重依赖层后会主动删除 clone 源码，并在删除前写入同格式
    # stamp；没有精确 stamp 时，不信任无源码可核对的归档。
    GRPC_INSTALLED_STAMP="$(read_stamp "$GRPC_STAMP_FILE")"
    if ! have_all_files "${GRPC_ARTIFACTS[@]}" \
        || [ "$GRPC_INSTALLED_STAMP" != "$GRPC_REPO_STAMP" ]; then
        echo "ERROR: gRPC source is absent and the prebuilt install has no valid source stamp." >&2
        exit 1
    fi
    echo "gRPC/protobuf prebuilt source stamp present, skipping ($GRPC_INSTALLED_STAMP)"
fi

# ── 3. hiredis (must be before muduo so muduo finds it) ─────────────────────

HIREDIS_DIR="$REPO_ROOT/third_party/redis/deps/hiredis"
if [ ! -f "$LIB_DIR/libhiredis.a" ]; then
    echo "=== Building hiredis ==="
    cd "$HIREDIS_DIR"
    make -j"$CPU"
    cp -f libhiredis.a "$LIB_DIR/"
    make install PREFIX=/usr/local
    ldconfig
    cd "$REPO_ROOT"
    echo "hiredis install ok"
else
    echo "hiredis already built, skipping"
fi

# ── 4. muduo-linux ──────────────────────────────────────────────────────────

MUDUO_DIR="$REPO_ROOT/third_party/muduo-linux"
MUDUO_BUILD="$MUDUO_DIR/.build_linux"
MUDUO_COMPAT="$MUDUO_BUILD/muduo_compat.cmake"
MUDUO_STAMP_FILE="$MUDUO_BUILD/.mmorpg-source.stamp"
MUDUO_ARTIFACTS=(
    "$LIB_DIR/libmuduo_base.a"
    "$LIB_DIR/libmuduo_net.a"
    "$LIB_DIR/libmuduo_protobuf_codec.a"
    "$LIB_DIR/libmuduo_hiredis.a"
)
OVERLAY_DIR="$SCRIPT_DIR/muduo_linux_overlay"

if [ ! -f "$MUDUO_DIR/CMakeLists.txt" ]; then
    echo "ERROR: muduo-linux source is missing: $MUDUO_DIR" >&2
    exit 1
fi
if [ ! -d "$OVERLAY_DIR" ]; then
    echo "ERROR: required muduo Linux overlay is missing: $OVERLAY_DIR" >&2
    exit 1
fi

# 每次都先应用 overlay。Git checkout 使用不可变 gitlink SHA；Docker/tar 源码
# 使用应用 overlay 后的内容哈希，重复运行仍稳定；overlay 自身另算完整目录哈希。
echo "--- checking muduo overlay ---"
overlay_count=0
while IFS= read -r -d '' overlay_file; do
    rel="${overlay_file#"$OVERLAY_DIR"/}"
    if [ ! -f "$MUDUO_DIR/$rel" ]; then
        echo "ERROR: overlay target missing in muduo: $rel" >&2
        echo "       Upstream moved or removed it; re-check the overlay." >&2
        exit 1
    fi
    if ! cmp -s "$overlay_file" "$MUDUO_DIR/$rel"; then
        cp -f "$overlay_file" "$MUDUO_DIR/$rel"
        echo "    overlaid: $rel"
    fi
    overlay_count=$((overlay_count + 1))
done < <(LC_ALL=C find "$OVERLAY_DIR" -type f -print0 | LC_ALL=C sort -z)
if [ "$overlay_count" -eq 0 ]; then
    echo "ERROR: muduo Linux overlay contains no files: $OVERLAY_DIR" >&2
    exit 1
fi

MUDUO_SOURCE_ID="$(source_identity muduo-linux "$MUDUO_DIR")"
MUDUO_OVERLAY_HASH="$(hash_source_tree "$OVERLAY_DIR")"
MUDUO_EXPECTED_STAMP="muduo-linux-v1:source=$MUDUO_SOURCE_ID:overlay=$MUDUO_OVERLAY_HASH"
MUDUO_INSTALLED_STAMP="$(read_stamp "$MUDUO_STAMP_FILE")"

if have_all_files "${MUDUO_ARTIFACTS[@]}" \
    && [ "$MUDUO_INSTALLED_STAMP" = "$MUDUO_EXPECTED_STAMP" ]; then
    echo "muduo source/overlay stamp matches, skipping"
else
    if have_all_files "${MUDUO_ARTIFACTS[@]}"; then
        echo "=== muduo source/overlay stamp changed; rebuilding ==="
    else
        echo "=== muduo install incomplete; rebuilding ==="
    fi
    rm -rf "$MUDUO_BUILD"
    mkdir -p "$MUDUO_BUILD"

    # Injected below with -DCMAKE_PROJECT_INCLUDE, which CMake runs as the
    # last step of muduo's project() call.
    #
    # Do NOT try to do either of these with -DCMAKE_CXX_FLAGS: muduo's own
    # CMakeLists.txt overwrites that variable wholesale a few lines later
    #     string(REPLACE ";" " " CMAKE_CXX_FLAGS "${CXX_FLAGS}")
    # so anything passed that way is dropped without a word.  add_definitions()
    # writes the directory COMPILE_DEFINITIONS property, which that overwrite
    # does not touch and which every add_subdirectory() below inherits.
    #
    # (1) GOOGLE_* -> ABSL_*.  muduo still uses GOOGLE_CHECK_EQ / GOOGLE_LOG /
    #     GOOGLE_DCHECK; protobuf deleted all three in v22 when it moved to
    #     abseil, and we build against protobuf 7.35.1.  These are the same
    #     three aliases the Windows build already passes -- see
    #     <PreprocessorDefinitions> in cpp/libs/engine/core/core.vcxproj and
    #     cpp/libs/engine/muduo_windows/muduo.vcxproj.  Keep both platforms on
    #     the same shim.  The ABSL_ macros are in scope at every use site
    #     because google-inl.h includes <google/protobuf/message.h> first,
    #     which reaches absl/log/absl_log.h via descriptor.h.
    #
    # (2) muduo_hiredis.  The game links -lmuduo_hiredis (EXTERNAL_LIBS in
    #     tools/archived/vcxproj2cmake.py) and includes
    #     muduo/contrib/hiredis/Hiredis.h, but upstream muduo compiles
    #     Hiredis.cc only into the mrediscli *executable* -- the library
    #     target does not exist in any muduo tree in this repo.  Windows gets
    #     it by listing Hiredis.cc directly in muduo.vcxproj; this is the
    #     Linux equivalent.
    cat > "$MUDUO_COMPAT" <<'MUDUO_COMPAT_EOF'
add_definitions(-DGOOGLE_CHECK_EQ=ABSL_CHECK_EQ)
add_definitions(-DGOOGLE_LOG=ABSL_LOG)
add_definitions(-DGOOGLE_DCHECK=ABSL_DCHECK)

add_library(muduo_hiredis STATIC
    "${CMAKE_CURRENT_SOURCE_DIR}/contrib/hiredis/Hiredis.cc")
target_include_directories(muduo_hiredis
    PRIVATE "${CMAKE_CURRENT_SOURCE_DIR}")
MUDUO_COMPAT_EOF

    cmake -S "$MUDUO_DIR" -B "$MUDUO_BUILD" \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_CXX_STANDARD=23 \
        -DMUDUO_BUILD_EXAMPLES=OFF \
        -DCMAKE_PROJECT_INCLUDE="$MUDUO_COMPAT"
    # Build only the four libraries the game links.  This deliberately skips
    # muduo_protorpc / muduo_protorpc_wire: nothing here uses them and they
    # carry more removed-protobuf-API surface than the codec does.
    cmake --build "$MUDUO_BUILD" -j "$CPU" \
        --target muduo_base muduo_net muduo_protobuf_codec muduo_hiredis
    # Copy libs to repo lib/
    find "$MUDUO_BUILD" -name "libmuduo_*.a" -exec cp {} "$LIB_DIR/" \;
    if ! have_all_files "${MUDUO_ARTIFACTS[@]}"; then
        echo "ERROR: muduo build completed without all required archives." >&2
        exit 1
    fi
    write_stamp "$MUDUO_STAMP_FILE" "$MUDUO_EXPECTED_STAMP"
    echo "muduo install ok ($MUDUO_EXPECTED_STAMP)"
fi

# ── 5. librdkafka ──────────────────────────────────────────────────────────

RDKAFKA_DIR="$REPO_ROOT/third_party/librdkafka"
RDKAFKA_BUILD="$RDKAFKA_DIR/.build_linux"
if [ ! -f "$LIB_DIR/librdkafka++.a" ]; then
    echo "=== Building librdkafka ==="
    mkdir -p "$RDKAFKA_BUILD"
    cmake -S "$RDKAFKA_DIR" -B "$RDKAFKA_BUILD" \
        -DCMAKE_BUILD_TYPE=Release \
        -DRDKAFKA_BUILD_EXAMPLES=OFF \
        -DRDKAFKA_BUILD_TESTS=OFF \
        -DRDKAFKA_BUILD_STATIC=ON
    cmake --build "$RDKAFKA_BUILD" -j "$CPU"
    find "$RDKAFKA_BUILD" -name "librdkafka*.a" -exec cp {} "$LIB_DIR/" \;
    cmake --install "$RDKAFKA_BUILD" --prefix /usr/local
    echo "librdkafka install ok"
else
    echo "librdkafka already built, skipping"
fi

# ── 6. yaml-cpp ─────────────────────────────────────────────────────────────

YAMLCPP_DIR="$REPO_ROOT/third_party/yaml-cpp"
YAMLCPP_BUILD="$YAMLCPP_DIR/.build_linux"
if [ ! -f "$LIB_DIR/libyaml-cpp.a" ]; then
    echo "=== Building yaml-cpp ==="
    mkdir -p "$YAMLCPP_BUILD"
    cmake -S "$YAMLCPP_DIR" -B "$YAMLCPP_BUILD" \
        -DCMAKE_BUILD_TYPE=Release \
        -DYAML_CPP_BUILD_TESTS=OFF \
        -DYAML_CPP_BUILD_TOOLS=OFF \
        -DYAML_BUILD_SHARED_LIBS=OFF
    cmake --build "$YAMLCPP_BUILD" -j "$CPU"
    find "$YAMLCPP_BUILD" -name "libyaml-cpp.a" -exec cp {} "$LIB_DIR/" \;
    cmake --install "$YAMLCPP_BUILD" --prefix /usr/local
    echo "yaml-cpp install ok"
else
    echo "yaml-cpp already built, skipping"
fi

# ── 7. zlib ─────────────────────────────────────────────────────────────────

ZLIB_DIR="$REPO_ROOT/third_party/zlib"
ZLIB_BUILD="$ZLIB_DIR/.build_linux"
if [ ! -f "$LIB_DIR/libz.a" ]; then
    echo "=== Building zlib ==="
    # Deliberately CMake and not ./configure.  On a Windows host git checks
    # this submodule out with CRLF endings, which turns the shebang into
    # "#!/bin/sh\r"; the kernel then looks for an interpreter literally named
    # "/bin/sh\r" and the container fails with the thoroughly misleading
    #     ./configure: cannot execute: required file not found   (exit 127)
    # Every other dependency in this script already builds through CMake,
    # which does not care about line endings.  zlib was the last holdout.
    #
    # ZLIB_BUILD_SHARED=OFF keeps this equivalent to the old `--static`: only
    # libz.a gets installed, so -lz cannot quietly resolve to a .so in
    # /usr/local/lib instead of the static archive.
    cmake -S "$ZLIB_DIR" -B "$ZLIB_BUILD" \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_INSTALL_PREFIX=/usr/local \
        -DZLIB_BUILD_SHARED=OFF \
        -DZLIB_BUILD_TESTING=OFF
    cmake --build "$ZLIB_BUILD" -j "$CPU"
    cmake --install "$ZLIB_BUILD"
    cp -f "$ZLIB_BUILD/libz.a" "$LIB_DIR/"
    echo "zlib install ok"
else
    echo "zlib already built, skipping"
fi

# ── Done ────────────────────────────────────────────────────────────────────
echo ""
echo "=== All dependencies ready ==="
echo "  Installed to: /usr/local  +  $LIB_DIR"
echo ""
echo "System prerequisites (install with apt-get if missing):"
echo "  build-essential cmake git libssl-dev libboost-dev pkg-config"
