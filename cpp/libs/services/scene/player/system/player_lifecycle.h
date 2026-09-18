#pragma once
#include <atomic>
#include <cstddef>
#include <cstdint>
#include <string>
#include <unordered_map>
#include "engine/core/type_define/type_define.h"
#include "engine/infra/storage/redis_client/redis_client.h"
#include "proto/common/database/player_cache.pb.h"
#include "proto/common/component/player_async_comp.pb.h"

class ChangeSceneInfoComp;
class PlayerMigrationEvent;
namespace scene_manager { class EnterSceneResponse; }

// 一次 EnterScene 路由决策带给本节点的完整上下文:proto 入场信息 + 归属字段。
//
// homeZoneId / ownerEpoch 来自 PlayerEnterGameNodeRequest(gate 从 RoutePlayerEvent 透传),
// 是"这次路由决策"的值,不是持久化组件,所以不进 PlayerGameNodeEntryInfoComp 这个 proto comp;
// 只在建实体 / 重连时消费一次。0 的含义见 player/comp/player_ownership_comp.h。
struct PlayerEnterContext
{
	PlayerGameNodeEntryInfoComp enterInfo;
	uint32_t homeZoneId{0};
	uint64_t ownerEpoch{0};
};

// Map of player_id → enter-context for players whose Redis load is in flight.
// Replaces the std::any extra_data on MessageAsyncClient.
using PendingEnterMap = std::unordered_map<Guid, PlayerEnterContext>;

// 归属 / epoch 健康计数。与 dirty_save_stats 同一套写法(relaxed atomic,
// 由 RedisSystem 的 30s 快照定时器打成一行 [OwnerEpoch] 日志,任一非 0 才打、级别 WARN),
// 不接 Prometheus 的理由也相同。
//
//   stale_owner_write_rejected  存盘被 Redis epoch CAS 拒绝的次数。压测期应恒 0
//                               (cross-zone-scene-travel.md §6.3);非 0 = 曾出现双主。
//   home_zone_unknown           存盘时 PlayerHomeZoneComp 为 0、被迫 fail-closed 落进程 zone
//                               的次数。非 0 = 路由链上有一环没填 home_zone_id。
//   owner_epoch_unknown         存盘时 epoch 为 0、跳过 CAS 的次数。滚动升级窗口内允许非 0,
//                               全链升级完成后应恒 0,否则等于假防护。
namespace owner_epoch_stats
{
	inline std::atomic<uint64_t>& StaleOwnerWriteRejected()
	{
		static std::atomic<uint64_t> g_counter{0};
		return g_counter;
	}

	inline std::atomic<uint64_t>& HomeZoneUnknown()
	{
		static std::atomic<uint64_t> g_counter{0};
		return g_counter;
	}

	inline std::atomic<uint64_t>& OwnerEpochUnknown()
	{
		static std::atomic<uint64_t> g_counter{0};
		return g_counter;
	}

	inline void IncStaleOwnerWriteRejected() { StaleOwnerWriteRejected().fetch_add(1, std::memory_order_relaxed); }
	inline void IncHomeZoneUnknown() { HomeZoneUnknown().fetch_add(1, std::memory_order_relaxed); }
	inline void IncOwnerEpochUnknown() { OwnerEpochUnknown().fetch_add(1, std::memory_order_relaxed); }

	struct Snapshot
	{
		uint64_t staleOwnerWriteRejected;
		uint64_t homeZoneUnknown;
		uint64_t ownerEpochUnknown;

		bool Any() const { return staleOwnerWriteRejected + homeZoneUnknown + ownerEpochUnknown > 0; }
	};

	inline Snapshot Read()
	{
		return Snapshot{
			StaleOwnerWriteRejected().load(std::memory_order_relaxed),
			HomeZoneUnknown().load(std::memory_order_relaxed),
			OwnerEpochUnknown().load(std::memory_order_relaxed)};
	}
} // namespace owner_epoch_stats

class PlayerLifecycleSystem
{
public:
	static PendingEnterMap& GetPendingEnterMap();

	// 传送交接的 EnterScene 请求随 gRPC metadata 带上发起玩家 id(值为十进制字符串;生成的
	// 客户端会先 Base64 再发)。EnterSceneResponse 里没有 player_id,应答处理方靠 scene_manager
	// 在响应 header 里回显这个键来定位玩家(与 id_segment_bootstrap 的 x-idseg-seq 同款接口)。
	static constexpr char kTravelPlayerIdMetaKey[] = "x-travel-player-id";

	static void HandlePlayerAsyncLoaded(Guid player_id, const PlayerAllData& message);
	static void HandlePlayerAsyncLoadFailed(Guid player_id,
											MessageAsyncClient<Guid, PlayerAllData>::LoadFailureReason reason);
	static void HandlePlayerAsyncSaved(Guid player_id, PlayerAllData& message);

	// 存盘被 Redis 侧 owner_epoch CAS 拒绝:本节点缓存的 epoch 已不是 Redis 当前值,
	// 说明 scene_manager 已把该玩家改派给别的节点 —— 我已被废黜。
	//
	// 处理是终态的:计数 stale_owner_write_rejected、LOG_ERROR,然后销毁本地实体、清
	// playerList / session 映射。**不再存盘、不发 relocate**:再存只会再被拒,改派也走
	// EnterScene、会拿着旧 epoch 撞门。同一次存盘并行发出的 DBTask 由 db 服务按
	// DBTask.owner_epoch 独立拒绝(reentry-barrier §6.3),这里不用管。
	// 由 MessageAsyncClient::SetSaveRejectedCallback 接线(core/system/redis.cpp)。
	static void HandlePlayerSaveRejected(Guid player_id, const std::string& redisKey);

	// 存盘连续失败达到告警阈值后的通知。底层仍保留最新 payload 继续重试。
	//
	// 必须存在的理由:退出流程在 SavePlayerToRedis 返回 true 后,把"销毁实体、
	// 清 session、清 playerList"整段收尾都挂在 HandlePlayerAsyncSaved 上。
	// 旧实现达到上限后丢弃最新值且不通知调用方;调用方既无法告警,也无法
	// 区分"仍在重试"与"已经没有任何最新副本"。现在回调只用于高危告警,
	// **绝不**更新 PlayerLastPersistedSnapshotComp,也不在失败点销毁唯一内存态。
	// 详见 docs/design/player-async-save-loss-windows.md §4.1。
	static void HandlePlayerAsyncSaveFailed(Guid player_id, const std::string& redisKey, int retryCount);

	// ctx.homeZoneId / ctx.ownerEpoch 非 0 时更新实体上的归属组件(重连 / 顶号复用实体的路径
	// 靠这里拿到 Go 给的当前 epoch;首登路径 InitPlayerFromAllData 先挂零值组件再经这里赋值)。
	static void EnterScene(const entt::entity player, const PlayerEnterContext& ctx);
	static void HandleBindPlayerToGateOK(entt::entity player);
	static void RemovePlayerSession(Guid player_id);
	static void RemovePlayerSession(entt::entity player);
	static void RemovePlayerSessionSilently(Guid playerId);
	static void DestroyPlayer(Guid player_id);
	static void HandleExitGameNode(entt::entity player);
	static void HandleCrossZoneTransfer(entt::entity playerEntity);
	static void HandlePlayerMigration(const PlayerMigrationEvent& msg);

	// Cross-zone migration ACK handler.
	//
	// Called when this node receives a `player_migrate_ack` Kafka message
	// confirming the destination zone successfully loaded the migrating
	// player. Removes PlayerFrozenComp and finally calls DestroyPlayer to
	// release the source-side entity.
	//
	// If the matching Frozen entity is not present (player already destroyed
	// by reaper / manual intervention / restart recovery), this is a no-op
	// — duplicate ACKs are idempotent.
	//
	// See docs/design/cross-zone-readiness-audit.md §3.2 件 3 for the full
	// ACK protocol and §7 for failure handling. The Kafka topic plumbing
	// is task #25 (still pending — this stub only handles the "ACK arrived"
	// half of the protocol).
	static void HandlePlayerMigrationAck(Guid player_id, uint32_t to_zone_id);

	// True iff this player is currently frozen for cross-zone migration.
	// Business systems (AOI / combat / currency / bag / chat / movement)
	// MUST check this and skip writes when it returns true — Frozen entities
	// are conceptually "not on this node anymore" even though the entt entity
	// still exists. See cross-zone-readiness-audit.md §3.2 件 2.
	static bool IsCrossZoneFrozen(entt::entity player);

	static entt::entity InitPlayerFromAllData(const PlayerAllData& playerAllData, const PlayerEnterContext& ctx);

	// 返回 true 表示"确实向 Redis / DB 队列压了一次写,稍后会有 HandlePlayerAsyncSaved 回调";
	// 返回 false 表示 proto-compare 快路径判定与上次落盘完全一致、本次不写,
	// 或者该玩家的跨 zone 交接已发起(handoff 标记已写,本节点不得再写,见 PlayerTravelHandoffComp)。
	//
	// 返回值必须被消费:退出流程靠 HandlePlayerAsyncSaved 才会清 session / 销毁实体,
	// 快路径跳过时那条回调**永远不会来**,调用方必须自己收尾。
	//
	// 落库目的地由 PlayerHomeZoneComp 决定(CZ-2),Redis 写带 owner_epoch CAS(CZ-4 ①),
	// DBTask 带 owner_epoch(reentry-barrier §6.3)。
	static bool SavePlayerToRedis(entt::entity player);

	// ── 跨 zone 传送:源端释放链(CZ-5)──────────────────────────────────────────
	// 前置:实体已挂 PlayerTravelHandoffComp(+ PlayerFrozenComp 冻结输入),且**这一刻的
	// 状态已经在 Redis 落地**。两条到达路径:
	//   * SavePlayerToRedis 返回 true  → HandlePlayerAsyncSaved 落地回调自动调本函数;
	//   * SavePlayerToRedis 返回 false → 快路径没写盘(盘上已是同一份),调用方必须自己调。
	// 与退出流程的 FinishExitAfterPersist 是同一种"落地后再收尾"的双路径约定。
	//
	// 动作:异步 SET player:{id}:handoff "{epoch}:{now_ms}" EX 300 → 回调里向 scene_manager
	// 请求 EnterScene(ZoneId=目标, SceneId=0, SceneConfId) → 应答见 HandleTravelEnterSceneReply。
	// 幂等:requestedAtMs 已非 0(交接已发起)则直接返回,不重复写标记、不重复请求。
	// 失败分支(无 epoch / Redis 断连 / 无 gate 会话 / 无 scene_manager)一律解冻回 tip,不悬挂。
	static void BeginTravelHandoff(Guid playerId);

	// 传送交接的 EnterScene 应答。由 scene_manager 应答处理方按 kTravelPlayerIdMetaKey 回显
	// (或 Redirect 票据 GateTokenPayload.player_id)定位到玩家后调用。
	//   * error_code != 0             → 目标 zone 不可用等:移除传送标记、解冻、回 tip,玩家留在本节点;
	//   * 带 redirect                 → scene_manager 已放行并推进 epoch,本节点不再持有该玩家:
	//                                   与退出同款销毁(摘场景 / 摘会话 / 销毁实体),**不再存盘**。
	//   * 既无错误也无 redirect       → 协议异常,按失败处理并 LOG_ERROR。
	// 实体已不存在(玩家在途中断线,退出优先)或已无传送标记(看门狗先到)时为幂等 no-op。
	static void HandleTravelEnterSceneReply(Guid playerId, const ::scene_manager::EnterSceneResponse& resp);

	// ── 节点身份冲突时的紧急疏散 ────────────────────────────────────────────
	// 本节点的 etcd 身份失效(租约过期 / node_id 被别人抢走)时调用。
	// 步骤固定为「先抄会话 -> 存盘 -> 存盘落地后再改派」:
	//   * 会话信息必须在实体销毁前抄下来,否则改派时拿不到 gate / session;
	//   * 改派必须等存盘落地,否则新 scene node 会从 Redis 读到存盘前的旧数据(回档)。
	// 改派本身走 SceneManager.EnterScene(scene_id=0 / scene_conf_id=0),
	// 由 SceneManager 按世界频道表选一个存活节点上的大世界频道 ——
	// 副本节点挂掉和大世界节点挂掉走同一条路径,落点都是大世界。
	static void BeginEmergencyRelocateAll();

	// ── 单场景排空(世界频道缩容)────────────────────────────────────────
	// 把某个场景里**残留的**玩家全部改派到大世界,走的是和整节点疏散
	// 完全相同的一条路径:存盘 -> 落地后请求 SceneManager.EnterScene(0,0)
	// -> 摘会话 -> 销毁本地实体。
	//
	// 与 BeginEmergencyRelocateAll 的区别只有两点:
	//   1. 范围是一个场景,不是整个节点;
	//   2. **不**设 tlsEmergencyRelocating —— 节点自己不退出,还要继续服务
	//      别的场景。把它标成"疏散中"会让 Node 的 drain 看门狗误判。
	//
	// 返回登记了改派票据的玩家数(已断线的玩家只存盘、不计入)。
	// 调用方不应假设返回后玩家已经走完:改派是异步的,要靠"下一拍再看
	// 场景空没空"来收敛。
	static std::size_t BeginSceneDrain(entt::entity sceneEntity);

	// 疏散是否收敛:所有玩家都存盘完成、改派请求都已派发、本地实体都已销毁。
	// 供 Node 的有界 drain 看门狗轮询。
	static bool IsEmergencyRelocateDrained();

	// True iff this player has an in-flight HandleExitGameNode → SavePlayerToRedis
	// cycle that has not yet seen its HandlePlayerAsyncSaved completion. Detected
	// by the presence of the UnregisterPlayer ECS marker on the player's entity.
	//
	// See todo.md #280 / NOTES Part 2 P0. Same-node reconnect is already handled
	// by EnterScene clearing the marker; cross-node reconnect is gated by the
	// player_locator 30s lease. This method exposes the saving state for callers
	// that want to log / reject / wait when the save outruns those mechanisms.
	//
	// THREADING (Review O3, 2026-05-17): MUST be called from the ECS thread
	// only. Internally does tlsEcs.GetPlayer(playerId) +
	// tlsEcs.actorRegistry.any_of<UnregisterPlayer>(...), both of which touch
	// entt's registry. entt is NOT thread-safe; calling this from a Kafka
	// consumer thread, a gRPC handler thread, or a GM tool background thread
	// is a data race against any ECS mutation happening on the loop thread.
	// If you need cross-thread access, post a task to the EventLoop and read
	// the result back via a future / channel.
	static bool IsSaveInFlight(Guid playerId);

private:
	// 存盘确实落地之后的统一收尾:紧急疏散改派(如果在疏散中)-> 摘会话 -> 销毁实体。
	// 异步存盘回调与"快路径没写盘"两条路径都走这里,避免两份收尾逻辑漂移。
	static void FinishExitAfterPersist(Guid playerId);

	// 向 SceneManager 请求把该玩家改派到大世界频道。只在疏散票据存在时生效,
	// 消费即删除(幂等)。
	static void DispatchEmergencyRelocate(Guid playerId);

	// 抄改派票据 + 推进退出流程。整节点疏散与单场景排空共用。
	// 返回 true 表示登记了票据(玩家有活着的 gate 会话)。
	static bool EnqueueRelocateTicket(entt::entity playerEntity, const char *reasonTag);

	// 把玩家从所在场景摘掉:BeforeLeaveScene(AOI / crowd)→ ScenePlayers 摘除 → 摘 SceneEntityComp + Hex。
	// 退出流程与"被废黜"销毁共用;不在场景里则 no-op。
	static void DetachFromScene(entt::entity player);

	// "我已被废黜"的统一销毁:不存盘、不改派(疏散票据作废)、摘冻结 / 传送标记、摘场景、摘会话、销毁实体。
	// 两个入口:存盘被 epoch CAS 拒(HandlePlayerSaveRejected)、传送交接被放行(EnterScene 应答 Redirect)。
	static void DestroyDeposedPlayer(Guid playerId, const char *reasonTag);

	// 传送失败 / 超时:摘 PlayerTravelHandoffComp + PlayerFrozenComp、best-effort 删 handoff 标记、回 tip。
	// 已无传送意图(应答与看门狗只有一个能赢)时幂等 no-op。
	static void AbortTravelHandoff(Guid playerId, const char *reason);

	// handoff 标记落地后向 scene_manager 请求 EnterScene(目标 zone)。gate / session 从实体上现取
	// (与 DispatchEmergencyRelocate 不同:传送中实体还活着,不需要提前抄票据)。
	static void RequestTravelEnterScene(Guid playerId);

	// EnterScene 应答看门狗:一次性定时器,到期时若同一代(requestedAtMs 相同)的传送仍在途,
	// 按超时 AbortTravelHandoff。只捕获 playerId + 代际,回调里按 id 回查实体(§11.7 精神)。
	static void ArmTravelReplyWatchdog(Guid playerId, uint64_t requestedAtMs);
};
