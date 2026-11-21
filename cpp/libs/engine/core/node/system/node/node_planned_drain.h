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

// Pod 所属的工作负载修订:Deployment 的 pod-template-hash、StatefulSet 的 controller-revision-hash,部署经 downward API 注入。
// 同一个镜像只改配置或 rollout restart 时构建标识不变、修订会变;把它拼进 NodeInfo.build(ComposeBuildLabel),
// 滚动重启时已经换好的那一代 Pod 才分得出来(§4.6)。不设 = 没有修订(Agones Fleet、本机开发),只用构建标识。
inline constexpr const char *kPodRevisionEnvVar = "MMORPG_POD_REVISION";

// 预算上限。K8s 宽限期是秒级整数,一天足够任何一种排空;更大的值多半是手误多写了几个 0。
inline constexpr std::chrono::seconds kMaxBudget{24 * 60 * 60};

// 冲突停机收尾预算(秒):节点失租/冲突后等待在线玩家异步存盘与改派落地的有界上限。
// 不设或空串取缺省 15 秒(与普通停机排空预算 kShutdownDrainBudget 一致);允许运维按集群规模在 5..60 秒内调优。
inline constexpr const char *kConflictDrainBudgetEnvVar = "MMORPG_CONFLICT_DRAIN_BUDGET_SECONDS";
inline constexpr std::chrono::seconds kDefaultConflictDrainBudget{15};
inline constexpr std::chrono::seconds kMinConflictDrainBudget{5};
inline constexpr std::chrono::seconds kMaxConflictDrainBudget{60};

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

// 解析冲突停机收尾预算:未设返回 15 秒;合法值在 [5, 60] 秒内生效;其余取值回退 15 秒并置 valid=false(由调用方打告警)。
inline std::chrono::seconds ParseConflictDrainBudget(const char *raw, bool *valid = nullptr)
{
	const auto parsed = ParseBudget(raw);
	if (parsed.kind == BudgetKind::kUseDefault)
	{
		if (valid != nullptr)
		{
			*valid = true;
		}
		return kDefaultConflictDrainBudget;
	}
	if (parsed.kind == BudgetKind::kSeconds &&
		parsed.seconds >= kMinConflictDrainBudget &&
		parsed.seconds <= kMaxConflictDrainBudget)
	{
		if (valid != nullptr)
		{
			*valid = true;
		}
		return parsed.seconds;
	}
	if (valid != nullptr)
	{
		*valid = false;
	}
	return kDefaultConflictDrainBudget;
}

inline std::chrono::seconds ConflictDrainBudgetFromEnv(bool *valid = nullptr)
{
	return ParseConflictDrainBudget(std::getenv(kConflictDrainBudgetEnvVar), valid);
}

// 构建标识与工作负载修订拼成 NodeInfo.build:两样都有 = "<构建>@<修订>",只有一样 = 那一样,都没有 = 空串。
// 选点的一方只比相等,不解析这个串。纯函数。
inline std::string ComposeBuildLabel(const std::string_view build, const std::string_view revision)
{
	if (revision.empty())
	{
		return std::string(build);
	}
	if (build.empty())
	{
		return std::string(revision);
	}
	std::string label;
	label.reserve(build.size() + 1 + revision.size());
	label.append(build);
	label.push_back('@');
	label.append(revision);
	return label;
}

inline std::string BuildLabelFromEnv()
{
	const char *build = std::getenv(kBuildLabelEnvVar);
	const char *revision = std::getenv(kPodRevisionEnvVar);
	return ComposeBuildLabel(build != nullptr ? build : "", revision != nullptr ? revision : "");
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
