#!/bin/bash
#
# build_linux.sh — Linux build for the C++ game nodes (gate + scene + battle).
#
# This is the CANONICAL Linux build entry point. `deploy/k8s/Dockerfile.cpp`
# invokes it inside the builder stage, and it can also be run directly on a
# Linux host.
#
# Usage (from repo root):
#   bash tools/scripts/build_linux.sh --release
#   bash tools/scripts/build_linux.sh --skip-deps --relwithdebinfo --split-debug
#
# Flags:
#   --release          CMAKE_BUILD_TYPE=Release (default)
#   --debug            CMAKE_BUILD_TYPE=Debug
#   --relwithdebinfo   CMAKE_BUILD_TYPE=RelWithDebInfo (optimised + debug info)
#   --skip-deps        Do not touch submodules and do not run
#                      setup_dependencies.sh. REQUIRED inside Docker: the
#                      builder stage has no .git (so `git submodule` fails)
#                      and third-party deps were already built in the deps
#                      stage.
#   --skip-generate    Do not regenerate CMakeLists.txt from the .vcxproj files.
#   --split-debug      Extract .debug symbol files into bin/symbols/ and strip
#                      the binaries. Consumed by Dockerfile.cpp's `symbols`
#                      stage (`--target=symbols -o ./debug-symbols`).
#   --jobs N           Parallelism (default: nproc).
#   --dry-run          Print what would run and exit. Lets the flag/order logic
#                      be checked from a non-Linux shell.
#
# 环境变量(发布版本戳,默认关):
#   MMORPG_STAMP_BUILD=1   把 -DBUILD_GIT_SHA=\"<12位sha>\" 追加进每个工程的
#                          CMAKE_CXX_FLAGS(保留已有 flags),gate_version.h /
#                          build_info.h 启动行里的 commit 就来自它。只给发布流水线用。
#   MMORPG_BUILD_COMMIT    要写进去的 12 位小写 sha(可带 -dirty)。不给则取
#                          `git rev-parse --short=12 HEAD`(脏树加 -dirty);
#                          Docker builder 阶段没有 .git,必须显式给。
# 为什么默认关:CMAKE_CXX_FLAGS 是所有编译单元共享的,每次提交都换 sha =
# 开发机增量构建每次全量重编 C++(几十分钟)。日常版本号走镜像 ENV
# (MMORPG_BUILD_* / GATE_BUILD_*),头文件在宏为 unknown 时回落读它。
#
# NOTE on the project list: it lives HERE and nowhere else.
# tools/archived/autogen.sh used to carry its own copy; it now delegates to
# this script so the two cannot drift apart.
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
ARCHIVED_DIR="$REPO_ROOT/tools/archived"
cd "$REPO_ROOT"

BUILD_TYPE="Release"
SKIP_DEPS=0
SKIP_GENERATE=0
SPLIT_DEBUG=0
DRY_RUN=0
JOBS="$(nproc 2>/dev/null || echo 4)"

while [ $# -gt 0 ]; do
    case "$1" in
        --release)        BUILD_TYPE="Release" ;;
        --debug)          BUILD_TYPE="Debug" ;;
        --relwithdebinfo) BUILD_TYPE="RelWithDebInfo" ;;
        --skip-deps)      SKIP_DEPS=1 ;;
        --skip-generate)  SKIP_GENERATE=1 ;;
        --split-debug)    SPLIT_DEBUG=1 ;;
        --dry-run)        DRY_RUN=1 ;;
        --jobs)
            shift
            [ $# -gt 0 ] || { echo "build_linux.sh: --jobs needs a value" >&2; exit 2; }
            JOBS="$1"
            ;;
        --jobs=*)         JOBS="${1#--jobs=}" ;;
        -h|--help)
            # 打印到 `set -euo pipefail` 之前的整段头注释;别写死行号,头注释一加长就截断。
            sed -n '2,/^set -euo pipefail/p' "$0" | sed '$d'
            exit 0
            ;;
        *)
            echo "build_linux.sh: unknown flag '$1' (see --help)" >&2
            exit 2
            ;;
    esac
    shift
done

# Build order matters: every entry links the libs produced by the ones above
# it. Keep this list in sync with tools/archived/vcxproj2cmake.py's project
# list -- that script generates the CMakeLists.txt these directories consume.
LIB_PROJECTS=(
    "third_party"
    "cpp/generated/proto"
    "cpp/generated/rpc"
    "cpp/generated/proto_helpers"
    "cpp/generated/table"
    "cpp/generated/grpc_client"
    "cpp/libs/engine/core"
    "cpp/libs/engine/config"
    "cpp/libs/engine/session"
    "cpp/libs/engine/infra"
    "cpp/libs/engine/thread_context"
    "cpp/libs/modules"
    "cpp/libs/services/battle"
    "cpp/libs/services/scene"
    "cpp/libs/services/gate"
)

# Executables. cpp/libs/services/player is header-only (no .cpp) and
# cpp/libs/engine/muduo_windows is Windows-only -- neither is built here.
EXE_PROJECTS=(
    "cpp/nodes/gate"
    "cpp/nodes/scene"
    "cpp/nodes/battle"
)

BINARIES=("gate" "scene" "battle")

# ── 发布版本戳(MMORPG_STAMP_BUILD,见头注释) ───────────────────────────────
# 显式开关打开却拿不到合法 sha 时直接失败(exit 2):发布流水线要的是"二进制里有
# commit",悄悄退回 unknown 等于开关没生效,而且要到线上看启动日志才发现。
STAMP_BUILD="${MMORPG_STAMP_BUILD:-0}"
STAMP_SHA=""
STAMP_FLAG=""
case "$STAMP_BUILD" in
    0|"") ;;
    1)
        if [ -n "${MMORPG_BUILD_COMMIT:-}" ]; then
            STAMP_SHA="$MMORPG_BUILD_COMMIT"
        elif git -C "$REPO_ROOT" rev-parse --git-dir >/dev/null 2>&1; then
            STAMP_SHA="$(git -C "$REPO_ROOT" rev-parse --short=12 HEAD)"
            if [ -n "$(git -C "$REPO_ROOT" status --porcelain 2>/dev/null)" ]; then
                STAMP_SHA="${STAMP_SHA}-dirty"
            fi
        fi
        if ! printf '%s' "$STAMP_SHA" | grep -Eq '^[0-9a-f]{12}(-dirty)?$'; then
            echo "build_linux.sh: MMORPG_STAMP_BUILD=1 但拿不到合法的 12 位小写 commit(得到 '$STAMP_SHA')。" >&2
            echo "  没有 .git 的环境(如 Docker builder 阶段)必须同时传 MMORPG_BUILD_COMMIT=<12 位 sha>。" >&2
            exit 2
        fi
        # 值里的 \" 是给 make 调起的 /bin/sh 看的:CMake 把 CMAKE_CXX_FLAGS 原样拼进编译命令,
        # shell 剥掉反斜杠后编译器收到 -DBUILD_GIT_SHA="<sha>",宏才是字符串字面量。
        STAMP_FLAG="-DBUILD_GIT_SHA=\\\"${STAMP_SHA}\\\""
        ;;
    *)
        echo "build_linux.sh: MMORPG_STAMP_BUILD 只接受 0 或 1,得到 '$STAMP_BUILD'" >&2
        exit 2
        ;;
esac

echo "=== build_linux.sh ==="
echo "  repo root  : $REPO_ROOT"
echo "  build type : $BUILD_TYPE"
echo "  jobs       : $JOBS"
echo "  skip deps  : $SKIP_DEPS"
echo "  generate   : $([ "$SKIP_GENERATE" -eq 1 ] && echo no || echo yes)"
echo "  split debug: $SPLIT_DEBUG"
echo "  stamp sha  : $([ -n "$STAMP_SHA" ] && echo "$STAMP_SHA" || echo "off(MMORPG_STAMP_BUILD 未开)")"
echo ""

run() {
    if [ "$DRY_RUN" -eq 1 ]; then
        echo "[dry-run] $*"
        return 0
    fi
    "$@"
}

# 算出本工程要传给 cmake 的 CMAKE_CXX_FLAGS;输出到 stdout,返回 1 表示"不传,保持缓存原样"。
#
# 为什么要读 CMakeCache.txt:-DCMAKE_CXX_FLAGS 一旦传过就写进缓存,之后不传也一直生效。
#   * 开戳:以缓存里已有的 flags(首次配置则取 CMake 自己会用的 $CXXFLAGS)为底,先剔掉
#     旧戳再追加新戳 —— 不覆盖别人的 flags,也不会叠出两个 BUILD_GIT_SHA;
#   * 不开戳:只有缓存里残留旧戳时才传"剔掉旧戳后的 flags",否则一个参数都不加。
#     不清的话,在同一构建目录里开过一次戳,之后的日常构建会一直打着那个过期 sha,
#     比 unknown 更误导。
resolve_cxx_flags() {
    local cache="$1/CMakeCache.txt"
    local base="${CXXFLAGS:-}"
    local had_cache=0
    if [ -f "$cache" ]; then
        had_cache=1
        # 类型段不写死 STRING:首次由 -D 传入时 CMake 可能记成 UNINITIALIZED。
        base="$(awk '/^CMAKE_CXX_FLAGS:[A-Z]+=/ { sub(/^CMAKE_CXX_FLAGS:[A-Z]+=/, ""); print; exit }' "$cache")"
    fi
    local stripped
    stripped="$(printf '%s' "$base" | sed -E 's/(^| )-DBUILD_GIT_SHA=[^ ]*//g; s/^ +//; s/ +$//')"
    if [ -n "$STAMP_FLAG" ]; then
        printf '%s' "${stripped:+$stripped }$STAMP_FLAG"
        return 0
    fi
    if [ "$had_cache" -eq 1 ] && [ "$stripped" != "$base" ]; then
        printf '%s' "$stripped"
        return 0
    fi
    return 1
}

buildproject() {
    local dir="$1"
    local flags
    # 可选的 -DCMAKE_CXX_FLAGS 放进位置参数:空的 "$@" 在 set -u 下也安全
    # (空数组 "${arr[@]}" 在 bash < 4.4 会报 unbound variable)。
    set --
    if flags="$(resolve_cxx_flags "$REPO_ROOT/$dir/.build")"; then
        set -- "-DCMAKE_CXX_FLAGS=$flags"
    fi
    echo "--- building $dir ---"
    if [ "$DRY_RUN" -eq 1 ]; then
        local shown=""
        if [ $# -gt 0 ]; then
            shown=" '$1'"
        fi
        # 这一行的格式被 .github/workflows/cpp-build-ci.yml 用 sed 解析工程目录,附加参数只能追加在行尾。
        echo "[dry-run] cmake -S $dir -B $dir/.build -DCMAKE_BUILD_TYPE=$BUILD_TYPE -DCMAKE_PREFIX_PATH=/usr/local$shown"
        echo "[dry-run] cmake --build $dir/.build -j $JOBS"
        return 0
    fi
    if [ ! -f "$REPO_ROOT/$dir/CMakeLists.txt" ]; then
        echo "build_linux.sh: missing $dir/CMakeLists.txt." >&2
        echo "  Run without --skip-generate, or check tools/archived/vcxproj2cmake.py." >&2
        exit 1
    fi
    cmake -S "$REPO_ROOT/$dir" -B "$REPO_ROOT/$dir/.build" \
        -DCMAKE_BUILD_TYPE="$BUILD_TYPE" \
        -DCMAKE_PREFIX_PATH="/usr/local" \
        "$@"
    cmake --build "$REPO_ROOT/$dir/.build" -j "$JOBS"
    echo "--- $dir ok ---"
    echo ""
}

# ── 1. Submodules + third-party dependencies ────────────────────────────────
# Skipped in Docker: the builder stage has no .git, and the deps stage already
# compiled gRPC / muduo / librdkafka / redis / yaml-cpp / zlib into /usr/local.
if [ "$SKIP_DEPS" -eq 0 ]; then
    echo "[1] submodules + third-party dependencies"
    run git submodule update --init --recursive
    run env BUILD_JOBS="$JOBS" bash "$ARCHIVED_DIR/setup_dependencies.sh"
    echo ""
else
    echo "[1] skipped (--skip-deps)"
    echo ""
fi

# ── 2. Generate CMakeLists.txt from the .vcxproj files ──────────────────────
# The generator OVERWRITES each CMakeLists.txt, so never hand-edit them --
# put project-wide flags in vcxproj2cmake.py instead. (That is where
# -DMMORPG_AGONES_CURL=1 lives; losing it would silently build a binary whose
# Agones lifecycle is permanently Disabled.)
if [ "$SKIP_GENERATE" -eq 0 ]; then
    echo "[2] generating CMakeLists.txt from .vcxproj"
    run python3 "$ARCHIVED_DIR/vcxproj2cmake.py"
    echo ""
else
    echo "[2] skipped (--skip-generate)"
    echo ""
fi

# ── 3. Libraries, in dependency order ───────────────────────────────────────
echo "[3] libraries"
for proj in "${LIB_PROJECTS[@]}"; do
    buildproject "$proj"
done

# ── 4. Executables ──────────────────────────────────────────────────────────
echo "[4] executables"
for proj in "${EXE_PROJECTS[@]}"; do
    buildproject "$proj"
done

# ── 5. Split debug symbols ──────────────────────────────────────────────────
# Keep a separate .debug next to a stripped binary so the runtime image stays
# small while crashes remain analysable. Dockerfile.cpp's `symbols` stage
# exports bin/symbols/.
if [ "$SPLIT_DEBUG" -eq 1 ]; then
    echo "[5] splitting debug symbols"
    run mkdir -p "$REPO_ROOT/bin/symbols"
    for bin in "${BINARIES[@]}"; do
        target="$REPO_ROOT/bin/$bin"
        if [ "$DRY_RUN" -eq 1 ]; then
            echo "[dry-run] objcopy --only-keep-debug $target bin/symbols/$bin.debug"
            echo "[dry-run] objcopy --add-gnu-debuglink=... --strip-debug $target"
            continue
        fi
        if [ ! -f "$target" ]; then
            echo "build_linux.sh: expected binary $target not found" >&2
            exit 1
        fi
        objcopy --only-keep-debug "$target" "$REPO_ROOT/bin/symbols/$bin.debug"
        # add-gnu-debuglink must run BEFORE strip-debug drops the section, so
        # gdb can still find the detached symbols afterwards.
        objcopy --add-gnu-debuglink="$REPO_ROOT/bin/symbols/$bin.debug" "$target"
        objcopy --strip-debug --strip-unneeded "$target"
        echo "  $bin -> bin/symbols/$bin.debug ($(stat -c%s "$REPO_ROOT/bin/symbols/$bin.debug" 2>/dev/null || echo '?') bytes)"
    done
    echo ""
fi

# ── 6. Verify the expected artefacts exist ─────────────────────────────────
# Without this the Docker build would fail much later, in the COPY of stage 3,
# with a far less obvious message.
if [ "$DRY_RUN" -eq 0 ]; then
    for bin in "${BINARIES[@]}"; do
        if [ ! -x "$REPO_ROOT/bin/$bin" ]; then
            echo "build_linux.sh: bin/$bin missing or not executable after build" >&2
            exit 1
        fi
    done
fi

echo "========================================"
echo "  BUILD COMPLETE ($BUILD_TYPE)"
for bin in "${BINARIES[@]}"; do
    echo "  $bin : $REPO_ROOT/bin/$bin"
done
if [ "$SPLIT_DEBUG" -eq 1 ]; then
    echo "  symbols : $REPO_ROOT/bin/symbols/"
fi
echo "========================================"
