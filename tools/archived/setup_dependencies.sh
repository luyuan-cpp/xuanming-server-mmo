#!/bin/bash
#
# setup_dependencies.sh — Build third-party C++ dependencies for Linux
#
# Builds: 独立 Protobuf、Abseil、BoringSSL（protobuf 自带 utf8_range），
#         muduo-linux, curl, librdkafka, yaml-cpp, hiredis, zlib, nghttp2
#
# Run from repo root:  ./tools/archived/setup_dependencies.sh
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$REPO_ROOT"

CPU=${BUILD_JOBS:-$(nproc 2>/dev/null || echo 4)}
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

# ── 5. zlib（先于 curl 与 librdkafka 安装固定版本）───────────────────────

ZLIB_DIR="$REPO_ROOT/third_party/zlib"
ZLIB_BUILD="$ZLIB_DIR/.build_linux"
ZLIB_STAMP_FILE="/usr/local/share/mmorpg-deps/zlib-linux.stamp"
ZLIB_SOURCE_ID="$(source_identity zlib "$ZLIB_DIR")"
ZLIB_EXPECTED_STAMP="zlib-linux-v2:source=$ZLIB_SOURCE_ID:shared=1:static=1:ctest=1"
ZLIB_INSTALLED_STAMP="$(read_stamp "$ZLIB_STAMP_FILE")"
ZLIB_ARTIFACTS=(
    "$LIB_DIR/libz.a"
    /usr/local/lib/libz.a
    /usr/local/lib/libz.so
    /usr/local/include/zlib.h
    /usr/local/include/zconf.h
)
if have_all_files "${ZLIB_ARTIFACTS[@]}" \
    && [ "$ZLIB_INSTALLED_STAMP" = "$ZLIB_EXPECTED_STAMP" ] \
    && cmp -s /usr/local/lib/libz.a "$LIB_DIR/libz.a"; then
    echo "zlib 源码 stamp 与归档一致，跳过构建"
else
    echo "=== zlib 源码变化或安装不完整，重新构建 ==="
    rm -f "$ZLIB_STAMP_FILE"
    rm -rf "$ZLIB_BUILD"
    # CMake 不依赖 configure 的 shebang 换行；明确构建两种库并启用全部上游测试。
    # v1.3.2 的静态 suffix=s 仅由 WIN32 设置，Linux 原生名称仍为 libz.a。
    # 保留静态归档标准名称，不改第三方源码。
    cmake -S "$ZLIB_DIR" -B "$ZLIB_BUILD" \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_INSTALL_PREFIX=/usr/local \
        -DCMAKE_INSTALL_LIBDIR=lib \
        -DCMAKE_POSITION_INDEPENDENT_CODE=ON \
        -DZLIB_BUILD_SHARED=ON \
        -DZLIB_BUILD_STATIC=ON \
        -DZLIB_BUILD_TESTING=ON
    cmake --build "$ZLIB_BUILD" -j "$CPU"
    ctest --test-dir "$ZLIB_BUILD" --output-on-failure --no-tests=error
    if ! have_all_files "$ZLIB_BUILD/libz.a" "$ZLIB_BUILD/libz.so"; then
        echo "ERROR: zlib 本轮构建未产出所需静态库和共享库。" >&2
        exit 1
    fi
    cmake --install "$ZLIB_BUILD"
    cp -f "$ZLIB_BUILD/libz.a" "$LIB_DIR/libz.a"
    if ! have_all_files "${ZLIB_ARTIFACTS[@]}" \
        || ! cmp -s "$ZLIB_BUILD/libz.a" /usr/local/lib/libz.a \
        || ! cmp -s "$ZLIB_BUILD/libz.a" "$LIB_DIR/libz.a"; then
        echo "ERROR: zlib 安装缺少产物，或安装归档与本轮构建不一致。" >&2
        exit 1
    fi
    ldconfig
    write_stamp "$ZLIB_STAMP_FILE" "$ZLIB_EXPECTED_STAMP"
    echo "zlib 上游测试与安装完成 ($ZLIB_EXPECTED_STAMP)"
fi

# ── 2. 独立序列化与 TLS SDK ─────────────────────────────────────────

BUILD_JOBS="$CPU" bash "$REPO_ROOT/tools/scripts/third_party/build_linux_sdk.sh"
ldconfig

# ── 3. hiredis (must be before muduo so muduo finds it) ─────────────────────

HIREDIS_DIR="$REPO_ROOT/third_party/redis/deps/hiredis"
HIREDIS_BUILD="$REPO_ROOT/build/third_party/hiredis-linux"
HIREDIS_STAMP_FILE="/usr/local/share/mmorpg-deps/hiredis-linux.stamp"
HIREDIS_SOURCE_ID="$(source_identity hiredis "$HIREDIS_DIR")"
HIREDIS_EXPECTED_STAMP="hiredis-linux-v1:source=$HIREDIS_SOURCE_ID"
HIREDIS_INSTALLED_STAMP="$(read_stamp "$HIREDIS_STAMP_FILE")"
HIREDIS_ARTIFACTS=(
    "$LIB_DIR/libhiredis.a"
    /usr/local/lib/libhiredis.a
    /usr/local/include/hiredis/hiredis.h
)
if have_all_files "${HIREDIS_ARTIFACTS[@]}" \
    && [ "$HIREDIS_INSTALLED_STAMP" = "$HIREDIS_EXPECTED_STAMP" ]; then
    echo "hiredis 源码 stamp 一致，跳过构建"
else
    echo "=== hiredis 源码变化或安装不完整，重新构建 ==="
    rm -f "$HIREDIS_STAMP_FILE"
    # hiredis 的 Makefile 在当前目录产物；复制到独立目录，避免生成物污染源码哈希。
    rm -rf "$HIREDIS_BUILD"
    mkdir -p "$HIREDIS_BUILD"
    cp -a "$HIREDIS_DIR/." "$HIREDIS_BUILD/"
    cd "$HIREDIS_BUILD"
    # 来源可能留有旧对象，升级后不能仅靠文件时间戳判断它们有效。
    make clean
    make -j"$CPU"
    cp -f libhiredis.a "$LIB_DIR/"
    make install PREFIX=/usr/local
    ldconfig
    cd "$REPO_ROOT"
    if ! have_all_files "${HIREDIS_ARTIFACTS[@]}"; then
        echo "ERROR: hiredis 安装后缺少所需产物。" >&2
        exit 1
    fi
    write_stamp "$HIREDIS_STAMP_FILE" "$HIREDIS_EXPECTED_STAMP"
    echo "hiredis 安装完成 ($HIREDIS_EXPECTED_STAMP)"
fi

# ── 4. muduo-linux ──────────────────────────────────────────────────────────

MUDUO_DIR="$REPO_ROOT/third_party/muduo-linux"
MUDUO_BUILD="$MUDUO_DIR/.build_linux"
MUDUO_COMPAT="$MUDUO_BUILD/muduo_compat.cmake"
MUDUO_STAMP_FILE="$MUDUO_BUILD/.mmorpg-source.stamp"
BOOST_DIR="$REPO_ROOT/third_party/boost"
MUDUO_ARTIFACTS=(
    "$LIB_DIR/libmuduo_base.a"
    "$LIB_DIR/libmuduo_net.a"
    "$LIB_DIR/libmuduo_protobuf_codec.a"
    "$LIB_DIR/libmuduo_hiredis.a"
    "$LIB_DIR/libmuduo_http.a"
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
if ! have_all_files "$BOOST_DIR/boost/version.hpp" "$BOOST_DIR/boost/any.hpp" \
    || ! grep -Eq '^#define BOOST_VERSION 109200([[:space:]]|$)' "$BOOST_DIR/boost/version.hpp"; then
    echo "ERROR: muduo requires the copied Boost 1.92.0 headers at $BOOST_DIR/boost." >&2
    echo "Run refresh_boost_headers.py before building dependencies; system Boost is not a fallback." >&2
    exit 1
fi
# TcpConnection/EventLoop 的公开布局含 boost::any，依赖库和业务必须使用同一套头。
# 聚合头是普通副本，不能只用 Boost 根仓 HEAD 代表它的实际内容。
BOOST_SOURCE_ID="boost-headers-tree-v1:$(hash_source_tree "$BOOST_DIR/boost")"

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
MUDUO_EXPECTED_STAMP="muduo-linux-v3:source=$MUDUO_SOURCE_ID:overlay=$MUDUO_OVERLAY_HASH:boost=$BOOST_SOURCE_ID"
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

# Boost 是外部头文件，保留 muduo 自身的警告即错误策略。
include_directories(SYSTEM "${BOOST_ROOT}")

add_library(muduo_hiredis STATIC
    "${CMAKE_CURRENT_SOURCE_DIR}/contrib/hiredis/Hiredis.cc")
target_include_directories(muduo_hiredis
    PRIVATE "${CMAKE_CURRENT_SOURCE_DIR}")
MUDUO_COMPAT_EOF

    cmake -S "$MUDUO_DIR" -B "$MUDUO_BUILD" \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_CXX_STANDARD=23 \
        -DMUDUO_BUILD_EXAMPLES=OFF \
        -DBoost_NO_BOOST_CMAKE=ON \
        -DBoost_NO_SYSTEM_PATHS=ON \
        -DBOOST_ROOT="$BOOST_DIR" \
        -DBOOST_INCLUDEDIR="$BOOST_DIR" \
        -DBoost_INCLUDE_DIR="$BOOST_DIR" \
        -DCMAKE_PROJECT_INCLUDE="$MUDUO_COMPAT"
    BOOST_CONFIGURED_INCLUDE="$(sed -n 's/^Boost_INCLUDE_DIR:PATH=//p' "$MUDUO_BUILD/CMakeCache.txt")"
    if [ "$BOOST_CONFIGURED_INCLUDE" != "$BOOST_DIR" ]; then
        echo "ERROR: muduo resolved unexpected Boost headers: $BOOST_CONFIGURED_INCLUDE" >&2
        exit 1
    fi
    # Build only the five libraries the game links.  This deliberately skips
    # muduo_protorpc / muduo_protorpc_wire: nothing here uses them and they
    # carry more removed-protobuf-API surface than the codec does.
    # muduo_http backs the C++ nodes' Prometheus /metrics exporter
    # (cpp/libs/engine/core/metrics/metrics_http_exporter.cpp).
    cmake --build "$MUDUO_BUILD" -j "$CPU" \
        --target muduo_base muduo_net muduo_protobuf_codec muduo_hiredis muduo_http
    # Copy libs to repo lib/
    find "$MUDUO_BUILD" -name "libmuduo_*.a" -exec cp {} "$LIB_DIR/" \;
    if ! have_all_files "${MUDUO_ARTIFACTS[@]}"; then
        echo "ERROR: muduo build completed without all required archives." >&2
        exit 1
    fi
    write_stamp "$MUDUO_STAMP_FILE" "$MUDUO_EXPECTED_STAMP"
    echo "muduo install ok ($MUDUO_EXPECTED_STAMP)"
fi

# ── 6. curl（Agones HTTP 与 librdkafka 共用固定源码版本）──────────────────

CURL_DIR="$REPO_ROOT/third_party/curl"
CURL_BUILD="$CURL_DIR/.build_linux"
CURL_STAMP_FILE="/usr/local/share/mmorpg-deps/curl-linux.stamp"
if [ ! -f "$CURL_DIR/CMakeLists.txt" ]; then
    echo "ERROR: curl 源码不存在: $CURL_DIR" >&2
    echo "请执行: git submodule update --init third_party/curl" >&2
    exit 1
fi

# TLS 消费者统一使用独立固定的 BoringSSL；不能把这套头与系统 OpenSSL 库混配。
BORINGSSL_INCLUDE_DIR=/usr/local/include
BORINGSSL_SSL_LIBRARY=/usr/local/lib/libssl.a
BORINGSSL_CRYPTO_LIBRARY=/usr/local/lib/libcrypto.a
if ! have_all_files "$BORINGSSL_INCLUDE_DIR/openssl/base.h" \
    "$BORINGSSL_INCLUDE_DIR/openssl/ssl.h" \
    "$BORINGSSL_SSL_LIBRARY" "$BORINGSSL_CRYPTO_LIBRARY" \
    || ! grep -Eq '^#define OPENSSL_IS_BORINGSSL([[:space:]]|$)' "$BORINGSSL_INCLUDE_DIR/openssl/base.h"; then
    echo "ERROR: 缺少独立 BoringSSL 的完整头与静态库。" >&2
    exit 1
fi
BORINGSSL_HEADERS_HASH="$(hash_source_tree "$BORINGSSL_INCLUDE_DIR/openssl")"
BORINGSSL_ARCHIVES_HASH="$(sha256sum "$BORINGSSL_SSL_LIBRARY" "$BORINGSSL_CRYPTO_LIBRARY" | sha256sum | awk '{print $1}')"
BORINGSSL_SOURCE_ID="boringssl-v2:boringssl=2b44a3701a4788e1ef866ddc7f143060a3d196c9:headers=$BORINGSSL_HEADERS_HASH:archives=$BORINGSSL_ARCHIVES_HASH"

CURL_SOURCE_ID="$(source_identity curl "$CURL_DIR")"
CURL_EXPECTED_STAMP="curl-linux-v3:source=$CURL_SOURCE_ID:zlib=$ZLIB_SOURCE_ID:tls=$BORINGSSL_SOURCE_ID:shared=1:versioned=1:no-undefined=1"
CURL_INSTALLED_STAMP="$(read_stamp "$CURL_STAMP_FILE")"
CURL_ARTIFACTS=(
    /usr/local/lib/libcurl.so
    /usr/local/include/curl/curl.h
    /usr/local/include/curl/curlver.h
    /usr/local/lib/cmake/CURL/CURLConfig.cmake
)
if have_all_files "${CURL_ARTIFACTS[@]}" \
    && [ "$CURL_INSTALLED_STAMP" = "$CURL_EXPECTED_STAMP" ]; then
    echo "curl 源码 stamp 一致，跳过构建"
else
    echo "=== curl 源码变化或安装不完整，重新构建 ==="
    rm -f "$CURL_STAMP_FILE"
    rm -rf "$CURL_BUILD"
    cmake -S "$CURL_DIR" -B "$CURL_BUILD" \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_INSTALL_PREFIX=/usr/local \
        -DCMAKE_INSTALL_LIBDIR=lib \
        -DBUILD_SHARED_LIBS=ON \
        -DBUILD_STATIC_LIBS=OFF \
        -DBUILD_CURL_EXE=OFF \
        -DBUILD_TESTING=OFF \
        -DCURL_ZLIB=ON \
        -DZLIB_INCLUDE_DIR=/usr/local/include \
        -DZLIB_LIBRARY=/usr/local/lib/libz.so \
        -DCURL_USE_OPENSSL=ON \
        -DOPENSSL_USE_STATIC_LIBS=ON \
        -DOPENSSL_ROOT_DIR=/usr/local \
        -DOPENSSL_INCLUDE_DIR="$BORINGSSL_INCLUDE_DIR" \
        -DOPENSSL_SSL_LIBRARY="$BORINGSSL_SSL_LIBRARY" \
        -DOPENSSL_CRYPTO_LIBRARY="$BORINGSSL_CRYPTO_LIBRARY" \
        -DCURL_HIDDEN_SYMBOLS=ON \
        -DCURL_LIBCURL_VERSIONED_SYMBOLS=ON \
        -DCMAKE_SHARED_LINKER_FLAGS=-Wl,--no-undefined
    # 官方版本脚本只导出 curl_*，也把嵌入的静态 BoringSSL 完整限制在共享库内部。
    if ! grep -Eq '^HAVE_BORINGSSL:INTERNAL=(1|TRUE)$' "$CURL_BUILD/CMakeCache.txt" \
        || ! grep -Eq '^HAVE_VERSIONED_SYMBOLS:INTERNAL=(1|TRUE)$' "$CURL_BUILD/CMakeCache.txt"; then
        echo "ERROR: curl 未确认 BoringSSL 后端或 curl_* 限定导出。" >&2
        exit 1
    fi
    cmake --build "$CURL_BUILD" -j "$CPU"
    cmake --install "$CURL_BUILD"
    ldconfig
    if ! have_all_files "${CURL_ARTIFACTS[@]}"; then
        echo "ERROR: curl 安装后缺少所需产物。" >&2
        exit 1
    fi
    write_stamp "$CURL_STAMP_FILE" "$CURL_EXPECTED_STAMP"
    echo "curl 安装完成 ($CURL_EXPECTED_STAMP)"
fi

# ── 7. librdkafka ──────────────────────────────────────────────────────────

RDKAFKA_DIR="$REPO_ROOT/third_party/librdkafka"
RDKAFKA_BUILD="$RDKAFKA_DIR/.build_linux"
RDKAFKA_STAMP_FILE="/usr/local/share/mmorpg-deps/librdkafka-linux.stamp"
RDKAFKA_PATCH="$REPO_ROOT/tools/patches/librdkafka/v2.16.0-cgrp-rejoin-varargs.patch"
# Docker 的源码副本没有 .git；补丁必须能在普通目录中幂等恢复，不能依赖宿主已应用。
# Windows 上下文可带 CRLF；仅规范本补丁涉及的 C 文件，不依赖 Git 全局换行设置。
sed -i 's/\r$//' "$RDKAFKA_DIR/src/rdkafka_cgrp.c"
if git -C "$RDKAFKA_DIR" apply --reverse --check "$RDKAFKA_PATCH" >/dev/null 2>&1; then
    echo "librdkafka 重入组变参补丁已应用"
elif git -C "$RDKAFKA_DIR" apply --check "$RDKAFKA_PATCH"; then
    git -C "$RDKAFKA_DIR" apply "$RDKAFKA_PATCH"
else
    echo "ERROR: librdkafka 重入组变参补丁既不能应用，也未完整应用。" >&2
    exit 1
fi
RDKAFKA_BORINGSSL_PATCH="$REPO_ROOT/tools/patches/librdkafka/v2.16.0-boringssl-compat.patch"
# 只规范本补丁涉及的两个文件，使无 .git 的 Docker 副本也能按相同字节幂等恢复。
sed -i 's/\r$//' "$RDKAFKA_DIR/src/rdrand.c" "$RDKAFKA_DIR/src/rdkafka_ssl.c"
if git -C "$RDKAFKA_DIR" apply --reverse --check "$RDKAFKA_BORINGSSL_PATCH" >/dev/null 2>&1; then
    echo "librdkafka BoringSSL 兼容补丁已应用"
elif git -C "$RDKAFKA_DIR" apply --check "$RDKAFKA_BORINGSSL_PATCH"; then
    git -C "$RDKAFKA_DIR" apply "$RDKAFKA_BORINGSSL_PATCH"
else
    echo "ERROR: librdkafka BoringSSL 兼容补丁既不能应用，也未完整应用。" >&2
    exit 1
fi
RDKAFKA_BORINGSSL_PATCH_ID="$(sha256sum "$RDKAFKA_BORINGSSL_PATCH" | awk '{print $1}')"
RDKAFKA_PATCH_ID="$(sha256sum "$RDKAFKA_PATCH" | awk '{print $1}')"
RDKAFKA_SOURCE_ID="$(source_identity librdkafka "$RDKAFKA_DIR")"
# Git 提交号不包含本地补丁；摘要必须进入 stamp，避免复用修复前的静态库。
RDKAFKA_EXPECTED_STAMP="librdkafka-linux-v5:source=$RDKAFKA_SOURCE_ID:patch=$RDKAFKA_PATCH_ID:boringssl_patch=$RDKAFKA_BORINGSSL_PATCH_ID:implicit-declarations=error:zlib=$ZLIB_SOURCE_ID:curl=$CURL_EXPECTED_STAMP"
RDKAFKA_INSTALLED_STAMP="$(read_stamp "$RDKAFKA_STAMP_FILE")"
RDKAFKA_ARTIFACTS=(
    "$LIB_DIR/librdkafka.a"
    "$LIB_DIR/librdkafka++.a"
    /usr/local/lib/librdkafka.a
    /usr/local/lib/librdkafka++.a
    /usr/local/include/librdkafka/rdkafka.h
    /usr/local/include/librdkafka/rdkafkacpp.h
)
if have_all_files "${RDKAFKA_ARTIFACTS[@]}" \
    && [ "$RDKAFKA_INSTALLED_STAMP" = "$RDKAFKA_EXPECTED_STAMP" ]; then
    echo "librdkafka 源码 stamp 一致，跳过构建"
else
    echo "=== librdkafka 源码变化或安装不完整，重新构建 ==="
    rm -f "$RDKAFKA_STAMP_FILE"
    rm -rf "$RDKAFKA_BUILD"
    cmake -S "$RDKAFKA_DIR" -B "$RDKAFKA_BUILD" \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_C_FLAGS=-Werror=implicit-function-declaration \
        -DCMAKE_INSTALL_PREFIX=/usr/local \
        -DCMAKE_INSTALL_LIBDIR=lib \
        -DRDKAFKA_BUILD_EXAMPLES=OFF \
        -DRDKAFKA_BUILD_TESTS=OFF \
        -DRDKAFKA_BUILD_STATIC=ON \
        -DWITH_ZLIB=ON \
        -DZLIB_INCLUDE_DIR=/usr/local/include \
        -DZLIB_LIBRARY=/usr/local/lib/libz.so \
        -DWITH_CURL=ON \
        -DCURL_DIR=/usr/local/lib/cmake/CURL \
        -DWITH_SSL=ON \
        -DOPENSSL_USE_STATIC_LIBS=ON \
        -DOPENSSL_ROOT_DIR=/usr/local \
        -DOPENSSL_INCLUDE_DIR="$BORINGSSL_INCLUDE_DIR" \
        -DOPENSSL_SSL_LIBRARY="$BORINGSSL_SSL_LIBRARY" \
        -DOPENSSL_CRYPTO_LIBRARY="$BORINGSSL_CRYPTO_LIBRARY"
    cmake --build "$RDKAFKA_BUILD" -j "$CPU"
    find "$RDKAFKA_BUILD" -name "librdkafka*.a" -exec cp {} "$LIB_DIR/" \;
    cmake --install "$RDKAFKA_BUILD" --prefix /usr/local
    if ! have_all_files "${RDKAFKA_ARTIFACTS[@]}"; then
        echo "ERROR: librdkafka 安装后缺少所需产物。" >&2
        exit 1
    fi
    write_stamp "$RDKAFKA_STAMP_FILE" "$RDKAFKA_EXPECTED_STAMP"
    echo "librdkafka 安装完成 ($RDKAFKA_EXPECTED_STAMP)"
fi

# ── 8. yaml-cpp ─────────────────────────────────────────────────────────────

YAMLCPP_DIR="$REPO_ROOT/third_party/yaml-cpp"
YAMLCPP_BUILD="$YAMLCPP_DIR/.build_linux"
YAMLCPP_STAMP_FILE="/usr/local/share/mmorpg-deps/yaml-cpp-linux.stamp"
YAMLCPP_SOURCE_ID="$(source_identity yaml-cpp "$YAMLCPP_DIR")"
YAMLCPP_EXPECTED_STAMP="yaml-cpp-linux-v1:source=$YAMLCPP_SOURCE_ID"
YAMLCPP_INSTALLED_STAMP="$(read_stamp "$YAMLCPP_STAMP_FILE")"
YAMLCPP_ARTIFACTS=(
    "$LIB_DIR/libyaml-cpp.a"
    /usr/local/lib/libyaml-cpp.a
    /usr/local/include/yaml-cpp/yaml.h
)
if have_all_files "${YAMLCPP_ARTIFACTS[@]}" \
    && [ "$YAMLCPP_INSTALLED_STAMP" = "$YAMLCPP_EXPECTED_STAMP" ]; then
    echo "yaml-cpp 源码 stamp 一致，跳过构建"
else
    echo "=== yaml-cpp 源码变化或安装不完整，重新构建 ==="
    rm -f "$YAMLCPP_STAMP_FILE"
    rm -rf "$YAMLCPP_BUILD"
    cmake -S "$YAMLCPP_DIR" -B "$YAMLCPP_BUILD" \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_INSTALL_PREFIX=/usr/local \
        -DCMAKE_INSTALL_LIBDIR=lib \
        -DYAML_CPP_BUILD_TESTS=OFF \
        -DYAML_CPP_BUILD_TOOLS=OFF \
        -DYAML_BUILD_SHARED_LIBS=OFF
    cmake --build "$YAMLCPP_BUILD" -j "$CPU"
    find "$YAMLCPP_BUILD" -name "libyaml-cpp.a" -exec cp {} "$LIB_DIR/" \;
    cmake --install "$YAMLCPP_BUILD" --prefix /usr/local
    if ! have_all_files "${YAMLCPP_ARTIFACTS[@]}"; then
        echo "ERROR: yaml-cpp 安装后缺少所需产物。" >&2
        exit 1
    fi
    write_stamp "$YAMLCPP_STAMP_FILE" "$YAMLCPP_EXPECTED_STAMP"
    echo "yaml-cpp 安装完成 ($YAMLCPP_EXPECTED_STAMP)"
fi

# ── 9. nghttp2 (HTTP/2 codec core for cpp/libs/engine/muduo_grpc) ──────────
#
# 只编静态核心库(ENABLE_LIB_ONLY):不编 nghttp / nghttpx / h2load / 示例 / 测试 / 文档,
# 也用不到 nghttp2 自己的嵌套子模块(mruby / neverbleed / munit / urlparse)。
# 版本钉 v1.70.0,与 Windows 的 tools/scripts/third_party/build_nghttp2.ps1 是同一提交,
# 升级时两处与 docs/design/grpc-muduo-transport.md §2 一起改。
#
# 构建方式照 zlib 段(上游 CMake、静态、装到 /usr/local),但是否重编按源码 stamp 判断,
# 不只看 libnghttp2.a 在不在:子模块升级后旧归档还在,只看文件会静默沿用旧版本。
#
# BUILD_SHARED_LIBS=OFF 保证 /usr/local/lib 里只有 libnghttp2.a,-lnghttp2 不会落到 .so 上。
# CMAKE_POSITION_INDEPENDENT_CODE=ON:我们自己的库都带 -fPIC,归档也按 PIC 编,
# 链进 PIE 可执行文件时不会报 "recompile with -fPIC"。
# CMAKE_INSTALL_LIBDIR=lib:GNUInstallDirs 在部分发行版上会选 lib64 / 多架构目录,
# 而 vcxproj2cmake.py 生成的 link_directories 只认 /usr/local/lib。

NGHTTP2_DIR="$REPO_ROOT/third_party/nghttp2"
NGHTTP2_BUILD="$NGHTTP2_DIR/.build_linux"
NGHTTP2_STAMP_FILE="/usr/local/share/mmorpg-deps/nghttp2-linux.stamp"
EXPECTED_NGHTTP2_GIT_SHA="85e300c79fb6dbcfa9c1013215c8710c1c2cd3d2"
EXPECTED_NGHTTP2_VERSION="1.70.0"
NGHTTP2_ARTIFACTS=(
    /usr/local/lib/libnghttp2.a
    /usr/local/include/nghttp2/nghttp2.h
    /usr/local/include/nghttp2/nghttp2ver.h
)

if [ ! -f "$NGHTTP2_DIR/CMakeLists.txt" ]; then
    echo "ERROR: nghttp2 source is missing: $NGHTTP2_DIR" >&2
    echo "Run: git submodule update --init third_party/nghttp2" >&2
    exit 1
fi

if is_git_worktree_root "$NGHTTP2_DIR"; then
    ACTUAL_NGHTTP2_GIT_SHA="$(git -C "$NGHTTP2_DIR" rev-parse HEAD)"
    if [ "$ACTUAL_NGHTTP2_GIT_SHA" != "$EXPECTED_NGHTTP2_GIT_SHA" ]; then
        echo "ERROR: unexpected nghttp2 source revision." >&2
        echo "  expected: $EXPECTED_NGHTTP2_GIT_SHA (v$EXPECTED_NGHTTP2_VERSION)" >&2
        echo "  actual  : $ACTUAL_NGHTTP2_GIT_SHA" >&2
        exit 1
    fi
    NGHTTP2_EXPECTED_STAMP="nghttp2-linux-v1:git=$EXPECTED_NGHTTP2_GIT_SHA"
else
    # Docker deps 阶段 COPY 进来的源码树没有可用的 .git,核对不了提交号:
    # 退而核对上游 CMakeLists.txt 声明的版本号,stamp 用内容哈希(与 muduo 段同口径)。
    if ! grep -Fq "project(nghttp2 VERSION $EXPECTED_NGHTTP2_VERSION " "$NGHTTP2_DIR/CMakeLists.txt"; then
        echo "ERROR: nghttp2 source tree at $NGHTTP2_DIR does not declare version $EXPECTED_NGHTTP2_VERSION." >&2
        exit 1
    fi
    NGHTTP2_EXPECTED_STAMP="nghttp2-linux-v1:tree=$(hash_source_tree "$NGHTTP2_DIR")"
fi
NGHTTP2_INSTALLED_STAMP="$(read_stamp "$NGHTTP2_STAMP_FILE")"

if have_all_files "${NGHTTP2_ARTIFACTS[@]}" \
    && [ "$NGHTTP2_INSTALLED_STAMP" = "$NGHTTP2_EXPECTED_STAMP" ]; then
    echo "nghttp2 source stamp matches, skipping"
else
    if have_all_files "${NGHTTP2_ARTIFACTS[@]}"; then
        echo "=== nghttp2 source stamp changed; rebuilding ==="
    else
        echo "=== nghttp2 install incomplete; rebuilding ==="
    fi

    # 与 gRPC 段同理:stamp 失配时整个构建目录重来,不依赖旧目录里的文件时间戳。
    rm -f "$NGHTTP2_STAMP_FILE"
    rm -rf "$NGHTTP2_BUILD"
    mkdir -p "$NGHTTP2_BUILD"
    cmake -S "$NGHTTP2_DIR" -B "$NGHTTP2_BUILD" \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_INSTALL_PREFIX=/usr/local \
        -DCMAKE_INSTALL_LIBDIR=lib \
        -DCMAKE_POSITION_INDEPENDENT_CODE=ON \
        -DENABLE_LIB_ONLY=ON \
        -DBUILD_SHARED_LIBS=OFF \
        -DBUILD_STATIC_LIBS=ON \
        -DENABLE_DOC=OFF \
        -DBUILD_TESTING=OFF \
        -DENABLE_FAILMALLOC=OFF
    cmake --build "$NGHTTP2_BUILD" -j "$CPU"
    cmake --install "$NGHTTP2_BUILD"
    if ! have_all_files "${NGHTTP2_ARTIFACTS[@]}"; then
        echo "ERROR: nghttp2 install completed without all required artifacts." >&2
        exit 1
    fi
    if ! grep -Fq "#define NGHTTP2_VERSION \"$EXPECTED_NGHTTP2_VERSION\"" \
        /usr/local/include/nghttp2/nghttp2ver.h; then
        echo "ERROR: installed nghttp2ver.h does not declare version $EXPECTED_NGHTTP2_VERSION." >&2
        exit 1
    fi
    write_stamp "$NGHTTP2_STAMP_FILE" "$NGHTTP2_EXPECTED_STAMP"
    echo "nghttp2 install ok ($NGHTTP2_EXPECTED_STAMP)"
fi

# ── Done ────────────────────────────────────────────────────────────────────
echo ""
echo "=== All dependencies ready ==="
echo "  Installed to: /usr/local  +  $LIB_DIR"
echo ""
echo "System prerequisites (install with apt-get if missing):"
echo "  build-essential cmake git libssl-dev libboost-dev libpsl-dev pkg-config"
