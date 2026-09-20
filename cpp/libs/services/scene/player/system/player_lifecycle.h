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

// 疏散 / 排空的改派票据,定义在 player_lifecycle.cpp(只有那里需要它的字段)。
struct EmergencyRelocateTicket;

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

// 归属交接(跨 zone 传送 / 同 zone 跨节点换图)的成败计数。与 owner_epoch_stats 同一套写法
// (relaxed atomic、不接 Prometheus),由 player_lifecycle.cpp 每 30s 打成一行 [TravelHandoff]
// 日志:key=value 平铺、数值都是进程启动以来的累计值,有变化才打,级别 INFO。
//
// 为什么需要:生产配置(scene_manager AllowUnsafeCrossNodeHandoff=false)下每一次跨节点换图都走
// 这条链,它是常规路径而不是异常路径。只有逐条日志的话,压测时「交接未成率」「冻结时长」无从统计,
// 看门狗反复重挂也只能靠翻 ERROR 日志发现。与 owner_epoch_stats 分开:那三个值应恒 0、非 0 才打
// WARN;这里的值平时就非 0,混进同一行会让那条 WARN 每 30s 必出、失去告警意义。
//
//   started                进入交接(已冻结、已发起存盘)的次数
//   started_cross_zone     其中目标是别的 zone 的(跨 zone 传送);其余是同 zone 跨节点换图
//   granted                放行:归属已交给目标,源实体不存盘销毁
//   granted_without_reply  其中应答丢失 / 失败,靠核实 owner_epoch 或再入路由才知道已放行的。应接近 0
//   resolved_in_place      同 zone 重发后落回本节点,静默解冻(不算失败)
//   aborted                交接未成,解冻并回失败 tip。aborted / started = 交接未成率
//   exit_wins              交接在途时玩家退出,交接作废(退出优先)
//   save_watchdog_fired    交接存盘 30s 没落地,看门狗解冻(同时计入 aborted)
//   reply_watchdog_fired   EnterScene 应答 30s 没到,看门狗去核实归属
//   verify_rearmed         核实归属时 Redis 不可用 / 读失败,保持冻结并重挂看门狗。
//                          持续增长 = 有玩家一直冻着出不来
//   frozen_ms_total        走到终态(granted / resolved_in_place / aborted)的交接累计冻结毫秒数;
//   frozen_ms_max          平均冻结时长 = frozen_ms_total / 三个终态之和,max 是单次最大值
//   withdraw_deferred      作废交接后撤回 handoff 标记没能当场确认(Redis 不通 / 命令没发出去 / 应答丢失或报错),
//                          条目留在待撤回表里重试。趋势类计数:同一条目每失败一次计一次。持续增长 = Redis 不稳
//   withdraw_expired       到截止时刻(标记 TTL + 余量)仍未确认撤回而放弃,或待撤回表满被淘汰。应恒为 0,
//                          非 0 = 有标记只靠 TTL 过期,期间存在回档窗口,要排查(见 handoff_mark_withdraw.h)
//
// started 减去各终态 = 仍在途的 + 交接期间被存盘 CAS 拒而销毁的(后者已计入
// owner_epoch_stats::StaleOwnerWriteRejected)。
namespace travel_handoff_stats
{
	struct Counters
	{
		std::atomic<uint64_t> started{0};
		std::atomic<uint64_t> startedCrossZone{0};
		std::atomic<uint64_t> granted{0};
		std::atomic<uint64_t> grantedWithoutReply{0};
		std::atomic<uint64_t> resolvedInPlace{0};
		std::atomic<uint64_t> aborted{0};
		std::atomic<uint64_t> exitWins{0};
		std::atomic<uint64_t> saveWatchdogFired{0};
		std::atomic<uint64_t> replyWatchdogFired{0};
		std::atomic<uint64_t> verifyRearmed{0};
		std::atomic<uint64_t> frozenMsTotal{0};
		std::atomic<uint64_t> frozenMsMax{0};
		std::atomic<uint64_t> withdrawDeferred{0};
		std::atomic<uint64_t> withdrawExpired{0};
	};

	inline Counters& Get()
	{
		static Counters g_counters;
		return g_counters;
	}

	inline void Inc(std::atomic<uint64_t>& counter) { counter.fetch_add(1, std::memory_order_relaxed); }

	// 记一次走到终态的交接的冻结时长。max 用"读-比-写"而不是 CAS 循环:计数只在 scene 的
	// 逻辑线程上写,relaxed 的含义与本文件其它计数一致(统计值,不参与任何判定)。
	inline void ObserveFrozenMs(uint64_t frozenMs)
	{
		auto& counters = Get();
		counters.frozenMsTotal.fetch_add(frozenMs, std::memory_order_relaxed);
		if (frozenMs > counters.frozenMsMax.load(std::memory_order_relaxed))
		{
			counters.frozenMsMax.store(frozenMs, std::memory_order_relaxed);
		}
	}

	struct Snapshot
	{
		uint64_t started{0};
		uint64_t startedCrossZone{0};
		uint64_t granted{0};
		uint64_t grantedWithoutReply{0};
		uint64_t resolvedInPlace{0};
		uint64_t aborted{0};
		uint64_t exitWins{0};
		uint64_t saveWatchdogFired{0};
		uint64_t replyWatchdogFired{0};
		uint64_t verifyRearmed{0};
		uint64_t frozenMsTotal{0};
		uint64_t frozenMsMax{0};
		uint64_t withdrawDeferred{0};
		uint64_t withdrawExpired{0};

		bool operator==(const Snapshot&) const = default;
	};

	inline Snapshot Read()
	{
		const auto& counters = Get();
		Snapshot snapshot;
		snapshot.started = counters.started.load(std::memory_order_relaxed);
		snapshot.startedCrossZone = counters.startedCrossZone.load(std::memory_order_relaxed);
		snapshot.granted = counters.granted.load(std::memory_order_relaxed);
		snapshot.grantedWithoutReply = counters.grantedWithoutReply.load(std::memory_order_relaxed);
		snapshot.resolvedInPlace = counters.resolvedInPlace.load(std::memory_order_relaxed);
		snapshot.aborted = counters.aborted.load(std::memory_order_relaxed);
		snapshot.exitWins = counters.exitWins.load(std::memory_order_relaxed);
		snapshot.saveWatchdogFired = counters.saveWatchdogFired.load(std::memory_order_relaxed);
		snapshot.replyWatchdogFired = counters.replyWatchdogFired.load(std::memory_order_relaxed);
		snapshot.verifyRearmed = counters.verifyRearmed.load(std::memory_order_relaxed);
		snapshot.frozenMsTotal = counters.frozenMsTotal.load(std::memory_order_relaxed);
		snapshot.frozenMsMax = counters.frozenMsMax.load(std::memory_order_relaxed);
		snapshot.withdrawDeferred = counters.withdrawDeferred.load(std::memory_order_relaxed);
		snapshot.withdrawExpired = counters.withdrawExpired.load(std::memory_order_relaxed);
		return snapshot;
	}
} // namespace travel_handoff_stats

class PlayerLifecycleSystem
{
public:
	static PendingEnterMap& GetPendingEnterMap();

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
	static void HandlePlayerSaveRejected(Guid player_id, const std::string& redisKey, const std::string& rejectedEpoch);

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

	// 该玩家是否处于"归属交接在途"的冻结态(PlayerFrozenComp 存在)。
	// 业务系统(AOI / 战斗 / 货币 / 背包 / 聊天 / 移动)写之前必须查它,为真就跳过:冻结中的实体
	// 虽然 entt 实体还在,但盘上那份才是要交给目标节点的真值,本节点再写就是制造分叉。
	// 名字里的 CrossZone 是历史遗留(最早只服务 player_migrate 搬数据链,该链已按
	// cross-zone-scene-travel.md CZ-1 下线);现在跨 zone 传送与同 zone 跨节点换图都走它。
	// 调用点有二十来处,不为改名去动它们。
	static bool IsCrossZoneFrozen(entt::entity player);

	static entt::entity InitPlayerFromAllData(const PlayerAllData& playerAllData, const PlayerEnterContext& ctx);

	// 返回 true 表示"确实向 Redis / DB 队列压了一次写,稍后会有 HandlePlayerAsyncSaved 回调";
	// 返回 false 表示 proto-compare 快路径判定与上次落盘完全一致、本次不写,
	// 或者该玩家的归属交接已发起(handoff 标记已写,本节点不得再写,见 PlayerTravelHandoffComp)。
	//
	// 返回值必须被消费:退出流程靠 HandlePlayerAsyncSaved 才会清 session / 销毁实体,
	// 快路径跳过时那条回调**永远不会来**,调用方必须自己收尾。
	//
	// 落库目的地由 PlayerHomeZoneComp 决定(CZ-2),Redis 写带 owner_epoch CAS(CZ-4 ①),
	// DBTask 带 owner_epoch(reentry-barrier §6.3)。
	static bool SavePlayerToRedis(entt::entity player);

	// ── 归属交接:源端释放链(CZ-5)──────────────────────────────────────────────
	// 跨 zone 传送与同 zone 跨节点换图共用同一条链,区别只有目标 zone 是不是本 zone。

	// StartTravelHandoff / RequestZoneTravel 的"已受理"返回值。不用 kSuccess:那是 tip 轴上的一个
	// 非 0 码,而这里的返回值会被原样当成 tip id 回给客户端,客户端 / robot 按 id != 0 判拒绝。
	static constexpr uint32_t kTravelAccepted = 0;

	// scene_manager.EnterScene 的"暂拒"码:玩家已有位置记录、这次要换到别的节点,但源 scene 还没为
	// 当前归属代际写出落盘标记。对普通换图来说它不是错误,而是"请先存盘并出示标记再来" ——
	// HandleTravelEnterSceneReply 据此起同 zone 交接。
	// 数值手抄自 go/scene_manager/internal/constants/errors.go 的 ErrHandoffPending:它走的是
	// scene_manager 自己的 error_code 轴(不是 tip 轴),Go 与 C++ 之间没有共享的生成物,
	// 改那边必须同步这里。全 C++ 只此一处定义,别处一律引用它,不许再写字面量 18。
	static constexpr uint32_t kSmErrHandoffPending = 18;

	// 客户端 RPC SceneSceneClientPlayer.TravelToZone 的系统层入口(CZ-7),handler 只委托到这里。
	// 校验 CZ-6(战斗 / 备战在途、组队在途不可传送)与参数,通过后调 StartTravelHandoff。
	// 返回 kTravelAccepted = 已受理(玩家已冻结、存盘已发起),**不代表已到达**:到达 = 客户端随后
	// 收到 RedirectToGate;未成 = 随后收到 SendTipToClient 且已解冻。非 0 = 拒绝的 tip id,未改任何状态。
	// 目标 zone 是否真的存在由 scene_manager 判(C++ 侧没有 zone 表):不存在时走"受理后未成"。
	// sceneConfigId:0 = 由目标 zone 挑默认大世界;非 0 必须是 World 表登记的世界图,否则同步拒绝
	// (kEnterSceneSceneNotFound)。这张图在目标 zone 开没开只有 scene_manager 知道:没开时同样走
	// "受理后未成"(它在放行之前只读检查,未改任何状态)。
	static uint32_t RequestZoneTravel(entt::entity player, uint32_t targetZoneId, uint32_t sceneConfigId);

	// 发起一次归属交接的**唯一入口**:挂 PlayerTravelHandoffComp + PlayerFrozenComp,然后存盘;
	// 落地后由 BeginTravelHandoff 接手。两个调用方:RequestZoneTravel(跨 zone),以及
	// HandleTravelEnterSceneReply 收到 18 时(同 zone 跨节点换图,targetZoneId = 本 zone)。
	// 返回 kTravelAccepted = 已进入交接;非 0 = tip id,且未改任何状态。
	// 自带一份最小校验(实体有效 / 未在退出 / 未在交接 / 不在战斗 / 会话活着):18 到达时离发请求
	// 已经隔了一个往返,玩家可能刚进备战或刚断线,不能只信发请求那一刻的检查。
	static uint32_t StartTravelHandoff(entt::entity player, uint32_t targetZoneId, uint64_t sceneId, uint32_t sceneConfigId);

	// 普通 EnterScene(客户端换图 / 镜像自动进场 / 队伍跟随)的发送侧闸:交接在途,或上一条
	// EnterScene 的应答还没回来(短 TTL 内)时为真,调用方不得再发。理由见 PlayerSceneChangeInFlightComp。
	// 该玩家有一份作废交接留下的 handoff 标记还没确认撤回时同样为真(按 player_id 判,见
	// handoff_mark_withdraw.h):标记可能还带着当前 owner_epoch,这时发跨节点 EnterScene 会免存盘过门。
	static bool IsSceneChangeBusy(entt::entity player);

	// 重试还没确认的 handoff 标记撤回,并丢弃过了截止时刻的条目。由 RedisSystem 驱动:
	//   reconnected = true  → Redis 重连回调里调,忽略重试间隔、全部立刻重发;
	//   reconnected = false → 1s 周期定时器里调,只发到了重试间隔的。
	// 表为空时零开销。没有初始化 RedisSystem 的宿主(单测)只登记、不重试。
	static void RetryPendingHandoffWithdrawals(bool reconnected);

	// 发出普通 EnterScene **之前**调用,记下这次要去哪。应答只回显 player_id,18 到达时靠它起交接。
	// 凡是替在线玩家发普通 EnterScene 的调用点都必须成对调用 IsSceneChangeBusy + 本函数:漏掉的那
	// 一条,它的应答会把别人记下的在途目标摘掉。
	// playerRequested = false:服务器替玩家发的(队伍跟随),被拒只记日志,不起交接、不回 tip
	// (见 PlayerSceneChangeInFlightComp)。
	static void NoteSceneChangeRequested(entt::entity player, uint64_t sceneId, uint32_t sceneConfigId,
										 bool playerRequested = true);

	// 交接是否已经发起(handoff 标记已写、EnterScene 已发)。此后本实体的去留只由 EnterScene 应答与
	// 看门狗裁决:ReleasePlayer 不得把它推进退出流程(见 scene_node_service.cpp HandleReleasePlayer)。
	static bool IsHandoffRequested(entt::entity player);

	// 进场路由(PlayerEnterGameNode)撞上一个交接已发起的旧实体,且路由带来的 owner_epoch 比实体
	// 缓存的新:说明那次交接其实已被放行(应答丢了,看门狗还没到期),玩家在别处玩过一圈又被
	// 派回本节点。旧实体的内存态是交接那一刻的,复用它 = 回档。按"被废黜"销毁(不存盘)并返回
	// true,调用方必须走 AsyncLoad 从盘上重新加载。其余情形返回 false、什么都不做。
	static bool DiscardStaleHandoffEntity(entt::entity player, uint64_t incomingOwnerEpoch);

	// 前置:实体已挂 PlayerTravelHandoffComp(+ PlayerFrozenComp 冻结输入),且**这一刻的
	// 状态已经在 Redis 落地**。两条到达路径:
	//   * SavePlayerToRedis 返回 true  → HandlePlayerAsyncSaved 落地回调自动调本函数;
	//   * SavePlayerToRedis 返回 false → 快路径没写盘(盘上已是同一份),调用方必须自己调。
	// 与退出流程的 FinishExitAfterPersist 是同一种"落地后再收尾"的双路径约定。
	//
	// 动作:异步 SET player:{id}:handoff "{epoch}:{now_ms}" EX 300 → 回调里向 scene_manager
	// 请求 EnterScene(ZoneId=目标, SceneId, SceneConfId) → 应答见 HandleTravelEnterSceneReply。
	// 幂等:requestedAtMs 已非 0(交接已发起)则直接返回,不重复写标记、不重复请求。
	// 失败分支(无 epoch / Redis 断连 / 无 gate 会话 / 无 scene_manager)一律解冻回 tip,不悬挂。
	static void BeginTravelHandoff(Guid playerId);

	// 每一条 EnterScene 应答的入口。scene_manager 在 EnterSceneResponse.player_id 里回显发起玩家,
	// 应答处理方(rpc_replies/scene_manager_response_handler.cpp)对**每一条**应答都会调进来。
	//
	// 第一段 —— 实体上没有交接(PlayerTravelHandoffComp),看 PlayerSceneChangeInFlightComp:
	//   * 没有                          → 疏散等别处发的请求,静默 no-op;
	//   * 有,但 playerRequested=false  → 队伍跟随的应答:摘在途组件,被拒只记日志,到此为止;
	//   * error_code == 18              → 目标场景在别的节点:用记下的目标 StartTravelHandoff(本 zone);
	//   * 其它非 0                      → 换图失败,回 kEnterSceneFailed tip(此前客户端对失败毫无感知);
	//   * 0                             → 同节点换图成功,只摘在途组件。
	// 第二段 —— 交接在途:
	//   * requestedAtMs == 0            → 存盘还没落地,这是交接之前那条请求的迟到应答,忽略;
	//   * error_code != 0               → 不能直接解冻:失败应答不证明 scene_manager 没铸造过 epoch
	//                                     (例如路由发送失败后的回滚本身也可能失败)。走
	//                                     ResolveTravelOutcome 核实后再决定解冻还是销毁;
	//   * 带 redirect                   → 跨 zone 放行,scene_manager 已推进 epoch,本节点不再持有该玩家:
	//                                     与退出同款销毁(摘场景 / 摘会话 / 销毁实体),**不再存盘**;
	//   * 无错无 redirect、目标是本 zone → 同 zone 放行的正常形态。是交给了别的节点还是重发后落回了
	//                                     本节点,应答本身分不出来,走 ResolveTravelOutcome 按 epoch 判;
	//   * 无错无 redirect、目标是别的 zone → 协议异常,LOG_ERROR 后同样走 ResolveTravelOutcome。
	// 实体已不存在(玩家在途中断线,退出优先)或已无交接标记(看门狗先到)时为幂等 no-op。
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

	// "我已被废黜"的统一销毁:不存盘、不改派(疏散票据作废)、摘冻结 / 交接标记、摘场景、摘会话、销毁实体。
	// 入口:存盘被 epoch CAS 拒(HandlePlayerSaveRejected)、交接被放行(EnterScene 应答 Redirect /
	// ResolveTravelOutcome 判出 epoch 已变)、进场路由撞上过期的交接实体(DiscardStaleHandoffEntity)。
	// routine = 这是交接成功的正常收尾,收尾日志打 INFO;否则打 WARN(异常信号,要留证据)。
	static void DestroyDeposedPlayer(Guid playerId, const char *reasonTag, bool routine = false);

	// 撤回本节点写下的 handoff 标记 "{markEpoch}:{requestedAtMs}"(交接作废的两处:"退出优先"与
	// AbortTravelHandoff)。先登记进待撤回表,再发按标记原文比对的条件删除;只有拿到 Redis 的整数应答
	// 才销账。Redis 不通 / 命令没发出去 / 应答丢失或报错:LOG_ERROR + withdraw_deferred,条目留表,由
	// RetryPendingHandoffWithdrawals 重试到标记 TTL 为止;期间 IsSceneChangeBusy 对该玩家返回 true。
	// site 只进日志,标明是哪个撤回点。
	static void WithdrawHandoffMark(Guid playerId, uint64_t markEpoch, uint64_t requestedAtMs, const char *site);

	// 交接未成 **且已确认没有被放行**:摘 PlayerTravelHandoffComp + PlayerFrozenComp、撤回 handoff
	// 标记(WithdrawHandoffMark)、按情形回 tip。只有两种调用方:EnterScene 请求发出之前的失败分支(此时不可能被放行),
	// 以及 ResolveTravelOutcome 核实过的分支。已无交接意图时幂等 no-op。
	//   notifyFailure = true  → 回失败 tip 让客户端收起遮罩:跨 zone 传送未成 kZoneTravelTargetBusy,
	//                           同 zone 换图未成 kEnterSceneFailed(按组件里的 targetZoneId 自己分);
	//   notifyFailure = false → 静默解冻。只用于"同 zone 重发后落回了本节点":换图其实成功了,场景由
	//                           PlayerEnterGameNode → EnterScene 就地切换,再发失败 tip 是误报。
	static void AbortTravelHandoff(Guid playerId, const char *reason, bool notifyFailure = true);

	// EnterScene 请求已经发出之后,凡是"应答本身不足以判定去留"的情形都走这里,先判清楚有没有被放行:
	//   1. DEL player:{id}:handoff —— scene_manager 的铸造 Lua 要求标记原样还在,DEL 之后
	//      不可能再有凭这份标记的放行;
	//   2. GET player:{id}:owner_epoch —— 与缓存值相等 = 归属没动 → AbortTravelHandoff(解冻);
	//      不相等 = 已被放行 → DestroyDeposedPlayer,玩家已在去目标节点 / zone 的路上。
	// 两条命令在同一条 Redis 连接上顺序发出,判定没有竞态窗口。Redis 不可用时保持冻结并重新
	// 挂看门狗:解冻一个可能已被放行的玩家 = 同一名玩家在两处同时活着。
	// requestedAtMs 是交接代际,回调到达时不符即 no-op。
	//   replyWasSuccess = false:应答超时 / 失败应答。归属没动 = 交接未成,回失败 tip。
	//   replyWasSuccess = true :同 zone 放行的成功应答(无 redirect),或进场路由已经把交接中的玩家
	//                            就地放进了新场景(EnterScene 3.2 步,不等应答 —— 应答可能丢)。
	//                            归属没动 = 重发后落回了本节点(同物理节点不铸造 epoch),静默解冻;
	//                            归属已动 = 正常放行。
	//                            这条路也必须经过第 1 步的 DEL:否则标记会带着没变的 epoch 再活 300s,
	//                            玩家解冻后继续产生新状态,下一次跨节点 EnterScene 会凭这份旧标记被
	//                            直接放行,新节点读到的是旧档。
	static void ResolveTravelOutcome(Guid playerId, uint64_t requestedAtMs, const char *reason,
									 bool replyWasSuccess = false);

	// 疏散 / 排空的改派请求本体(EnterScene(zone=本 zone, scene_id=0))。票据按值传入:
	// 调用时本地实体多半已经销毁。
	static void SendEmergencyRelocateEnterScene(Guid playerId, const EmergencyRelocateTicket &ticket);

	// handoff 标记落地后向 scene_manager 请求 EnterScene(目标 zone / 场景)。gate / session 从实体上现取
	// (与 DispatchEmergencyRelocate 不同:交接中实体还活着,不需要提前抄票据)。
	static void RequestTravelEnterScene(Guid playerId);

	// EnterScene 应答看门狗:一次性定时器,到期时若同一代(requestedAtMs 相同)的交接仍在途,
	// 按超时走 ResolveTravelOutcome。只捕获 playerId + 代际,回调里按 id 回查实体(§11.7 精神)。
	static void ArmTravelReplyWatchdog(Guid playerId, uint64_t requestedAtMs);

	// 存盘阶段看门狗:覆盖"已冻结、存盘在途、交接还没发起(requestedAtMs == 0)"这一段 ——
	// 应答看门狗要到 EnterScene 发出才挂,Redis 长时间不可用时这一段没有别的兜底。
	// 到期仍未发起就直接 AbortTravelHandoff(标记没写,不可能已被放行)。代际用 PlayerFrozenComp.frozenAtMs。
	static void ArmTravelSaveWatchdog(Guid playerId, int64_t frozenAtMs);
};
