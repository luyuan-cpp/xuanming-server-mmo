#pragma once

// 节点计划排空的纯规则(docs/design/scene-node-planned-drain.md §4.1、§4.6):环境变量名、预算解析、每拍的结论。
//
// 计划下线(K8s 给 Pod 发 SIGTERM、Agones 排空标签)先进计划排空阶段:进程照常服务,业务把工作交给别的节点
// (scene 疏散玩家、battle 等局结束),完成或到预算才进普通停机(Node::ShutdownInLoop)。阶段本身在 Node 里
// (Node::SetPlannedDrain / Node::RequestPlannedStop),这里只放不依赖 muduo / protobuf 的部分,供单测直接调用。
//
// 预算由部署注入,与 Pod 的 terminationGracePeriodSeconds 出自同一个参数(宽限期 - 25 秒:停机排空看门狗 15 秒、
// gRPC 两段 GOAWAY 约 3 秒、拆除余量),两者对不上 K8s 会在排空中途强杀。

#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <string>
#include <string_view>

namespace planned_drain
{
// 计划排空预算(秒)。不设 = 用节点自己的缺省值;"0" = 关闭计划排空(SIGTERM 直接停机,应急开关);
// 1..kMaxBudget = 预算;其余取值拒启(Node::SetPlannedDrain)。
inline constexpr const char *kBudgetEnvVar = "MMORPG_PLANNED_DRAIN_BUDGET_SECONDS";

// 部署构建标识:与 Pod 标签 mmorpg.io/build 同一个值,写进 NodeInfo.build,只作选点偏好(§4.6)。
// 不设 = 空串(本机开发),不校验取值:它是不透明的标签值,选点只比相等。
inline constexpr const char *kBuildLabelEnvVar = "MMORPG_BUILD_LABEL";

// 预算上限。K8s 宽限期是秒级整数,一天足够任何一种排空;更大的值多半是手误多写了几个 0。
inline constexpr std::chrono::seconds kMaxBudget{24 * 60 * 60};

// 预算环境变量的解析结论。底层类型按种类数选(4 种),AGENTS.md §11.2。
enum class BudgetKind : uint8_t
{
	kUseDefault, // 没设或空串
	kDisabled,	 // "0"
	kSeconds,	 // 1..kMaxBudget
	kInvalid,	 // 其余:非十进制数字、带符号或空白、超过上限

	kCount
};

struct Budget
{
	BudgetKind kind{BudgetKind::kUseDefault};
	std::chrono::seconds seconds{0}; // 只在 kSeconds 时有意义
};

// 环境变量原文 → 预算。只认纯十进制数字,不认符号、空白与小数:部署脚本只会写整数,别的写法都是配错。
inline Budget ParseBudget(const char *raw)
{
	if (raw == nullptr || raw[0] == '\0')
	{
		return {BudgetKind::kUseDefault, std::chrono::seconds{0}};
	}
	const std::string_view text(raw);
	if (text.size() > 9)
	{
		return {BudgetKind::kInvalid, std::chrono::seconds{0}};
	}
	uint64_t value = 0;
	for (const char c : text)
	{
		if (c < '0' || c > '9')
		{
			return {BudgetKind::kInvalid, std::chrono::seconds{0}};
		}
		value = value * 10 + static_cast<uint64_t>(c - '0');
	}
	if (value == 0)
	{
		return {BudgetKind::kDisabled, std::chrono::seconds{0}};
	}
	if (value > static_cast<uint64_t>(kMaxBudget.count()))
	{
		return {BudgetKind::kInvalid, std::chrono::seconds{0}};
	}
	return {BudgetKind::kSeconds, std::chrono::seconds{static_cast<std::chrono::seconds::rep>(value)}};
}

inline Budget BudgetFromEnv()
{
	return ParseBudget(std::getenv(kBudgetEnvVar));
}

inline std::string BuildLabelFromEnv()
{
	const char *raw = std::getenv(kBuildLabelEnvVar);
	return raw != nullptr ? std::string(raw) : std::string();
}

// 计划排空一拍的结论。
enum class Verdict : uint8_t
{
	kWait,	  // 业务还没交接完,预算未到
	kDrained, // 业务交接完了:进普通停机
	kExpired, // 预算到了,业务还没交接完:进普通停机,剩下的工作走停机路径(scene 是存盘下线)

	kCount
};

// 先看业务是否完成再看是否到期:最后一拍恰好两者同时成立时如实记成完成。now / deadline 用单调时钟(AGENTS.md §11.3)。
inline Verdict Decide(const bool businessDrained, const std::chrono::steady_clock::time_point now,
					  const std::chrono::steady_clock::time_point deadline)
{
	if (businessDrained)
	{
		return Verdict::kDrained;
	}
	return now >= deadline ? Verdict::kExpired : Verdict::kWait;
}

} // namespace planned_drain
