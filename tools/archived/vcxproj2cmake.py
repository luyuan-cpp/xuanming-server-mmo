#!/usr/bin/env python3
# -*- encoding: utf-8 -*-

"""
vcxproj2cmake.py — Generate per-project CMakeLists.txt from *.vcxproj

Run from repo root:
    python3 tools/archived/vcxproj2cmake.py

Current project structure (active nodes: gate + scene + battle):
  - third_party/         → navmesh/hexagon static lib
  - cpp/generated/       → proto, rpc, table, grpc_client, proto_helpers
  - cpp/libs/engine/     → core, config, session, infra, thread_context, muduo_grpc
  - cpp/libs/modules/    → bag, mission, currency, etc.
  - cpp/libs/services/   → battle, scene, gate, player (header-only)
  - cpp/nodes/gate/      → gate executable
  - cpp/nodes/scene/     → scene executable
  - cpp/nodes/battle/    → battle executable

On Linux, muduo comes from third_party/muduo-linux (NOT muduo_windows).
Protobuf、Abseil 与独立 BoringSSL 由 setup_dependencies.sh 安装到 /usr/local。
"""

import xml.dom.minidom
import os
import sys

# Accumulated library names (libs built earlier are linked by later exes)
built_libs = []

# ── Include path transforms (Windows → Linux) ──────────────────────────────
# Paths containing these substrings are removed from Linux builds
EXCLUDE_INCLUDE_PATTERNS = [
    "muduo/contrib/windows",       # Windows-only muduo adapter headers
    "contrib/windows/include",
]

# Substring replacements applied to each include path
INCLUDE_REPLACEMENTS = [
    # On Linux use muduo-linux tree, not the Windows port
    ("third_party/muduo/", "third_party/muduo-linux/"),
    ("third_party/muduo;", "third_party/muduo-linux;"),
]

# Linux 使用已安装的独立依赖头，不引用 Windows 构建目录或 Protobuf 的内部依赖树。
SKIP_INCLUDE_PATTERNS = [
    "third_party/protobuf/src",
    "third_party/protobuf/third_party/utf8_range",
    "third_party/protobuf/install_vs2026_",
    "third_party/abseil-cpp",
    "third_party/boringssl/install_vs2026_",
    "third_party/googletest/install_vs2026_",
    # Linux 只使用独立 BoringSSL，避免源码树中的 OpenSSL 头遮蔽同名安装头。
    "third_party/openssl/include",
    # Windows 上 build_nghttp2.ps1 的安装目录;Linux 的 nghttp2 头由 setup_dependencies.sh
    # 装到 /usr/local/include(下面每个工程都会加这一项)。
    "third_party/nghttp2/install_vs2026_dbg",
]

# ── External link libraries (installed by setup_dependencies.sh) ────────────
# Libraries that have CMake find_package support use CMake target names.
# Others use plain names (resolved via link_directories).
EXTERNAL_LIBS = [
    # 独立 Protobuf 的导出目标同时携带所需 Abseil / utf8_range 依赖。
    "protobuf::libprotobuf",
    # nghttp2 静态核心库(setup_dependencies.sh 第 8 段装到 /usr/local/lib/libnghttp2.a),
    # 供 muduo_grpc 使用;排在 --end-group 之后,组内的 muduo_grpc 引用都能解析到。
    #
    # --exclude-libs:libcurl.so 自己依赖系统的 libnghttp2.so.14(apt 的 libcurl4 带进来的,
    # 版本比我们钉的旧)。不加这一项时,链接器会把可执行文件里静态链入的 nghttp2_* 符号导出到
    # 动态符号表(因为 libcurl.so 引用了它们),运行期 libcurl 会一部分调到我们的 1.70、
    # 一部分调到系统 .so —— 两个版本的内部结构混用。隐藏静态库的符号后各用各的。
    "-Wl,--exclude-libs,libnghttp2.a",
    "nghttp2",
    # muduo (from muduo-linux cmake build, no find_package)
    # muduo_http:C++ 节点的 Prometheus 出口(core/metrics/metrics_http_exporter.cpp)用 muduo 自带的 HttpServer;
    # 它依赖 muduo_net,必须排在 muduo_net 之前。
    "muduo_protobuf_codec", "muduo_hiredis", "muduo_http", "muduo_net", "muduo_base",
    # other third-party (installed to /usr/local/lib)
    "rdkafka++", "rdkafka",
    "yaml-cpp", "hiredis", "curl",
    # 显式使用独立 BoringSSL 静态导出目标，避免混入系统 OpenSSL 的同名库。
    "z", "zstd", "OpenSSL::SSL", "OpenSSL::Crypto",
    # system
    "pthread", "dl",
]

# Abseil cmake imported targets
ABSEIL_TARGETS = [
    "absl::absl_check", "absl::absl_log",
    "absl::algorithm", "absl::base", "absl::bind_front", "absl::bits",
    "absl::btree", "absl::cleanup", "absl::cord", "absl::core_headers",
    "absl::debugging", "absl::die_if_null", "absl::dynamic_annotations",
    "absl::flags", "absl::flags_parse",
    "absl::flat_hash_map", "absl::flat_hash_set",
    "absl::function_ref", "absl::hash", "absl::layout",
    "absl::log_initialize", "absl::log_severity", "absl::memory",
    "absl::node_hash_map", "absl::node_hash_set",
    "absl::optional", "absl::span",
    "absl::status", "absl::statusor", "absl::strings",
    "absl::synchronization", "absl::time",
    "absl::type_traits", "absl::utility", "absl::variant",
]


# ── Helpers ─────────────────────────────────────────────────────────────────

def _relpath_to_root(vcxproj_dir):
    """Compute relative path from vcxproj_dir back to repo root.

    For example, './cpp/libs/engine/core/' → '../../../../'
    """
    clean = vcxproj_dir.strip("./").rstrip("/")
    if not clean:
        return "./"
    depth = len(clean.split("/"))
    return "/".join([".."] * depth) + "/"


def _project_name_from_file(vcxproj_file):
    """Derive project/target name from vcxproj filename (matches MSBuild TargetName)."""
    return os.path.splitext(os.path.basename(vcxproj_file))[0]


# ── Parse ───────────────────────────────────────────────────────────────────

def parse_vcxproj(vcxproj_file):
    """Parse a .vcxproj file, return (source_files, include_dirs)."""
    source_files = []
    include_dirs = []

    xmldoc = xml.dom.minidom.parse(vcxproj_file)
    # 这是构建入口的边界检查，防止历史工程项在下一次导出时重新带回官方 SDK。
    for name in ("AdditionalIncludeDirectories", "AdditionalLibraryDirectories"):
        for item in xmldoc.getElementsByTagName(name):
            if item.firstChild and "third_party/grpc" in item.firstChild.data.replace("\\", "/").lower():
                raise ValueError("工程仍引用官方 gRPC SDK: %s" % vcxproj_file)
            if item.firstChild and "protobuf/third_party/abseil-cpp" in item.firstChild.data.replace("\\", "/").lower():
                raise ValueError("工程仍引用已拆出的嵌套 Abseil 路径: %s" % vcxproj_file)
    for item in xmldoc.getElementsByTagName("AdditionalDependencies"):
        if not item.firstChild:
            continue
        for library in item.firstChild.data.lower().split(";"):
            library = library.strip()
            if (library in {"gpr.lib", "cares.lib", "address_sorting.lib", "re2.lib", "libupb.lib", "libupbd.lib", "utf8_range_lib.lib"}
                    or library.startswith("upb_")
                    or (library.startswith("grpc") and library != "grpc_client.lib")):
                raise ValueError("工程仍链接官方 gRPC 库: %s: %s" % (vcxproj_file, library))
    for root_node in xmldoc.childNodes:
        for sub_node in root_node.childNodes:
            # Source files
            if sub_node.nodeName == "ItemGroup":
                for item in sub_node.childNodes:
                    if item.nodeName == "ClCompile":
                        attrs = item.attributes
                        if "Include" in attrs.keys():
                            path = attrs["Include"].value.replace("\\", "/")
                            if path.endswith((".grpc.pb.cc", ".grpc.pb.cpp")):
                                raise ValueError("工程仍编译官方 gRPC 生成代码: %s" % vcxproj_file)
                            source_files.append(path)

            # Include directories — merge from ALL ItemDefinitionGroups
            # (Debug and Release may have different paths; we need the union)
            if sub_node.nodeName == "ItemDefinitionGroup":
                for group in sub_node.childNodes:
                    for node in group.childNodes:
                        if node.nodeName == "AdditionalIncludeDirectories" and node.firstChild:
                            for inc in node.firstChild.data.split(";"):
                                inc = inc.strip().replace("\\", "/")
                                if inc and not inc.startswith("%") and inc not in include_dirs:
                                    include_dirs.append(inc)

    return source_files, include_dirs


def transform_include_for_linux(inc):
    """Transform a Windows include path for Linux. Returns None to exclude."""
    for pattern in EXCLUDE_INCLUDE_PATTERNS:
        if pattern in inc:
            return None
    for pattern in SKIP_INCLUDE_PATTERNS:
        if pattern in inc:
            return None
    for old, new in INCLUDE_REPLACEMENTS:
        inc = inc.replace(old, new)
    # Handle end-of-string: "third_party/muduo" without trailing / or ;
    if inc.endswith("third_party/muduo"):
        inc = inc[:-len("third_party/muduo")] + "third_party/muduo-linux"
    return inc


# ── Generate ────────────────────────────────────────────────────────────────

def write_cmake(vcxproj_dir, project_name, source_files, include_dirs, target_type):
    """Write a CMakeLists.txt for the given project."""
    root_rel = _relpath_to_root(vcxproj_dir)
    lib_dir = root_rel + "lib"
    bin_dir = root_rel + "bin"

    lines = []
    # Mark the output as generated.
    #
    # These CMakeLists.txt files are checked into the repo AND rewritten from
    # scratch by this script on every Linux build (build_linux.sh runs it unless
    # --skip-generate). Without a banner they look like ordinary hand-maintained
    # files, and a hand edit here disappears silently on the next build.
    #
    # That already bit us once: -DMMORPG_AGONES_CURL=1 was hand-added to
    # cpp/nodes/scene/CMakeLists.txt. It would have been wiped, and because the
    # macro only *enables* the curl transport, the Linux binary would still have
    # linked and started -- with the Agones lifecycle permanently Disabled and
    # every GameServer stuck in Scheduled. Project-wide flags belong in THIS
    # file (see EXTERNAL_LIBS / add_definitions above), never in the output.
    lines.append("# === GENERATED FILE -- DO NOT EDIT ===")
    lines.append("# Produced by tools/archived/vcxproj2cmake.py from %s.vcxproj."
                 % project_name)
    lines.append("# Every Linux build regenerates it; hand edits are lost without warning.")
    lines.append("# Change the generator instead.")
    lines.append("")
    lines.append("cmake_minimum_required(VERSION 3.22)")
    lines.append("project(%s)" % project_name)
    lines.append("")
    lines.append("set(CMAKE_VERBOSE_MAKEFILE ON)")
    # C++23 is the ceiling for the current toolchain, not a conservative choice:
    # deploy/k8s/Dockerfile.cpp builds on `gcc:13`, which implements C++23 as
    # -std=c++2b but only fragments of C++26 (-std=c++2c). Asking for 26 here
    # makes CMake's compiler-feature check fail on that image. Bump the base
    # image to gcc:14+ FIRST, then raise this. MSVC side uses `stdcpplatest`.
    lines.append("set(CMAKE_CXX_STANDARD 23)")
    lines.append("set(CMAKE_CXX_STANDARD_REQUIRED ON)")
    lines.append("")

    # Compile flags
    lines.append('set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -O0 -g -ggdb")')
    lines.append('set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -Wall -Wextra")')
    lines.append('set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -Wno-unused-parameter -Wno-sign-conversion")')
    lines.append('set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -pthread -fPIC")')
    lines.append("")

    # Output paths (use CMAKE_CURRENT_SOURCE_DIR so both in-source and
    # out-of-source builds resolve correctly)
    if target_type == "lib":
        lines.append('set(LIBRARY_OUTPUT_PATH "${CMAKE_CURRENT_SOURCE_DIR}/%s")' % lib_dir)
    else:
        lines.append('set(EXECUTABLE_OUTPUT_PATH "${CMAKE_CURRENT_SOURCE_DIR}/%s")' % bin_dir)
    lines.append("")

    # Definitions
    lines.append("add_definitions(-D__LINUX__)")
    lines.append("add_definitions(-D__linux__)")
    lines.append("add_definitions(-DNOMINMAX)")
    lines.append("add_definitions(-DENTT_ID_TYPE=uint64_t)")
    lines.append("add_definitions(-DABSL_PROPAGATE_CXX_STD=TRUE)")
    # muduo's legacy protobuf logging macros.
    #
    # cpp/libs/engine/core/network/codec/codec.cpp includes
    # muduo/net/protorpc/google-inl.h, and game_channel.h includes
    # muduo/net/protobuf/ProtobufCodecLite.h.  Both still call
    # GOOGLE_CHECK_EQ / GOOGLE_LOG / GOOGLE_DCHECK, which protobuf removed in
    # v22 when it moved to abseil; we build against protobuf 6.31.1.
    #
    # These three aliases are exactly what the Windows build already passes
    # (<PreprocessorDefinitions> in cpp/libs/engine/core/core.vcxproj), so the
    # two platforms stay on one shim.  They are needed here *in addition to*
    # the copy in tools/archived/setup_dependencies.sh: that one only covers
    # muduo's own translation units, this one covers ours.
    lines.append("add_definitions(-DGOOGLE_CHECK_EQ=ABSL_CHECK_EQ)")
    lines.append("add_definitions(-DGOOGLE_LOG=ABSL_LOG)")
    lines.append("add_definitions(-DGOOGLE_DCHECK=ABSL_DCHECK)")
    # Agones lifecycle: enable the libcurl transport on Linux.
    #
    # MUST be emitted here, not only hand-written into a checked-in
    # CMakeLists.txt: write_cmake() overwrites CMakeLists.txt unconditionally,
    # so anything hand-added there is lost on the next generation.
    #
    # Losing this macro does NOT break the build -- that is exactly why it is
    # dangerous. MakeDefaultHttpTransport() would return nullptr and the whole
    # Agones lifecycle would silently degrade to Disabled: the Linux binary
    # links fine, starts fine, and never talks to the SDK sidecar. Every
    # GameServer would stay in Scheduled forever.
    #
    # "curl" is already in EXTERNAL_LIBS so the link side is covered.
    lines.append("add_definitions(-DMMORPG_AGONES_CURL=1)")
    lines.append("")

    # Include directories (transformed for Linux)
    linux_includes = []
    for inc in include_dirs:
        transformed = transform_include_for_linux(inc)
        if transformed:
            linux_includes.append(transformed)
    # 独立 Protobuf、Abseil 与 BoringSSL 的安装头。
    linux_includes.append("/usr/local/include")
    # Common third-party header paths that MSVC propagates from parent projects
    # but CMake does not.  Add them globally so every target can find them.
    for extra in [
        "third_party/spdlog/include",
        "third_party/entt/single_include",
        "third_party/fmt/include",
    ]:
        abs_extra = root_rel + extra
        if abs_extra not in linux_includes:
            linux_includes.append(abs_extra)
    if project_name == "infra" and target_type == "lib":
        # infra 直接编译 librdkafka C++ 包装层，须复用依赖自身 CMake 生成的配置。
        # 上游特设 dummy 目录，使头文件里的 ../config.h 命中 generated/config.h。
        linux_includes.append(root_rel + "third_party/librdkafka/.build_linux/generated/dummy")
    if linux_includes:
        lines.append("include_directories(")
        for inc in sorted(set(linux_includes)):
            lines.append("    %s" % inc)
        lines.append(")")
    lines.append("")

    # Source files
    if source_files:
        lines.append("set(SOURCE_FILES")
        for src in source_files:
            lines.append("    %s" % src)
        lines.append(")")
        lines.append("")

    # Target
    if target_type == "lib":
        if source_files:
            lines.append("add_library(%s STATIC ${SOURCE_FILES})" % project_name)
        else:
            lines.append("# Header-only — no sources")
            lines.append("add_library(%s INTERFACE)" % project_name)
    else:
        # Executable
        # 三个依赖各自安装，构建不再查找官方 gRPC SDK。
        lines.append("find_package(absl CONFIG REQUIRED)")
        lines.append("find_package(protobuf CONFIG REQUIRED)")
        lines.append("find_package(OpenSSL CONFIG REQUIRED PATHS /usr/local/lib/cmake/OpenSSL NO_DEFAULT_PATH)")
        lines.append("")

        # link_directories must come before add_executable
        lines.append('link_directories(')
        lines.append('    "${CMAKE_CURRENT_SOURCE_DIR}/%s"' % lib_dir)
        lines.append('    /usr/local/lib')
        lines.append('    /usr/lib/x86_64-linux-gnu')
        lines.append(')')
        lines.append("")

        lines.append("add_executable(%s ${SOURCE_FILES})" % project_name)
        lines.append("")

        # Link libraries: use -l for internal libs to avoid CMake target
        # self-reference (e.g. gate exe linking gate service lib).
        # Reverse order: GCC linker needs dependents before dependencies.
        # Wrap internal libs in --start-group/--end-group for circular deps.
        link_items = []
        reversed_libs = list(reversed(built_libs))
        link_items.append("-Wl,--start-group")
        for lib in reversed_libs:
            link_items.append("-l%s" % lib)
        link_items.append("-Wl,--end-group")
        link_items += EXTERNAL_LIBS
        link_items += ABSEIL_TARGETS

        lines.append("target_link_libraries(%s" % project_name)
        for item in link_items:
            lines.append("    %s" % item)
        lines.append(")")

    lines.append("")

    # Track library for later exe linking
    if target_type == "lib" and source_files:
        built_libs.append(project_name)

    # Write
    cmake_path = os.path.join(vcxproj_dir, "CMakeLists.txt")
    with open(cmake_path, "w") as f:
        f.write("\n".join(lines) + "\n")
    print("  Generated: %s  (%d sources)" % (cmake_path, len(source_files)))


def generate(vcxproj_file, vcxproj_dir, target_type):
    """Parse vcxproj and generate CMakeLists.txt."""
    if not os.path.exists(vcxproj_file):
        print("  SKIP (not found): %s" % vcxproj_file)
        return None

    project_name = _project_name_from_file(vcxproj_file)
    source_files, include_dirs = parse_vcxproj(vcxproj_file)
    write_cmake(vcxproj_dir, project_name, source_files, include_dirs, target_type)
    return project_name


# ── Main ────────────────────────────────────────────────────────────────────

if __name__ == "__main__":
    print("=== vcxproj2cmake: generating Linux CMakeLists.txt ===")
    print()

    # ── Static libraries (build order matters) ──

    print("[1/16] third_party (navmesh/hexagon)")
    generate("./third_party/third_party.vcxproj", "./third_party/", "lib")

    print("[2/16] generated/proto")
    generate("./cpp/generated/proto/proto.vcxproj", "./cpp/generated/proto/", "lib")

    print("[3/16] generated/rpc")
    generate("./cpp/generated/rpc/rpc.vcxproj", "./cpp/generated/rpc/", "lib")

    print("[4/16] generated/proto_helpers")
    generate("./cpp/generated/proto_helpers/proto_helpers.vcxproj", "./cpp/generated/proto_helpers/", "lib")

    print("[5/16] generated/table")
    generate("./cpp/generated/table/table.vcxproj", "./cpp/generated/table/", "lib")

    print("[6/16] generated/grpc_client")
    generate("./cpp/generated/grpc_client/grpc_client.vcxproj", "./cpp/generated/grpc_client/", "lib")

    print("[7/16] engine/core")
    generate("./cpp/libs/engine/core/core.vcxproj", "./cpp/libs/engine/core/", "lib")

    print("[8/16] engine/config")
    generate("./cpp/libs/engine/config/config.vcxproj", "./cpp/libs/engine/config/", "lib")

    print("[9/16] engine/session")
    generate("./cpp/libs/engine/session/session.vcxproj", "./cpp/libs/engine/session/", "lib")

    print("[10/16] engine/infra")
    generate("./cpp/libs/engine/infra/infra.vcxproj", "./cpp/libs/engine/infra/", "lib")

    print("[11/16] engine/thread_context")
    generate("./cpp/libs/engine/thread_context/thread_context.vcxproj", "./cpp/libs/engine/thread_context/", "lib")

    # muduo_grpc 只依赖 muduo 头与 nghttp2。core(建通道、etcd 传输)与 grpc_client(生成的 Send*)的源码都依赖它,
    # 三个节点都链接它;静态库之间构建顺序无关,放在 engine 库末尾、各节点之前。
    print("[12/16] engine/muduo_grpc")
    generate("./cpp/libs/engine/muduo_grpc/muduo_grpc.vcxproj", "./cpp/libs/engine/muduo_grpc/", "lib")

    print("[13/16] modules")
    generate("./cpp/libs/modules/modules.vcxproj", "./cpp/libs/modules/", "lib")

    print("[14/16] services/battle")
    generate("./cpp/libs/services/battle/battle.vcxproj", "./cpp/libs/services/battle/", "lib")

    print("[15/16] services/scene")
    generate("./cpp/libs/services/scene/scene.vcxproj", "./cpp/libs/services/scene/", "lib")

    print("[16/16] services/gate")
    generate("./cpp/libs/services/gate/gate.vcxproj", "./cpp/libs/services/gate/", "lib")

    # NOTE: services/player is header-only (0 .cpp files) — no CMake needed
    # NOTE: engine/muduo_windows is NOT used on Linux — use muduo-linux instead

    print()

    # ── Executables ──

    print("[exe] nodes/gate")
    generate("./cpp/nodes/gate/gate.vcxproj", "./cpp/nodes/gate/", "exe")

    print("[exe] nodes/scene")
    generate("./cpp/nodes/scene/scene.vcxproj", "./cpp/nodes/scene/", "exe")

    print()
    print("[exe] nodes/battle")
    generate("./cpp/nodes/battle/battle.vcxproj", "./cpp/nodes/battle/", "exe")

    print("=== Done. Run tools/scripts/build_linux.sh to build. ===")
