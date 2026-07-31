#!/bin/bash
#
# build_linux.sh — Linux build for the C++ game nodes (gate + scene).
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
            sed -n '2,40p' "$0"
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
    "cpp/libs/services/scene"
    "cpp/libs/services/gate"
)

# Executables. cpp/libs/services/player is header-only (no .cpp) and
# cpp/libs/engine/muduo_windows is Windows-only -- neither is built here.
EXE_PROJECTS=(
    "cpp/nodes/gate"
    "cpp/nodes/scene"
)

BINARIES=("gate" "scene")

echo "=== build_linux.sh ==="
echo "  repo root  : $REPO_ROOT"
echo "  build type : $BUILD_TYPE"
echo "  jobs       : $JOBS"
echo "  skip deps  : $SKIP_DEPS"
echo "  generate   : $([ "$SKIP_GENERATE" -eq 1 ] && echo no || echo yes)"
echo "  split debug: $SPLIT_DEBUG"
echo ""

run() {
    if [ "$DRY_RUN" -eq 1 ]; then
        echo "[dry-run] $*"
        return 0
    fi
    "$@"
}

buildproject() {
    local dir="$1"
    echo "--- building $dir ---"
    if [ "$DRY_RUN" -eq 1 ]; then
        echo "[dry-run] cmake -S $dir -B $dir/.build -DCMAKE_BUILD_TYPE=$BUILD_TYPE -DCMAKE_PREFIX_PATH=/usr/local"
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
        -DCMAKE_PREFIX_PATH="/usr/local"
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
    run bash "$ARCHIVED_DIR/setup_dependencies.sh"
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
