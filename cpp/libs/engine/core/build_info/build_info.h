#pragma once

// Build version info (todo.md #273).
//
// Production debugging often hits the wall "we know which version was
// running but we can't go back to that exact build" — binary plus source
// snapshot in a known state. This header gives every node a consistent
// startup banner and a query API so triage can correlate a crash dump
// with the exact source tree.
//
// Granularity:
//   - `__DATE__` / `__TIME__`  always available (compiler builtins). They
//                              update on every recompile of THIS file.
//   - `BUILD_GIT_SHA`          optional macro, expected to be defined by
//                              the build system as a string literal (e.g.
//                              `-DBUILD_GIT_SHA="abc1234"`). Falls back to
//                              "unknown" if not set, so the header is
//                              usable in any build environment.
//   - `BUILD_BRANCH`           optional, same shape as BUILD_GIT_SHA.
//
// Why a header instead of a generated .cpp: keeping the macros at the
// translation-unit level guarantees that whoever last recompiled the
// node imprinted their own __DATE__/__TIME__. A separate generated .cpp
// can go stale silently if the build graph doesn't depend on it; the
// header pattern can't.
//
// 版本三元组的运行期回落(docs/design/release-packaging-standard-20260914.md P1):
//   * commit:  编译期宏 BUILD_GIT_SHA 优先,为 "unknown" 时回落环境变量
//              MMORPG_BUILD_COMMIT,都没有则 "unknown";
//   * version: 环境变量 MMORPG_BUILD_VERSION,没有则 "unknown"。
// 与 cpp/nodes/gate/gate_version.h 同一口径(commit 宏优先、version 环境变量优先),
// 只是环境变量名不同;deploy/k8s/Dockerfile.runtime / Dockerfile.cpp 的运行阶段
// 两组变量(GATE_BUILD_* 与 MMORPG_BUILD_*)同值写入,两处打印不会互相矛盾。
//
// 为什么宏默认不注入、要靠环境变量:BUILD_GIT_SHA 进的是全局 CMAKE_CXX_FLAGS,
// 每次提交都变 = 开发机增量构建每次全量重编;cpp/nodes/*/CMakeLists.txt 又会被
// vcxproj2cmake.py 重新生成,手加 -D 会被抹掉。所以只有发布流水线显式
// MMORPG_STAMP_BUILD=1 时 tools/scripts/build_linux.sh 才注入宏,日常靠镜像 ENV。
//
// 注意:镜像 ENV 描述的是"打包这个镜像时的仓库 commit"。Dockerfile.runtime 打包的是
// 预先编好的二进制,两者可能不是同一次提交;宏注入过的二进制以宏为准,正是为此
// commit 让宏优先。

#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <ctime>
#include <sstream>
#include <string>

#include <muduo/base/Logging.h>

#ifndef BUILD_GIT_SHA
#define BUILD_GIT_SHA "unknown"
#endif

#ifndef BUILD_BRANCH
#define BUILD_BRANCH "unknown"
#endif

namespace build_info {

inline constexpr char kUnknown[] = "unknown";
inline constexpr char kVersionEnv[] = "MMORPG_BUILD_VERSION";
inline constexpr char kCommitEnv[] = "MMORPG_BUILD_COMMIT";

// 空串与未设置同义:K8s / Dockerfile 里 `value: ""` 很常见,不能把空串当版本号打出去。
// 返回的指针来自 getenv,只在环境未被修改前有效;本头只在启动期读取,不缓存它。
inline const char* NonEmptyEnv(const char* name)
{
    const char* value = std::getenv(name);
    if (value == nullptr || value[0] == '\0')
    {
        return nullptr;
    }
    return value;
}

inline const char* GitSha()
{
    const char* compiled = BUILD_GIT_SHA;
    if (compiled[0] != '\0' && std::strcmp(compiled, kUnknown) != 0)
    {
        return compiled;
    }
    if (const char* fromEnv = NonEmptyEnv(kCommitEnv))
    {
        return fromEnv;
    }
    return kUnknown;
}

inline const char* Version()
{
    if (const char* fromEnv = NonEmptyEnv(kVersionEnv))
    {
        return fromEnv;
    }
    return kUnknown;
}

inline const char* Branch()
{
    return BUILD_BRANCH;
}

inline const char* CompileDate()
{
    return __DATE__;
}

inline const char* CompileTime()
{
    return __TIME__;
}

// One-line summary for log banners and /version-style diagnostic RPCs.
inline std::string AsString()
{
    std::ostringstream oss;
    oss << "version=" << Version()
        << " git=" << GitSha()
        << " branch=" << Branch()
        << " compiled=" << CompileDate() << " " << CompileTime();
    return oss.str();
}

// Multi-line variant for the startup banner: scans well in `journalctl`
// output where 80-col wrapping otherwise mangles the one-line form.
inline std::string AsBanner()
{
    std::ostringstream oss;
    oss << "=== Build Info ===\n"
        << "  version:       " << Version() << "\n"
        << "  git_sha:       " << GitSha() << "\n"
        << "  branch:        " << Branch() << "\n"
        << "  compile_date:  " << CompileDate() << "\n"
        << "  compile_time:  " << CompileTime() << "\n"
        << "==================";
    return oss.str();
}

// Convenience: emit the banner via muduo logging at startup.
inline void LogStartupBanner()
{
    LOG_INFO << "\n" << AsBanner();
}

} // namespace build_info
