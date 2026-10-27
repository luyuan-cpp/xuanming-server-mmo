#!/usr/bin/env bash
# 独立构建固定的序列化与 TLS 依赖；不获取或链接官方 gRPC SDK。
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CPU="${BUILD_JOBS:-1}"
# 与 setup_dependencies.sh 的 zlib、消费者和 CMake 导入位置保持同一安装契约。
PREFIX=/usr/local
if [ -n "${SDK_INSTALL_PREFIX:-}" ] && [ "${SDK_INSTALL_PREFIX%/}" != "$PREFIX" ]; then
    echo 'Linux SDK 安装前缀固定为 /usr/local，不支持单独更改。' >&2
    exit 2
fi
[[ "$CPU" =~ ^[1-9][0-9]*$ ]] || { echo 'BUILD_JOBS 必须为正整数' >&2; exit 2; }

# Docker 普通源码目录没有 .git；此时用全量源码内容作 stamp，不能借用宿主 Git 状态。
source_id() {
    local source="$1" expected="$2" actual top
    [ -f "$source/CMakeLists.txt" ] || { echo "独立SDK源码缺失：$source" >&2; return 1; }
    if [ -e "$source/.git" ]; then
        top="$(git -C "$source" rev-parse --show-toplevel)"
        [ "$(cd "$top" && pwd -P)" = "$(cd "$source" && pwd -P)" ] || exit 1
        actual="$(git -C "$source" rev-parse HEAD)"
        [ "$actual" = "$expected" ] || { echo "依赖版本不符：$source $actual" >&2; exit 1; }
    fi
    (cd "$source" && find . -type f ! -name .git ! -path './.git/*' \
        ! -path './.build*/*' ! -path './install_*/*' -print0 | LC_ALL=C sort -z | \
        xargs -0 sha256sum) | sha256sum | awk '{print $1}'
}

build_install() {
    local name="$1" source="$2" expected="$3" artifact="$4"
    shift 4
    local build="$REPO_ROOT/build/third_party/$name-linux" stamp="$PREFIX/share/mmorpg-deps/$name-linux.stamp"
    local identity source_identity recipe_identity upstream='' receipt="$stamp.artifacts.sha256"
    if [ "$name" = protobuf ]; then
        # 固定路径不代表依赖字节不变；上游重建必须使静态消费者失效。
        upstream="$(sha256sum "$PREFIX/share/mmorpg-deps/abseil-linux.stamp" \
            "$PREFIX/share/mmorpg-deps/abseil-linux.stamp.artifacts.sha256" \
            "$PREFIX/share/mmorpg-deps/zlib-linux.stamp" \
            "$PREFIX/lib/libz.so" "$PREFIX/include/zlib.h" | sha256sum | awk '{print $1}')"
    fi
    source_identity="$(source_id "$source" "$expected")"
    recipe_identity="$(sha256sum "${BASH_SOURCE[0]}" | awk '{print $1}')"
    identity="$source_identity:$*:upstream=$upstream:recipe=$recipe_identity"
    if [ -f "$artifact" ] && [ -f "$stamp" ] && [ -f "$receipt" ] \
        && [ "$(cat "$stamp")" = "$identity" ] && sha256sum --check "$receipt" >/dev/null 2>&1; then
        echo "$name 独立源码及构建参数未变，跳过"
        return
    fi
    # 仅清理本脚本固定的仓内构建目录，不清理已安装的其它依赖。
    case "$build" in "$REPO_ROOT/build/third_party/"*) ;; *) exit 2;; esac
    rm -f "$stamp"
    rm -rf "$build"
    cmake -S "$source" -B "$build" -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_CXX_STANDARD=23 -DCMAKE_POSITION_INDEPENDENT_CODE=ON \
        -DCMAKE_INSTALL_PREFIX="$PREFIX" -DCMAKE_INSTALL_LIBDIR=lib "$@"
    if [ "$name" = boringssl ]; then
        cmake --build "$build" --parallel "$CPU" --target ssl crypto bssl
    else
        cmake --build "$build" --parallel "$CPU"
    fi
    cmake --install "$build"
    [ -f "$artifact" ] || { echo "安装产物缺失：$artifact" >&2; exit 1; }
    mkdir -p "$(dirname "$stamp")"
    # 安装清单是上游 CMake 的实际产物全集，缺失或变化时不能复用。
    while IFS= read -r installed || [ -n "$installed" ]; do
        case "$installed" in "$PREFIX/"*) ;; *) echo "SDK 安装路径越界：$installed" >&2; exit 1;; esac
        sha256sum "$installed"
    done < "$build/install_manifest.txt" > "$receipt"
    [ -s "$receipt" ] || { echo 'SDK 安装清单为空' >&2; exit 1; }
    printf '%s\n' "$identity" > "$stamp"
}

build_install abseil "$REPO_ROOT/third_party/abseil-cpp" \
    76bb24329e8bf5f39704eb10d21b9a80befa7c81 "$PREFIX/lib/cmake/absl/abslConfig.cmake" \
    -DBUILD_SHARED_LIBS=OFF -DABSL_ENABLE_INSTALL=ON -DABSL_BUILD_TESTING=OFF -DABSL_PROPAGATE_CXX_STD=ON
build_install protobuf "$REPO_ROOT/third_party/protobuf" \
    35cd01f9fe9afbeea38cc7b979a3b6bfcde82c03 "$PREFIX/bin/protoc" \
    -DCMAKE_PREFIX_PATH="$PREFIX" -Dabsl_DIR="$PREFIX/lib/cmake/absl" \
    -Dprotobuf_BUILD_TESTS=OFF -Dprotobuf_BUILD_SHARED_LIBS=OFF -Dprotobuf_BUILD_PROTOC_BINARIES=ON -Dprotobuf_BUILD_LIBUPB=ON \
    -Dprotobuf_LOCAL_DEPENDENCIES_ONLY=ON -Dprotobuf_WITH_ZLIB=ON \
    -DZLIB_INCLUDE_DIR="$PREFIX/include" -DZLIB_LIBRARY="$PREFIX/lib/libz.so"
build_install boringssl "$REPO_ROOT/third_party/boringssl" \
    2b44a3701a4788e1ef866ddc7f143060a3d196c9 "$PREFIX/lib/libssl.a" \
    -DBUILD_SHARED_LIBS=OFF
for artifact in lib/libprotobuf.a lib/libprotoc.a lib/libutf8_range.a lib/libutf8_validity.a \
    include/google/protobuf/runtime_version.h lib/libssl.a lib/libcrypto.a include/openssl/base.h; do
    [ -f "$PREFIX/$artifact" ] || { echo "独立SDK不完整：$artifact" >&2; exit 1; }
done
"$PREFIX/bin/protoc" --version
