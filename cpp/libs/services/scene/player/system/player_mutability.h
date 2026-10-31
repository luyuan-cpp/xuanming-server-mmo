#pragma once

// 玩家「此刻能不能被改」的唯一判定(docs/design/player-inbox-service.md §7.5、D12)。
//
// 从 asset_op_system.cpp 的匿名命名空间原样抽出,资产通道与玩家收件箱共用:一条业务知识只有一个来源
// (AGENTS §11.2 DRY)。抽出前后行为零变化:IsPlayerMutable 仍是"三种组件都不在 + 存盘带归属围栏"。
//
// 新增可改判定只改这里。资产通道 Decide 第 5 步为了按原因回不同的 reason(kAssetFrozen / kAssetPlayerNotHere)
// 仍逐条内联同样的判定,改这里时那一处要同改(asset_op_system.cpp 有注释指回本文件)。
//
// 线程:只在 scene loop 线程调用;只读 ECS,不改变实体的组件集合(AGENTS §7.5)。

#include <cstddef>
#include <cstdint>
#include <iterator>

#include "entt/src/entt/entity/entity.hpp"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "proto/common/component/player_comp.pb.h" // UnregisterPlayer
#include "thread_context/ecs_context.h"

namespace player_mutability
{
	// 不可改的原因,按判定顺序排列(先命中先返回)。最后一项固定为 kCount,同时用作名字表长度(AGENTS §11.2)。
	// 名字同时是收件箱 `[PlayerInbox] metric=inbox_deferred reason=` 日志的取值(设计 §9.4),改名就是改指标口径。
	enum class PlayerMutability : uint8_t
	{
		kMutable,
		kFrozen,            // PlayerFrozenComp:归属交接冻结输入
		kTravelHandoff,     // PlayerTravelHandoffComp:交接已挂,存盘会被 SavePlayer 直接跳过
		kExiting,           // UnregisterPlayer:退出存盘在途,退出优先
		kOwnerEpochUnknown, // PlayerOwnerEpochComp 缺失或 epoch == 0:本实体的存盘没有归属围栏
		kCount
	};

	inline constexpr const char *kPlayerMutabilityNames[] = {"mutable", "frozen", "handoff", "exiting", "epoch_zero"};
	static_assert(std::size(kPlayerMutabilityNames) == static_cast<std::size_t>(PlayerMutability::kCount),
				  "名字表与 PlayerMutability 脱节");

	// 日志用;越界回 "?"。返回 const char*:muduo 的 LogStream 没有 string_view 重载。
	inline const char *PlayerMutabilityName(PlayerMutability mutability)
	{
		const auto index = static_cast<std::size_t>(mutability);
		return index < std::size(kPlayerMutabilityNames) ? kPlayerMutabilityNames[index] : "?";
	}

	// 本实体的存盘是否受 owner_epoch 围栏保护(docs/design/guild-phase2/08-save-owner-fence.md §8.2.1)。
	//
	// epoch 由 scene_manager 铸造、随路由事件下发(PlayerOwnerEpochComp)。epoch == 0 是滚动升级的兼容窗口:
	// SavePlayer 走**无守卫**的旧 Save,db 侧按 legacy_zero 放行。此时若记账并上报 durable,
	// 旧节点晚到的退出存盘能原样盖掉这笔改动 —— 帮会已据 durable 记了资金,玩家侧的扣款却被抹掉(04 K1)。
	// 所以资产通道(以及同样"据落盘事实向外确认"的收件箱)在无围栏的实体上不引发任何写入。
	// 组件缺失与 epoch == 0 一律按无围栏处理(fail-closed)。
	inline bool HasFencedOwnership(entt::entity player)
	{
		const auto *ownerEpoch = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player);
		return ownerEpoch != nullptr && ownerEpoch->epoch != 0;
	}

	// 该实体此刻为什么不能被改(不变量 I1 单写者的本地一侧)。
	//
	// **刻意不复用 PlayerLifecycleSystem::IsCrossZoneFrozen**:它只看 PlayerFrozenComp,
	// 漏掉"归属交接已挂但存盘还没落地"的窗口。而 SavePlayer 在
	// `PlayerTravelHandoffComp.requestedAtMs != 0` 时直接跳过写盘并返回 false
	// (player_lifecycle.cpp 的 SavePlayer 开头,日志 "skip: zone travel handoff already
	// requested" 那一支;不写行号,该文件改动频繁)—— 与"脏比较相等、盘上已是最新"共用同一个返回值。
	// 若在交接在途时记账,这个 false 会被误当成"结局已在盘上",把没落地的结局报成 durable,
	// 对端随即终结,而这份内存态马上会随实体销毁一起丢。所以交接一挂上就必须在**记账之前**拦住。
	// UnregisterPlayer(退出存盘在途)同理:退出优先,不接受新改动。
	// 最后一条:本实体的存盘必须带归属围栏(HasFencedOwnership,见上)。
	inline PlayerMutability ClassifyPlayerMutability(entt::entity player)
	{
		const auto &registry = tlsEcs.actorRegistry;
		if (registry.any_of<PlayerFrozenComp>(player))
		{
			return PlayerMutability::kFrozen;
		}
		if (registry.any_of<PlayerTravelHandoffComp>(player))
		{
			return PlayerMutability::kTravelHandoff;
		}
		if (registry.any_of<UnregisterPlayer>(player))
		{
			return PlayerMutability::kExiting;
		}
		if (!HasFencedOwnership(player))
		{
			return PlayerMutability::kOwnerEpochUnknown;
		}
		return PlayerMutability::kMutable;
	}

	// 抽出前的原式是 `!any_of<PlayerFrozenComp, PlayerTravelHandoffComp, UnregisterPlayer>(player) &&
	// HasFencedOwnership(player)`;ClassifyPlayerMutability 只在三种组件都不在且有围栏时回 kMutable,两者等价。
	inline bool IsPlayerMutable(entt::entity player)
	{
		return ClassifyPlayerMutability(player) == PlayerMutability::kMutable;
	}
} // namespace player_mutability
