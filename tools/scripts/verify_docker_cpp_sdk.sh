#!/usr/bin/env bash
# 仅核对一次性只读容器内的独立 Linux SDK 安装戳、归档、头文件和来源一致性。
set -euo pipefail
cd /src

profile="${SDK_BUILD_PROFILE:-debug-compile-only}"
case "$profile" in
    debug-compile-only) zlib_ctest=0 ;;
    validated) zlib_ctest=1 ;;
    *)
        echo "ERROR: unsupported SDK_BUILD_PROFILE: $profile" >&2
        exit 2
        ;;
esac

stamps=/usr/local/share/mmorpg-deps
test ! -e /usr/local/include/grpc
test ! -e /usr/local/include/grpcpp
test ! -e /usr/local/bin/grpc_cpp_plugin
test -z "$(find /usr/local/lib -maxdepth 1 \( -name 'libgrpc*' -o -name 'libgpr*' \) -print -quit)"

for name in abseil protobuf boringssl; do
    test -s "$stamps/$name-linux.stamp"
    test -s "$stamps/$name-linux.stamp.artifacts.sha256"
    sha256sum --check "$stamps/$name-linux.stamp.artifacts.sha256" >/dev/null
done

hash_tree() (
    cd "$1"
    while IFS= read -r -d '' path; do
        path=${path#./}
        printf '%s\0' "$path"
        sha256sum "$path"
    done < <(find . -type f ! -name .git ! -path './.git/*' ! -path './.build_linux/*' -print0 | LC_ALL=C sort -z)
)

tree_id() { hash_tree "$1" | sha256sum | cut -d' ' -f1; }

# 独立 SDK 使用自己的来源摘要格式，保持路径前缀、选项顺序及上游戳一致。
independent_tree_id() (
    cd "$1"
    find . -type f ! -name .git ! -path './.git/*' ! -path './.build*/*' ! -path './install_*/*' -print0 |
        LC_ALL=C sort -z | xargs -0 sha256sum | sha256sum | cut -d' ' -f1
)

sdk_recipe=$(sha256sum /src/tools/scripts/third_party/build_linux_sdk.sh | cut -d' ' -f1)
check_independent() {
    local name="$1" source="$2" upstream="$3"
    shift 3
    test "$(cat "$stamps/$name-linux.stamp")" = "$(independent_tree_id "$source"):$*:upstream=$upstream:recipe=$sdk_recipe"
}

check_independent abseil third_party/abseil-cpp '' \
    -DBUILD_SHARED_LIBS=OFF -DABSL_ENABLE_INSTALL=ON -DABSL_BUILD_TESTING=OFF -DABSL_PROPAGATE_CXX_STD=ON
protobuf_upstream=$(sha256sum "$stamps/abseil-linux.stamp" "$stamps/abseil-linux.stamp.artifacts.sha256" \
    "$stamps/zlib-linux.stamp" /usr/local/lib/libz.so /usr/local/include/zlib.h | sha256sum | cut -d' ' -f1)
check_independent protobuf third_party/protobuf "$protobuf_upstream" \
    -DCMAKE_PREFIX_PATH=/usr/local -Dabsl_DIR=/usr/local/lib/cmake/absl \
    -Dprotobuf_BUILD_TESTS=OFF -Dprotobuf_BUILD_SHARED_LIBS=OFF -Dprotobuf_BUILD_PROTOC_BINARIES=ON -Dprotobuf_BUILD_LIBUPB=ON \
    -Dprotobuf_LOCAL_DEPENDENCIES_ONLY=ON -Dprotobuf_WITH_ZLIB=ON \
    -DZLIB_INCLUDE_DIR=/usr/local/include -DZLIB_LIBRARY=/usr/local/lib/libz.so
check_independent boringssl third_party/boringssl '' -DBUILD_SHARED_LIBS=OFF

overlay=tools/archived/muduo_linux_overlay
muduo=third_party/muduo-linux
overlay_stamp="$muduo/.build_linux/.mmorpg-source.stamp"
test -s "$overlay_stamp"
overlay_count=0
while IFS= read -r -d '' path; do
    cmp "$path" "$muduo/${path#"$overlay/"}"
    overlay_count=$((overlay_count + 1))
done < <(find "$overlay" -type f -print0)
test "$overlay_count" -gt 0
expected_muduo="muduo-linux-v3:source=muduo-linux-tree-v1:$(tree_id "$muduo"):overlay=$(tree_id "$overlay"):boost=boost-headers-tree-v1:$(tree_id third_party/boost/boost)"
test "$(cat "$overlay_stamp")" = "$expected_muduo"

test "$(cat "$stamps/hiredis-linux.stamp")" = "hiredis-linux-v1:source=hiredis-tree-v1:$(tree_id third_party/redis/deps/hiredis)"
zlib_source="zlib-tree-v1:$(tree_id third_party/zlib)"
test "$(cat "$stamps/zlib-linux.stamp")" = "zlib-linux-v2:source=$zlib_source:shared=1:static=1:ctest=$zlib_ctest"
boring_headers=$(tree_id /usr/local/include/openssl)
boring_archives=$(sha256sum /usr/local/lib/libssl.a /usr/local/lib/libcrypto.a | sha256sum | cut -d' ' -f1)
boring_source="boringssl-v2:boringssl=2b44a3701a4788e1ef866ddc7f143060a3d196c9:headers=$boring_headers:archives=$boring_archives"
curl_stamp="curl-linux-v3:source=curl-tree-v1:$(tree_id third_party/curl):zlib=$zlib_source:tls=$boring_source:shared=1:versioned=1:no-undefined=1"
test "$(cat "$stamps/curl-linux.stamp")" = "$curl_stamp"
rejoin_patch=$(sha256sum tools/patches/librdkafka/v2.16.0-cgrp-rejoin-varargs.patch | cut -d' ' -f1)
boring_patch=$(sha256sum tools/patches/librdkafka/v2.16.0-boringssl-compat.patch | cut -d' ' -f1)
rdkafka_stamp="librdkafka-linux-v5:source=librdkafka-tree-v1:$(tree_id third_party/librdkafka):patch=$rejoin_patch:boringssl_patch=$boring_patch:implicit-declarations=error:zlib=$zlib_source:curl=$curl_stamp"
test "$(cat "$stamps/librdkafka-linux.stamp")" = "$rdkafka_stamp"
test "$(cat "$stamps/yaml-cpp-linux.stamp")" = "yaml-cpp-linux-v1:source=yaml-cpp-tree-v1:$(tree_id third_party/yaml-cpp)"
test "$(cat "$stamps/nghttp2-linux.stamp")" = "nghttp2-linux-v1:tree=$(tree_id third_party/nghttp2)"

for name in libz.a libhiredis.a librdkafka.a librdkafka++.a; do
    cmp "/src/lib/$name" "/usr/local/lib/$name"
done
for name in muduo_base muduo_net muduo_protobuf_codec muduo_hiredis muduo_http; do
    test -s "/src/lib/lib$name.a"
done
for name in libz.so libcurl.so libssl.a libcrypto.a libprotobuf.a libprotoc.a libyaml-cpp.a libnghttp2.a; do
    test -s "/usr/local/lib/$name"
done

grep -Eq '^#define OPENSSL_IS_BORINGSSL([[:space:]]|$)' /usr/local/include/openssl/base.h
curl_links=$(ldd /usr/local/lib/libcurl.so)
if grep -Eq 'not found|libssl\.so|libcrypto\.so|libgrpc|libgpr' <<<"$curl_links"; then
    printf '%s\n' "$curl_links"
    exit 1
fi

sha256sum "$stamps"/*.stamp "$stamps"/*.artifacts.sha256 "$overlay_stamp"
sha256sum /src/lib/libmuduo*.a /usr/local/lib/lib{z,hiredis,ssl,crypto,protobuf,protoc,rdkafka,rdkafka++,yaml-cpp,nghttp2}.a /usr/local/lib/libcurl.so
/usr/local/bin/protoc --version
python3 --version
cmake --version
gcc -dumpfullversion
