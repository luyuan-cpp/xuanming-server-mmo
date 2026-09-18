#pragma once

#include <cstdint>
#include <string>

// 玩家归属三件套 —— 跨 zone 场景传送阶段 1 的 C++ 半边。
//
// 设计:docs/design/cross-zone-scene-travel.md §3 CZ-2..CZ-5、§6 不变量;
//       docs/design/scene-owner-reentry-barrier.md §3.3、§6。
//
// 三个组件都是纯运行时标记:不入 PlayerAllData、不序列化、不跨进程。它们描述的
// 是"这份数据归谁、由谁持有、正在交给谁",而这三件事只由 scene_manager 的
// EnterScene 闸口决定,节点只缓存、只校验、不铸造。
//
// ── Redis 键契约(Go 与 C++ 两边必须一字不差,改任何一边都要同步另一边)──
//   player:{player_id}:owner_epoch  纯十进制整数字符串。只由 scene_manager 用 INCR
//                                    铸造,单调递增。节点存盘时用它做 CAS 守卫。
//   player:{player_id}:handoff      值 "{epoch}:{saved_at_ms}",源 scene 在 Redis 落地
//                                    回调之后写,EX 300 秒。scene_manager 只比对不删除
//                                    (placement 之后它自然过时,靠 TTL 回收)。
//   player:{player_id}:location     PlayerLocation proto 二进制(Go 写),含 owner_epoch。
//
// ── epoch 的 0 语义 ──
//   0 = 未知 / 旧版 Go 未铸造。这是滚动升级的兼容窗口:校验一律跳过并计数
//   (owner_epoch_stats::OwnerEpochUnknown),不许静默。非 0 时"节点缓存值 ==
//   Redis 当前值"才允许写,否则本节点已被废黜。
namespace player_ownership
{
	inline std::string OwnerEpochRedisKey(uint64_t playerId)
	{
		return "player:" + std::to_string(playerId) + ":owner_epoch";
	}

	inline std::string HandoffRedisKey(uint64_t playerId)
	{
		return "player:" + std::to_string(playerId) + ":handoff";
	}

	// 交接标记的值格式 "{epoch}:{saved_at_ms}"。scene_manager 只解析冒号前的 epoch 与当前
	// owner_epoch 比对;saved_at_ms 仅供排障(看"落盘到放行"隔了多久)。
	inline std::string HandoffRedisValue(uint64_t epoch, uint64_t savedAtMs)
	{
		return std::to_string(epoch) + ":" + std::to_string(savedAtMs);
	}

	// 交接标记 TTL。放行后标记已无意义,300s 足够覆盖"源存盘落地 → 客户端重定向 →
	// 目标 zone 落点"的整条链(票据本身只有 5 分钟),过期即自然回收。
	inline constexpr int kHandoffMarkTtlSec = 300;
} // namespace player_ownership

// 玩家归属 zone:数据落库的目的地(DBTask 按它选 topic),与进程所在 zone 无关。
// 值来自 RoutePlayerEvent.home_zone_id → PlayerEnterGameNodeRequest.home_zone_id,
// 建实体时挂上;节点**不得**自己去 data_service 查(CZ-3:两次改派挨近时会读到后一次的值)。
//
// homeZoneId == 0:未知(旧版 gate / scene_manager 未填,或走 player_migrate 老路径建的实体)。
// 存盘时 fail-closed 用进程 zone 并计数 owner_epoch_stats::HomeZoneUnknown + LOG_WARN,
// 绝不静默落错库(不变量 §6.2)。
struct PlayerHomeZoneComp
{
	uint32_t homeZoneId{0};
};

// 本节点持有该玩家时的归属 epoch(scene_manager 铸造,随 RoutePlayerEvent 下发)。
// 存盘时作为 Redis Lua CAS 的期望值,并写进 DBTask.owner_epoch 供 db 服务落库前比对。
//
// epoch == 0:旧版 Go 未铸造(兼容窗口),存盘走无守卫的旧 Save,并计数。
// 重连 / 顶号路径若请求带非 0 epoch,取 max 更新:epoch 单调,Redis 当前值 ≥ 见过的最大值,
// 取 max 能挡住两条乱序到达的路由请求把缓存值倒退、进而把自己误判成被废黜。
struct PlayerOwnerEpochComp
{
	uint64_t epoch{0};
};

// 跨 zone 传送在途(CZ-5 源端释放链):存盘落地 → 写 handoff 标记 → 请求 EnterScene(目标 zone)
// → 应答 Redirect 则销毁本地实体 / 应答错误则解冻回 tip。
//
// 阶段 1 只建机制,不挂它;阶段 2 的 ScenePlayer.TravelToZone handler 校验 CZ-6 后挂上,
// 并同时挂 PlayerFrozenComp 冻结输入(业务系统已按 PlayerFrozenComp 拦写,传送复用同一道闸)。
// 与 player_migrate 老路径(HandleCrossZoneTransfer)互斥:两者都用 PlayerFrozenComp,
// 同一实体上不得同时存在两种在途。
//
// targetZoneId  目标 zone;0 非法(挂组件的一方必须校验目标 zone 存在)。
// sceneConfigId 目标地图配置 id,填进 EnterSceneRequest.scene_conf_id;0 = 让 scene_manager
//               按世界频道表挑大世界(与紧急疏散同款)。
// requestedAtMs 0 = 交接尚未发起(等本次存盘落地);非 0 = handoff 标记已写、EnterScene 已发。
//               发起之后本节点**不得再写**该玩家(SavePlayerToRedis 直接跳过):标记一旦落地,
//               scene_manager 随时可能放行并 INCR epoch,再写只会被 CAS 拒、徒增
//               stale_owner_write_rejected 噪声。也是 EnterScene 应答超时看门狗的代际:
//               看门狗到期时 requestedAtMs 若已变化,说明是另一次传送,不动。
struct PlayerTravelHandoffComp
{
	uint32_t targetZoneId{0};
	uint32_t sceneConfigId{0};
	uint64_t requestedAtMs{0};
};
