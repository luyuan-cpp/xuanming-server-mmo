#pragma once

// 玩家持久收件箱 scene 侧的类型注册表(docs/design/player-inbox-service.md §3.2)。
//
// 唯一事实源是 proto/player_inbox/player_inbox.proto 的枚举值选项(OptionPlayerInboxDomain = 420001、
// OptionPlayerInboxType = 420002):本文件只用描述符**读**它们,不另抄一份规格。
// 执行器是一张按 PlayerInboxMessageType 平铺的函数表(player_inbox_registry.cpp),
// static_assert(表长 == PlayerInboxMessageType_ARRAYSIZE) 防脱节;不用宏(AGENTS §11.2)。
//
// 新增一种消息类型的完整步骤:proto 里追加枚举值并写全规格 → 在本域执行器里实现 → 在函数表对应下标填上 →
// 单测 PlayerInboxRegistry.EveryMessageTypeHasExecutor 会扫描描述符核对三者一致。
//
// 线程:描述符读取的结果在首次调用时缓存进函数内静态对象(C++11 起线程安全初始化),此后只读。

#include <cstdint>
#include <string>
#include <vector>

#include "entt/src/entt/entity/entity.hpp"

// 执行器给一条条目的结局(设计 §7.6 第 3 步)。最后一项固定为 kCount(AGENTS §11.2)。
enum class PlayerInboxExecKind : uint8_t
{
	kApplied,  // 已生效:记账(reason = 0)
	kRejected, // 确定性拒绝:记账(reason = tip id),不再重试
	kDefer,    // 暂时做不了:在这一条停下(严格顺序),本地退避后重试,不重新 Fetch,不记账
	// 版本错位:本二进制不能安全执行这一条(例如要并进带着本版本不认识字段的欠款)。与未知类型同口径:
	// 停用本域、不记账、不确认这一条,已定前缀照常确认;本实体不再按本地副本续做,停用对本实体一直有效,
	// 实体重建后重拉(退出完成后再登录,或换节点);退出保留期内重登会复用实体,不重拉。
	// 不能用 kDefer:那条路按本地旧副本续做、看不到服务端之后挂的 VOID,而这类错位会在会话内消失
	// (例如那笔欠款被还清删除),续做就会把一条已作废的处置执行掉并报 APPLIED(设计 §7.5 评审 L1)。
	kVersionSkew,
	kCount
};

struct PlayerInboxExecResult
{
	PlayerInboxExecKind kind{PlayerInboxExecKind::kDefer};
	uint32_t reason{0}; // kRejected 时为 tip id;kDefer / kVersionSkew 时只进日志;kApplied 时为 0
};

// 执行器契约:
//   * 只在 scene loop 线程、在同一帧里调用;调用方已过前置闸(可改、非战斗、账本有效)。
//   * 先做零副作用的载荷解码与参数校验,确定性失败回 kRejected;在**任何改动之前**发现缺持久组件回 kDefer
//     (会话内不会自愈,本地续做不会变成成功),发现版本错位回 kVersionSkew(会话内可能消失,必须重拉)。
//   * 不碰账本、不触发存盘、不发请求:记账与确认由 PlayerInboxSystem 统一做。
//   * payload 是 PlayerInboxItem.payload 的原始字节(按类型规格的 payload_message 解码)。
using PlayerInboxExecutorFn = PlayerInboxExecResult (*)(entt::entity player, const std::string &payload);

// 域规格(读 PlayerInboxDomain 的枚举值选项)。
struct PlayerInboxDomainRule
{
	bool known{false};               // 枚举认识这个域,且写了 OptionPlayerInboxDomain
	uint32_t maxPending{0};
	bool requiresOutOfBattle{true};  // 未知域按"要求非战斗"处理(fail-closed)
};

namespace player_inbox_registry
{
	// 未知域 / 没写规格:known = false、requiresOutOfBattle = true。
	PlayerInboxDomainRule DomainRule(uint32_t domain);

	// 本二进制能否在 domain 上执行 msgType:枚举认识、写了类型规格、规格的 domain 等于 domain、执行器非空,
	// 四条全满足才返回执行器,否则返回 nullptr(调用方停在这一条并停用该域,绝不当成 REJECTED,设计 §7.6)。
	PlayerInboxExecutorFn FindExecutor(uint32_t msgType, uint32_t domain);

	// 日志用的类型名(枚举值名);不认识时返回 "unknown"。
	const std::string &TypeName(uint32_t msgType);

	// 本二进制要消费的域:至少有一种类型登记了执行器的那些域,升序。落地钩子按它为每个域建运行时状态。
	const std::vector<uint32_t> &ConsumerDomains();

	// 启动自检:每个非 0 类型都有规格、所属域非 0 且写了域规格、分级是 DURABLE、载荷消息名能在生成池里解析、
	// 执行器非空。返回空串 = 通过;非空串是可直接进日志的原因(PlayerInboxSystem::Initialize 据此停用消费)。
	std::string SelfCheck();
} // namespace player_inbox_registry
