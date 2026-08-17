#pragma once

// gate 启动版本行(工作包 c)。
//
// 事故复盘最先要回答的问题永远是"线上跑的到底是哪一版"。gate 原来只有
// Node::Initialize() 里 build_info::LogStartupBanner() 那段 git_sha/branch/
// 编译时间,既不含节点身份(node_type / node_id / zone_id),也受日志级别
// 影响;而 Node 的启动 banner 虽然带 node_id / zone_id,却不带版本。这里把
// 六个字段拼成**一行**、直接写 stdout,谁都能一条 grep 捞出来:
//
//     [gate_version] version=... commit=... build_time=... node_type=GATE
//                    node_id=... zone_id=...
//
// 打两次,不是冗余:
//   * main() 第一行 —— node_id 要等 etcd CAS、zone_id 要等 LoadConfigs,这时
//     都还没有,所以填 "pending"。哪怕进程在 etcd 阶段就崩了,版本三元组也
//     已经落盘;
//   * SetAfterStart —— node_id / zone_id 已是终值(见 main.cpp 里那段关于
//     node.cpp:928 的注释),补一条完整六元组。
//
// 版本三元组的来源(逐级回落):
//   * build_time:__DATE__ " " __TIME__,编译器内建,任何构建环境都有;
//   * commit:    编译期宏 BUILD_GIT_SHA(cpp/libs/engine/core/build_info/
//                build_info.h 用的同一个宏),没注入时回落到环境变量
//                GATE_BUILD_COMMIT;
//   * version:   环境变量 GATE_BUILD_VERSION 优先,其次编译期宏
//                GATE_BUILD_VERSION,都没有则 "unknown"。
//
// 为什么把环境变量排在编译期宏前面:cpp/nodes/gate/CMakeLists.txt 是
// tools/archived/vcxproj2cmake.py 从 gate.vcxproj **无条件重新生成**的
// (build_linux.sh 第 2 步),往里手加 -D 会在下一次构建时被抹掉。所以能在
// 部署侧一定生效的通道是环境变量(K8s 用镜像 tag / git sha 注入即可),
// 编译期宏留作构建流水线以后接入的入口。
//
// 本头只依赖标准库,故意不 include muduo —— 版本行必须在日志系统就绪之前
// 就能打,而且不能被 LogLevel 过滤掉。

#include <cstdio>
#include <cstdlib>
#include <string>

#ifndef BUILD_GIT_SHA
#define BUILD_GIT_SHA "unknown"
#endif

namespace gate_version
{

inline constexpr char kUnknown[] = "unknown";
inline constexpr char kVersionEnv[] = "GATE_BUILD_VERSION";
inline constexpr char kCommitEnv[] = "GATE_BUILD_COMMIT";

inline const char *NonEmptyEnv(const char *name)
{
	const char *value = std::getenv(name);
	if (value == nullptr || value[0] == '\0')
	{
		return nullptr;
	}
	return value;
}

inline std::string Version()
{
	if (const char *fromEnv = NonEmptyEnv(kVersionEnv))
	{
		return fromEnv;
	}
#ifdef GATE_BUILD_VERSION
	return GATE_BUILD_VERSION;
#else
	return kUnknown;
#endif
}

inline std::string Commit()
{
	const std::string compiled = BUILD_GIT_SHA;
	if (!compiled.empty() && compiled != kUnknown)
	{
		return compiled;
	}
	if (const char *fromEnv = NonEmptyEnv(kCommitEnv))
	{
		return fromEnv;
	}
	return kUnknown;
}

inline std::string BuildTime()
{
	return std::string(__DATE__) + " " + __TIME__;
}

// nodeId / zoneId 传 nullptr 表示"尚未分配",打成 pending。
inline std::string StartupLine(const char *nodeType,
							   const char *nodeId,
							   const char *zoneId)
{
	std::string line = "[gate_version] version=";
	line += Version();
	line += " commit=";
	line += Commit();
	line += " build_time=";
	line += BuildTime();
	line += " node_type=";
	line += nodeType != nullptr ? nodeType : kUnknown;
	line += " node_id=";
	line += nodeId != nullptr ? nodeId : "pending";
	line += " zone_id=";
	line += zoneId != nullptr ? zoneId : "pending";
	return line;
}

// 直写 stdout 并立刻 flush:这一行必须在 muduo 日志系统起来之前就可见,
// 而且不能被 base_deploy_config 的 LogLevel 过滤掉。
inline void PrintStartupLine(const char *nodeType, const char *nodeId, const char *zoneId)
{
	const std::string line = StartupLine(nodeType, nodeId, zoneId);
	std::fputs(line.c_str(), stdout);
	std::fputc('\n', stdout);
	std::fflush(stdout);
}

} // namespace gate_version
