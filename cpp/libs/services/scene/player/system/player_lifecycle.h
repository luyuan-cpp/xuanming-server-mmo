#pragma once
#include <array>
#include <atomic>
#include <chrono>
#include <functional>
#include <iterator>
#include <memory>
#include <string>
#include <unordered_map>
#include "engine/core/type_define/type_define.h"
#include "engine/infra/storage/redis_client/redis_client.h"
#include "proto/common/database/player_cache.pb.h"
#include "proto/common/component/player_async_comp.pb.h"
// ExitCause:HandleExitGameNode 的参数。写全路径:本头也被 modules 库(bag_service / currency_system)包含,
// 那个工程的包含目录里只有 cpp/libs,没有 cpp/libs/services/scene。
#include "services/scene/player/system/player_exit_intent.h"
#include "services/scene/player/system/exit_release_mark.h"
// 冻结硬上限 / 晚发闸的常量与纯判定(EnforceTravelFreezeCaps 等的参数类型)。写全路径,理由同上。
#include "services/scene/player/system/travel_freeze_cap.h"

namespace scene_manager { class EnterSceneRequest; class EnterSceneResponse; }
namespace storage { class PlayerLocation; }
// 按值传给 PlayerLifecycleSystem::HandleSceneChangeEnterSceneReply;定义在 player/comp/player_ownership_comp.h。
// 只前置声明:函数声明里的按值参数不要求完整类型,定义与调用都在 player_lifecycle.cpp(那里包含了组件头)。
struct PlayerSceneChangeInFlightComp;

// player:{id}:location 的解析(键名见 player_ownership_comp.h 的 LocationRedisKey)。全 C++ 只此一份,
// 读 location 的地方(ResolveTravelOutcome、队伍跟随)一律调它,不许再各抄一份。
// 放在这里而不是 player_ownership_comp.h:那个头被 player_frozen_comp.h 带进二十来个业务系统,
// 不该让它们都去包含 hiredis 与 storage.pb.h;本头本来就包含 redis_client.h(redisReply 已完整)。
namespace player_ownership
{
	// 解析一条 Redis 应答元素(GET 的应答,或 MGET 数组里的一项)里的 PlayerLocation 二进制。
	// NIL(键不存在)/ 非字符串 / 超长 / 解析失败都返回 false;返回 false 时 out 的内容不可用。
	bool ParsePlayerLocationElement(const redisReply *element, storage::PlayerLocation &out);
} // namespace player_ownership

// 一次 EnterScene 路由决策带给本节点的完整上下文:proto 入场信息 + 归属字段。
//
// homeZoneId / ownerEpoch 来自 PlayerEnterGameNodeRequest(gate 从 RoutePlayerEvent 透传),
// 是"这次路由决策"的值,不是持久化组件,所以不进 PlayerGameNodeEntryInfoComp 这个 proto comp;
// 只在建实体 / 重连时消费一次。0 的含义见 player/comp/player_ownership_comp.h。
// 一次载入生命周期里 A2′(载入前"先核归属再删"继承来的 handoff 标记,cross-zone-scene-travel.md §12.6.3
// 第三步)的状态。挂在待入场上下文上:同一次载入期间的重连覆盖上下文时必须原样拷过去
// (scene_handler.cpp 的新载入分支),载入完成 / 放弃时随上下文一起消失。
// 只在 scene 逻辑线程上读写(PlayerEnterGameNode、Redis 回调、RedisSystem 定时器同一个 EventLoop)。
struct InheritClearState
{
	// 本次载入生命周期号(节点内单调,0 = 还没分配)。A2′ 的应答只按值捕获它,晚到时据此认出
	// "这次载入已经放弃 / 已经建了实体"。
	uint64_t lifecycle{0};
	// A2′ 当前针对的 epoch 与进度(只描述"针对 targetEpoch 的那一次",见 exit_release_mark::DecideInheritGate)。
	uint64_t targetEpoch{0};
	exit_release_mark::InheritClearPhase phase{exit_release_mark::InheritClearPhase::kNone};
	// 每发一次 EVAL 加一;应答只按值捕获发送时的值,不等于当前值的应答只记账、不参与闸门判定。
	uint32_t attemptSeq{0};
	// 针对 targetEpoch 已发出的次数与首发时刻(单调时钟),重发上限见 exit_release_mark::IsInheritClearExhausted。
	uint32_t attemptsSent{0};
	exit_release_mark::Clock::time_point firstSentAt{};
	exit_release_mark::Clock::time_point nextRetryAt{};
	// 本生命周期已发出、应答未到的 EVAL 数(可能跨 epoch 不止一个)。
	uint32_t outstandingReplies{0};
	// 本生命周期删掉过(或可能删掉过)epoch == rewriteEpoch 的标记;0 = 没有。没建出实体就放弃时按它补写(M7)。
	uint64_t rewriteEpoch{0};
	// 载入已经完成、在等 A2′ 应答 / 重发时暂存的载入结果(M10:等待期间保留,不重新 GET)。
	std::shared_ptr<const PlayerAllData> stashedLoad;
};

struct PlayerEnterContext
{
	PlayerGameNodeEntryInfoComp enterInfo;
	uint32_t homeZoneId{0};
	uint64_t ownerEpoch{0};
	InheritClearState inheritClear;
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
//                          持续增长 = zone Redis 持续不可用 / 半开;单个玩家最迟在冻结上限(70s)被处置,
//                          看 freeze_cap_reached / mark_sent_destroyed(唯一例外见 destroy_deferred_unsettled_save)
//   frozen_ms_total        走到终态(granted / resolved_in_place / aborted / mark_sent_destroyed)的交接累计冻结毫秒数;
//   frozen_ms_max          平均冻结时长 = frozen_ms_total / (granted + resolved_in_place + aborted + mark_sent_destroyed),
//                          max 是单次最大值
//   withdraw_deferred      作废交接后撤回 handoff 标记没能当场确认(Redis 不通 / 命令没发出去 / 应答丢失或报错),
//                          条目留在待撤回表里重试。趋势类计数:同一条目每失败一次计一次。持续增长 = Redis 不稳
//   withdraw_expired       到截止时刻(标记 TTL + 余量)仍未确认撤回而放弃,或待撤回表满被淘汰。应恒为 0,
//                          非 0 = 有标记只靠 TTL 过期,期间存在回档窗口,要排查(见 handoff_mark_withdraw.h)
//   granted_client_reset   granted_without_reply 中,源端在销毁实体之前给客户端发了失败 tip + 踢线 34、
//                          让它断线回选服重登的次数(跨 zone 传送、放行却没收到放行应答、且 location
//                          确认是本次交接的等待落点,判据见 travel_outcome::ShouldResetClientOnGrant 与
//                          ResolveTravelOutcome)。重定向其实已送达时,客户端会先断开旧连接、实体先被
//                          "退出优先"销毁,走不到这里,所以它基本等于真正被踢回选服的人数。
//                          例外:gate-cmd 消费滞后超过 30s 时,一次"迟到但成功"的传送也会被踢回选服
//                          (踢线走 scene→gate TCP 直达,先于 Kafka 上的 124 到达)。应接近 0
//   reply_uncorrelated     EnterScene 应答没带 correlation_id(号为 0)、退回按 player_id 对应答的次数
//                          (见 enter_scene_reply)。scene_manager 全部升级后应恒为 0;非 0 = 有 scene_manager
//                          没升级,或有发送点绕过了统一出口(SendCorrelatedEnterScene)
//   reply_unmatched        被丢弃的可疑外来应答:有等待者、号对不上,且是拒绝 / 重定向 / 发生在交接期间
//                          (enter_scene_reply::IsSuspiciousUnmatched)——正是过去会被错吃成交接证据的那一类。
//                          基线接近 0,与"换图后立刻再换图 / 交接作废后立刻换图"的频率相关,只看突增。
//                          路由先到之后才到的纯成功迟到应答是常态,不计
//   ── 冻结硬上限 / 晚发闸 /「标记已发出」统一收口(travel_freeze_cap.h)──
//   freeze_cap_reached     冻结满 travel_freeze_cap::kFreezeCap(70s,单调时钟)仍无结论而被处置的次数:标记没发出
//                          的那一支同时计入 aborted,已发出的那一支同时计入 mark_sent_destroyed。两道 30s 看门狗与
//                          35s 晚发闸都先于它收敛,应恒为 0;非 0 = 有归属核实不了(zone Redis 不可用 / 半开)
//   dispatch_window_closed 冻结超过 kDispatchWindow(35s)才走到发送点、被晚发闸拦下的次数:set_mark 阶段(SET 还没发)
//                          同时计入 aborted,enter_scene 阶段(SET 已 OK)同时计入 mark_sent_destroyed。应接近 0
//   mark_sent_destroyed    新终态:handoff 标记的 SET 已发出、归属无法在本节点核实,tip(会话活着时)+ 踢线 34 + 不存盘
//                          销毁(ConcludeHandoffAfterMarkSent)。四个收口点:冻结上限 / enter_scene 晚发闸 / SET 之后发现
//                          没有 gate 会话 / SET 之后发现没有 scene_manager。应接近 0
//   mark_sent_client_reset mark_sent_destroyed 的子集:销毁前给客户端发了 tip + 踢线 34(会话活着、实体不在退出中)
//   destroy_deferred_unsettled_save  标记已发出、该销毁了,但该玩家还有未落地的存盘而推迟销毁的次数(每次交接最多计一次)。
//                          StartTravelHandoff 快路径补写之后不可达,应恒为 0;非 0 = 冻结越过了上限(唯一允许的例外)
//   freeze_unstamped       交接组件上的单调冻结起点(frozenAtSteady)没打点、被上限扫描就地补记的次数。应恒为 0,
//                          非 0 = 有一条挂交接组件的路径漏写了它(只会晚处置,不会提前销毁)
//   watchdog_early_fire    存盘 / 应答看门狗比单调截止时刻早 1s 以上被 muduo(按墙钟)唤醒、按剩余时间重挂的次数。
//                          非 0 = 墙钟向前跳过 ≥1s(对时 / 人工改时间)
//   handoff_fastpath_forced 交接快路径判"盘上已是同一份"、但该 key 还有在途 / 排队中的存盘,改走一次真实存盘再写标记的
//                          次数(与退出链的 exit_fastpath_deferred 同一写法)。趋势值
//   ── GO-2 根治:源端原子取证的三种新结论(travel_outcome::Ownership,cross-zone-scene-travel.md §12.8 判定表)──
//   rolled_back_adopted    B5:取证读到本次交接标记原文的回滚回执(scene_manager 推路由失败、单调回滚到本节点),
//                          采纳回滚后的 epoch(E+2)、解冻并强制存盘一次。同时计入 aborted(或 resolved_in_place)。
//                          基线 0,非 0 = 出现过 Kafka 路由失败,对照 scene_manager 的 [RouteRollback] 日志
//   returned_after_grant   B7:epoch 变了、没有本次回执,location 却指回本节点本 zone(已放行后又回到本节点 / 回滚链 /
//                          owner_epoch 键被淘汰读成 0)。不解冻,沿用"已放行"分支不存盘销毁,同时计入 granted。基线 0
//   rollback_receipt_anomaly B6:回执是本次标记原文,其余交叉校验却不成立(键被淘汰后补种 / 新旧 scene_manager 混跑 /
//                          数据写坏)。按"标记已发出"收口(ConcludeHandoffAfterMarkSent,同时计入 mark_sent_destroyed)。应恒 0
//
// started 减去 (granted + resolved_in_place + aborted + exit_wins + mark_sent_destroyed) = 仍在途的 + 交接期间被存盘
// CAS 拒而销毁的(后者已计入 owner_epoch_stats::StaleOwnerWriteRejected)。
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
		std::atomic<uint64_t> grantedClientReset{0};
		std::atomic<uint64_t> replyUncorrelated{0};
		std::atomic<uint64_t> replyUnmatched{0};
		std::atomic<uint64_t> freezeCapReached{0};
		std::atomic<uint64_t> dispatchWindowClosed{0};
		std::atomic<uint64_t> markSentDestroyed{0};
		std::atomic<uint64_t> markSentClientReset{0};
		std::atomic<uint64_t> destroyDeferredUnsettledSave{0};
		std::atomic<uint64_t> freezeUnstamped{0};
		std::atomic<uint64_t> watchdogEarlyFire{0};
		std::atomic<uint64_t> handoffFastpathForced{0};
		std::atomic<uint64_t> rolledBackAdopted{0};
		std::atomic<uint64_t> returnedAfterGrant{0};
		std::atomic<uint64_t> rollbackReceiptAnomaly{0};
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
		uint64_t grantedClientReset{0};
		uint64_t replyUncorrelated{0};
		uint64_t replyUnmatched{0};
		uint64_t freezeCapReached{0};
		uint64_t dispatchWindowClosed{0};
		uint64_t markSentDestroyed{0};
		uint64_t markSentClientReset{0};
		uint64_t destroyDeferredUnsettledSave{0};
		uint64_t freezeUnstamped{0};
		uint64_t watchdogEarlyFire{0};
		uint64_t handoffFastpathForced{0};
		uint64_t rolledBackAdopted{0};
		uint64_t returnedAfterGrant{0};
		uint64_t rollbackReceiptAnomaly{0};

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
		snapshot.grantedClientReset = counters.grantedClientReset.load(std::memory_order_relaxed);
		snapshot.replyUncorrelated = counters.replyUncorrelated.load(std::memory_order_relaxed);
		snapshot.replyUnmatched = counters.replyUnmatched.load(std::memory_order_relaxed);
		snapshot.freezeCapReached = counters.freezeCapReached.load(std::memory_order_relaxed);
		snapshot.dispatchWindowClosed = counters.dispatchWindowClosed.load(std::memory_order_relaxed);
		snapshot.markSentDestroyed = counters.markSentDestroyed.load(std::memory_order_relaxed);
		snapshot.markSentClientReset = counters.markSentClientReset.load(std::memory_order_relaxed);
		snapshot.destroyDeferredUnsettledSave = counters.destroyDeferredUnsettledSave.load(std::memory_order_relaxed);
		snapshot.freezeUnstamped = counters.freezeUnstamped.load(std::memory_order_relaxed);
		snapshot.watchdogEarlyFire = counters.watchdogEarlyFire.load(std::memory_order_relaxed);
		snapshot.handoffFastpathForced = counters.handoffFastpathForced.load(std::memory_order_relaxed);
		snapshot.rolledBackAdopted = counters.rolledBackAdopted.load(std::memory_order_relaxed);
		snapshot.returnedAfterGrant = counters.returnedAfterGrant.load(std::memory_order_relaxed);
		snapshot.rollbackReceiptAnomaly = counters.rollbackReceiptAnomaly.load(std::memory_order_relaxed);
		return snapshot;
	}
} // namespace travel_handoff_stats

// 退出存盘收尾(Z1 修复,cross-zone-scene-travel.md §12.6.3 第一步)的计数。与 travel_handoff_stats
// 同一套写法(relaxed atomic、不接 Prometheus、只在 scene 逻辑线程上写),由 player_lifecycle.cpp 里与
// [TravelHandoff] 同款的 30s 定时器(第一次有玩家退出时挂上)打成一行 [ExitPersist]:key=value 平铺、
// 进程启动以来的累计值,有变化才打,级别 INFO。
//
//   exit_superseded        退出存盘落地时发现已被一个更新的会话取代(player_exit::IsSupersedingSession),
//                          保留实体、摘退出标记。正常重连在 EnterScene 第 0 步就摘了标记,走不到这里,应接近 0
//   exit_intent_missing    带 UnregisterPlayer 却没有 PlayerExitIntentComp(成对约定被破坏)。fail-closed
//                          保留实体、摘 UnregisterPlayer。应恒 0,非 0 = 有路径漏挂 / 漏摘
//   exit_resave            落地内容与当前内存不一致,更新快照后重存。("还有未落地的存盘"只在落地回调之外的
//                          收尾口成立;落地回调里 HasUnsettledSave 恒为 false,见 redis_client.h 的说明)
//                          偶发正常(退出存盘在途时的战斗结算 / 客户端操作已被拦,但仍有别的改动来源)
//   exit_resave_capped     重存到上限仍不收敛,fail-closed 保留实体(交周期存盘 / 再次退出请求 / 停机看门狗)。
//                          已知的逐帧改动源(客户端消息、战斗结算、移动积分)都已关掉,应接近 0;持续非 0 =
//                          还有未识别的改动源在改退出中的实体
//   exit_exhausted_rekick  超限保留的实体又收到一次退出请求(停机 exitAllPlayers 等),补发了一次存盘
//   exit_fastpath_deferred 退出时 dirty-save 快路径判"盘上已是同一份",但该 key 还有在途 / 排队中的存盘,
//                          改走一次真实存盘、等落地回调收尾(M4)
//   exit_client_msg_rejected 退出中的实体收到 ExitGame 以外的客户端消息,被丢弃(已回 tip 的与只丢弃的都计)
//   exit_deposed_on_reentry 进场路由命中本节点上的旧实体(退出中,或不在交接中的活实体),且 owner_epoch 显示
//                          中间有别的持有者(player_exit::IsDeposedOnReentry),不存盘销毁后从盘上重载。非 0 = 有退出
//                          曾被保留到租约之后(看 exit_resave_capped / save outran reconnect lease),或本节点上
//                          留着没退出的僵尸实体(意图缺失分支保留的实体 / gate 崩溃没代发 ExitGame)
namespace exit_persist_stats
{
	struct Counters
	{
		std::atomic<uint64_t> superseded{0};
		std::atomic<uint64_t> intentMissing{0};
		std::atomic<uint64_t> resave{0};
		std::atomic<uint64_t> resaveExhausted{0};
		std::atomic<uint64_t> exhaustedRekick{0};
		std::atomic<uint64_t> fastpathDeferred{0};
		std::atomic<uint64_t> clientMsgRejected{0};
		std::atomic<uint64_t> deposedOnReentry{0};
	};

	inline Counters& Get()
	{
		static Counters g_counters;
		return g_counters;
	}

	inline void Inc(std::atomic<uint64_t>& counter) { counter.fetch_add(1, std::memory_order_relaxed); }

	struct Snapshot
	{
		uint64_t superseded{0};
		uint64_t intentMissing{0};
		uint64_t resave{0};
		uint64_t resaveExhausted{0};
		uint64_t exhaustedRekick{0};
		uint64_t fastpathDeferred{0};
		uint64_t clientMsgRejected{0};
		uint64_t deposedOnReentry{0};

		bool operator==(const Snapshot&) const = default;
	};

	inline Snapshot Read()
	{
		const auto& counters = Get();
		Snapshot snapshot;
		snapshot.superseded = counters.superseded.load(std::memory_order_relaxed);
		snapshot.intentMissing = counters.intentMissing.load(std::memory_order_relaxed);
		snapshot.resave = counters.resave.load(std::memory_order_relaxed);
		snapshot.resaveExhausted = counters.resaveExhausted.load(std::memory_order_relaxed);
		snapshot.exhaustedRekick = counters.exhaustedRekick.load(std::memory_order_relaxed);
		snapshot.fastpathDeferred = counters.fastpathDeferred.load(std::memory_order_relaxed);
		snapshot.clientMsgRejected = counters.clientMsgRejected.load(std::memory_order_relaxed);
		snapshot.deposedOnReentry = counters.deposedOnReentry.load(std::memory_order_relaxed);
		return snapshot;
	}
} // namespace exit_persist_stats

// 断线释放标记(A1′)与载入时清标记(A2′)的计数(cross-zone-scene-travel.md §12.6.3 第二步 / 第三步)。
// 写法同 exit_persist_stats(relaxed atomic、不接 Prometheus、player_id 不做 label),由 player_lifecycle.cpp
// 与 [ExitPersist] 同一个 30s 定时器打成一行 [ExitRelease](INFO,有变化才打,进程启动以来的累计值)。
//
//   decisions[]           A1′ 每一次判定按结果计数,key 见 exit_release_mark::kExitReleaseDecisionNames:
//                         attempted = 判定为写、发出了条件写;skip_* = 各类不写的原因
//   written / epoch_moved A1′ 条件写返回 1 / 0(owner_epoch 已变,不写是对的)
//   failed                A1′ 空应答 / ERROR / 派发失败(Redis 不可用也算)。一律不重试
//   inheritResults[]      A2′ 每一次 EVAL 的结果,key 见 exit_release_mark::kInheritClearResultNames
//   inherit_failed        A2′ 失败(应答失败 + 派发失败),会有上限地重发
//   inherit_refused       闸门拒建实体(核对不过 / 重发耗尽 / 没有针对本 epoch 的 A2′),已回 kEnterSceneFailed
//   inherit_rewritten     载入被放弃时补写成功(M7);inherit_rewrite_skipped = 条件写发现 owner_epoch 已变没写;
//                         inherit_rewrite_failed = 补写失败(不重试)
//   inherit_rewrite_dropped_node_holds  已放弃那一轮的晚到应答要补写,但本节点已建出该玩家实体(本节点就是
//                         持有者),丢弃补写(exit_release_mark::DecideAbandonedRewrite)
//   inherit_rewrite_handed_over         同上,但更新的一轮还在载入:补写责任移交给它(它建出实体即作废,
//                         它也放弃时由它补写,计入 inherit_rewritten / skipped / failed)
namespace exit_release_stats
{
	struct Counters
	{
		std::array<std::atomic<uint64_t>, exit_release_mark::kExitReleaseDecisionCount> decisions{};
		std::atomic<uint64_t> written{0};
		std::atomic<uint64_t> epochMoved{0};
		std::atomic<uint64_t> failed{0};
		std::array<std::atomic<uint64_t>, exit_release_mark::kInheritClearResultCount> inheritResults{};
		std::atomic<uint64_t> inheritFailed{0};
		std::atomic<uint64_t> inheritRefused{0};
		std::atomic<uint64_t> inheritRewritten{0};
		std::atomic<uint64_t> inheritRewriteSkipped{0};
		std::atomic<uint64_t> inheritRewriteFailed{0};
		std::atomic<uint64_t> inheritRewriteDroppedNodeHolds{0};
		std::atomic<uint64_t> inheritRewriteHandedOver{0};
	};

	inline Counters& Get()
	{
		static Counters g_counters;
		return g_counters;
	}

	inline void Inc(std::atomic<uint64_t>& counter) { counter.fetch_add(1, std::memory_order_relaxed); }

	struct Snapshot
	{
		std::array<uint64_t, exit_release_mark::kExitReleaseDecisionCount> decisions{};
		uint64_t written{0};
		uint64_t epochMoved{0};
		uint64_t failed{0};
		std::array<uint64_t, exit_release_mark::kInheritClearResultCount> inheritResults{};
		uint64_t inheritFailed{0};
		uint64_t inheritRefused{0};
		uint64_t inheritRewritten{0};
		uint64_t inheritRewriteSkipped{0};
		uint64_t inheritRewriteFailed{0};
		uint64_t inheritRewriteDroppedNodeHolds{0};
		uint64_t inheritRewriteHandedOver{0};

		bool operator==(const Snapshot&) const = default;
	};

	inline Snapshot Read()
	{
		const auto& counters = Get();
		Snapshot snapshot;
		for (std::size_t i = 0; i < snapshot.decisions.size(); ++i)
		{
			snapshot.decisions[i] = counters.decisions[i].load(std::memory_order_relaxed);
		}
		snapshot.written = counters.written.load(std::memory_order_relaxed);
		snapshot.epochMoved = counters.epochMoved.load(std::memory_order_relaxed);
		snapshot.failed = counters.failed.load(std::memory_order_relaxed);
		for (std::size_t i = 0; i < snapshot.inheritResults.size(); ++i)
		{
			snapshot.inheritResults[i] = counters.inheritResults[i].load(std::memory_order_relaxed);
		}
		snapshot.inheritFailed = counters.inheritFailed.load(std::memory_order_relaxed);
		snapshot.inheritRefused = counters.inheritRefused.load(std::memory_order_relaxed);
		snapshot.inheritRewritten = counters.inheritRewritten.load(std::memory_order_relaxed);
		snapshot.inheritRewriteSkipped = counters.inheritRewriteSkipped.load(std::memory_order_relaxed);
		snapshot.inheritRewriteFailed = counters.inheritRewriteFailed.load(std::memory_order_relaxed);
		snapshot.inheritRewriteDroppedNodeHolds = counters.inheritRewriteDroppedNodeHolds.load(std::memory_order_relaxed);
		snapshot.inheritRewriteHandedOver = counters.inheritRewriteHandedOver.load(std::memory_order_relaxed);
		return snapshot;
	}
} // namespace exit_release_stats

namespace player_exit
{
	// 刚落地的 PlayerAllData 是否就是当前内存(M1 "比对后才销毁")。比对前把两边 player_database /
	// player_database_1 的 stress_test_probe 剔掉:压测探针只在真正写盘的那份上打(SavePlayerToRedis 在快路径
	// 比对之后才 Stamp),落地的 message 带着 test_seq / test_sig,而新 marshal 的不带;不剔的话开了
	// STRESS_TEST_PROBE 的构建永远判"不一致",退出永不收敛。其余字段用 dirty_save::IsEqual(与快路径同一把尺子)。
	// 入参不改;内部各拷一份再剔。只在退出落地回调里调,不在逐帧路径上。
	bool IsPersistedPayloadCurrent(const PlayerAllData& landed, const PlayerAllData& current);
} // namespace player_exit

// 交接去留裁决(PlayerLifecycleSystem::ResolveTravelOutcome)手里的"证据":这次裁决是凭什么进来的。
// 替代原来的 bool replyWasSuccess —— 一个布尔分不清"显式失败""没收到应答""EnterScene 根本没发出去"
// "协议异常",而它们在"epoch 已变"时该不该让客户端断线重登答案不同(见 ShouldResetClientOnGrant)。
// 不许在"没能当场裁决"之后翻成 kNoReply:否则 kSucceeded(同 zone)会补一条假的失败 tip,
// kAnomalous / kMarkWriteUnknown 会被当成"没收到应答"踢线。两道保证缺一不可:
//   1. ResolveTravelOutcome 入口把非 kNoReply 的证据记到交接组件上,之后任何一次裁决(包括首次挂的、
//      从不取消的 kNoReply 看门狗先到期)都用 EffectiveEvidence 取记下的那条;
//   2. 看门狗重挂时原样透传当前证据(没有首次看门狗的 kMarkWriteUnknown 靠这一条)。
//
// 底层类型 uint8_t 数的是证据种类数(现 5 种)。纯内存使用,不进协议、不落库。kCount 固定为最后一项。
namespace travel_outcome
{
	enum class Evidence : uint8_t
	{
		kSucceeded,        // 同 zone 放行的成功应答(无 redirect),或进场路由已把交接中的玩家就地放进本节点的新场景
		kFailed,           // scene_manager 的显式失败应答(任何非 0 error_code)
		kNoReply,          // 应答看门狗到期(scene_manager 不可达 / zrpc 服务端超时:传输失败对交接只记日志,不当证据)
		kMarkWriteUnknown, // 写 handoff 标记的 SET 结果未知(连接断开,空 reply):EnterScene 根本没发出去
		kAnomalous,        // 跨 zone 却"成功且无 redirect":协议异常(典型是 scene_manager 版本错配)

		kCount
	};

	constexpr std::size_t kEvidenceCount = static_cast<std::size_t>(Evidence::kCount);

	// 只给日志用的名字,与枚举一一对应。数组长度由初始化项推导,漏加 / 多加一行都会被 static_assert 拦下。
	inline constexpr const char *kEvidenceNames[] = {
		"succeeded",
		"failed",
		"no_reply",
		"mark_write_unknown",
		"anomalous",
	};
	static_assert(std::size(kEvidenceNames) == kEvidenceCount, "kEvidenceNames 必须与 Evidence 一一对应");

	constexpr const char *EvidenceName(Evidence evidence)
	{
		const auto index = static_cast<std::size_t>(evidence);
		return index < kEvidenceCount ? kEvidenceNames[index] : "?";
	}

	// 裁决时实际采用的证据。交接组件上记下的证据(应答 / 路由落点带来的,见
	// PlayerTravelHandoffComp.recordedEvidence)优先于调用方这次带进来的证据:首次挂的 kNoReply 看门狗
	// 从不取消,它会在"应答已到、但 Redis 不可用没能裁决"之后先到期,不能让它把真实证据盖成超时。
	// 记下的底层值越界(不该发生)视同没记,退回调用方证据。参数拆开传,单测不必构造组件。
	constexpr Evidence EffectiveEvidence(bool hasRecorded, uint8_t recorded, Evidence incoming)
	{
		return hasRecorded && recorded < kEvidenceCount ? static_cast<Evidence>(recorded) : incoming;
	}

	// 交接已被放行(owner_epoch 已变)、源端却没拿到放行应答时,要不要在销毁实体之前让客户端断线重登
	// (失败 tip + 踢线 34)。纯判据,只看"交接种类 + 证据";运行期还有三道条件由 ResolveTravelOutcome
	// 叠加(实体没在退出、location 是本次交接的等待落点,见 IsAwaitingPlacementOfHandoff)。
	//
	// 只对跨 zone 生效:第一条腿只推 RedirectToGateEvent、从不改绑 gate 上的会话;重定向真送达了,
	// 客户端会立即摘掉旧连接的处理器并在连上新 gate 后关掉旧连接,旧连接一断 gate 就给本节点发 ExitGame,
	// 实体先被"退出优先"销毁,根本走不到这里。所以此时会话仍绑在本节点、客户端拿不到任何结果,
	// 不踢它就挂在一条"连着但没有实体"的哑连接上。
	// 同 zone 排除:放行靠 RoutePlayerEvent 把**同一个会话**改绑到目标节点,客户端不断线,本节点收不到
	// ExitGame;而"没收到应答"常常是应答丢了、路由已经到了 —— 此时踢线会断掉一条合法会话。
	// kSucceeded / kAnomalous / kMarkWriteUnknown 排除:前两者不说明客户端没拿到结果;kMarkWriteUnknown 时
	// EnterScene 根本没发出去,epoch 的任何推进都不是本次交接造成的。
	constexpr bool ShouldResetClientOnGrant(bool crossZone, Evidence evidence)
	{
		return crossZone && (evidence == Evidence::kFailed || evidence == Evidence::kNoReply);
	}

	// 放行后读到的 location 是不是**本次交接自己第一条腿**写下的等待落点:没有节点持有(node_id 为空)、
	// zone 是本次交接的目标 zone、location 里记的 epoch 与同一次原子读到的 owner_epoch 相同且非 0。
	// 不满足就说明 epoch 是被别的请求推进的(例如同一会话上一条迟到的同 zone 换图凭这份标记把会话改绑到
	// 了本 zone 的别的节点,或另一设备顶号),这时踢线会误伤一条合法会话,只销毁、不踢。
	// 参数用拆开的字段而不是 storage::PlayerLocation,单测不必链 proto。
	constexpr bool IsAwaitingPlacementOfHandoff(bool locationNodeEmpty, uint32_t locationZoneId,
												uint64_t locationOwnerEpoch, uint32_t targetZoneId,
												uint64_t redisOwnerEpoch)
	{
		return locationNodeEmpty && targetZoneId != 0 && locationZoneId == targetZoneId && redisOwnerEpoch != 0 &&
			   locationOwnerEpoch == redisOwnerEpoch;
	}

	// ── GO-2 根治:源端原子取证(cross-zone-scene-travel.md §12.8 判定表)──────────────────────────────
	//
	// 取证脚本。KEYS[1] = player:{id}:handoff   KEYS[2] = player:{id}:owner_epoch   KEYS[3] = player:{id}:location
	//          ARGV[1] = 本次交接的 requestedAtMs(std::to_string,即本次标记 "{E}:{t}" 里的 t)
	// 先删掉本次交接**这一族**标记,再在同一段脚本里 MGET owner_epoch 与 location,返回两元素数组(缺键 / 类型不对为 nil)。
	//
	// 为什么只删本族(值形如 "数字:t" 且 t 与 ARGV[1] 逐字节相同):本次的原标记 "E:t" 与 scene_manager 回滚转写出来的
	//   "E+2:t"(后缀逐字节不变,见 go/scene_manager/internal/logic/owner_epoch.go luaRollbackPlayerPlacement)都能删到;
	//   别人的标记(后缀不同)一律不碰 —— 过期源的看门狗若无条件 DEL,会删掉现任持有者的活标记,把它那次路由失败的回滚
	//   变成 marker_gone,玩家在任何节点都没有实体。这不削弱保证:epoch == E 的标记只能由 E 的唯一持有者(本节点)写出,
	//   别人的标记本来就不可能凭它把本节点的玩家放走。ms 恰好相同的碰撞,后果只是删了别人的标记(只伤活性),与改动前相同。
	// 为什么带 "#!lua"(Redis ≥ 7,全环境 7.2):不声明 no-writes 的带 shebang 脚本,在只读副本 / MISCONF / min-replicas
	//   不满足 / OOM 时**整体被拒**、返回 ERROR —— 不会出现"删除失败而读取成功"(旧写法里回调为空的 DEL 碰上 READONLY /
	//   MISCONF 会失败、MGET 照样成功,源端带着活标记解冻),也不会在本族标记恰好不在时从滞后副本读到旧 epoch。
	//   代价:OOM 时取证失败,保持冻结,由冻结硬上限收口。**不得**加 flags=no-writes,也不得去掉 shebang。
	// 为什么用 MGET 读而不用 GET:MGET 遇到类型不对的键返回 nil;GET 会抛 WRONGTYPE,脚本报错时删除已执行,
	//   之后每 30s 重挂一次、永远出不来。
	// 为什么是一段脚本(不变量 I0 机制 2):删除与读取在同一段原子脚本里,"拿到有效应答 ⇒ 本族标记已删、读数是删除之后
	//   那一刻的",不依赖连接的执行顺序。它**取代**了改动前"同一条不重放连接上先 DEL 后 MGET"的顺序保证。以后若拆回
	//   两条命令,**必须**走不重放的同一连接(tlsRedis.GetZoneRedis() 的 muduo hiredis::Hiredis:command() 是
	//   redisvAsyncCommand 的薄封装,无队列、无重放);改走 redis_client.h 那类带重试队列的接口会无声地打破这条保证。
	// 跨语言金样 go/scene_manager/internal/logic/owner_epoch_crosslang_test.go 逐字复制了本脚本(去掉首行 #!lua,
	// miniredis 不认 shebang):**改这里必须同步改那边**。
	inline constexpr const char *kLuaJudgeTravelOutcome =
		"#!lua\n"
		"local v = redis.call('MGET', KEYS[1])[1] "
		"if v and string.match(v, '^%d+:(%d+)$') == ARGV[1] then redis.call('DEL', KEYS[1]) end "
		"return redis.call('MGET', KEYS[2], KEYS[3])";

	// 取证脚本成功之后,读数说明了什么(判定表 B4–B9)。纯判定,不依赖 hiredis / entt,单测见 cross_zone_test 的
	// TravelOwnership.*。底层类型 uint8_t 数的是结论种类数(现 6 种)。纯内存使用,不进协议、不落库。kCount 固定为最后一项。
	enum class Ownership : uint8_t
	{
		kUnchanged,        // B4:owner_epoch == 缓存值 → 解冻
		kRolledBackToSelf, // B5:回执 == 本次标记原文,且其余交叉校验全部成立 → 采纳回滚后的 epoch、解冻、强制存盘
		kReceiptAnomaly,   // B6:回执 == 本次标记原文,其余交叉校验任一不成立 → 判不清,按"标记已发出"收口
		kReturnedToSelf,   // B7:epoch 变了、没有本次回执,location 却指回本节点本 zone → 不存盘销毁(沿用已放行分支)
		kMovedElsewhere,   // B8:epoch 变了,location 在别的节点 / 别的 zone / 是等待落点 → 已放行
		kLocationUnknown,  // B9:epoch 变了,location 缺失 / 解析失败 / 类型不对 → 已放行(location 已删或写坏)

		kCount
	};

	constexpr std::size_t kOwnershipCount = static_cast<std::size_t>(Ownership::kCount);

	// 只给日志用的名字,与枚举一一对应。数组长度由初始化项推导,漏加 / 多加一行都会被 static_assert 拦下。
	inline constexpr const char *kOwnershipNames[] = {
		"unchanged",
		"rolled_back_to_self",
		"receipt_anomaly",
		"returned_to_self",
		"moved_elsewhere",
		"location_unknown",
	};
	static_assert(std::size(kOwnershipNames) == kOwnershipCount, "kOwnershipNames 必须与 Ownership 一一对应");

	constexpr const char *OwnershipName(Ownership ownership)
	{
		const auto index = static_cast<std::size_t>(ownership);
		return index < kOwnershipCount ? kOwnershipNames[index] : "?";
	}

	// ClassifyOwnership 的输入,由 ResolveTravelOutcome 的取证回调从交接组件与脚本应答里采集。字段拆开传,单测不必链 proto。
	struct OwnershipFacts
	{
		uint64_t cachedEpoch{0};        // 发起这次取证时实体缓存的 owner_epoch(PlayerOwnerEpochComp)
		uint64_t markEpoch{0};          // 本次交接写标记用的 epoch(PlayerTravelHandoffComp.markEpoch;0 = SET 没发出去过)
		uint64_t redisEpoch{0};         // 脚本读到的 owner_epoch(缺键按 0)
		bool locationParsed{false};     // location 存在且能解析
		bool locationOnSelf{false};     // location 指回本节点本 zone(与 player_team.cpp 队伍跟随同一判法)
		uint64_t locationOwnerEpoch{0}; // location 里记的 owner_epoch
		bool receiptIsMine{false};      // location.rollback_receipt 非空,且与本次标记原文 "{markEpoch}:{requestedAtMs}" 逐字节相同
	};

	// 判定顺序即优先级,每一步都只可能落向更保守的一侧:
	//   1. epoch 未变最先判,不看 location 与回执:脚本已在同一原子步骤里删掉本族标记,"未变"同时证明此前从未放行
	//      (epoch 只经 INCR 前进,回滚也是 INCR)、此后也再放不出去;
	//   2. location 解析不了就不看回执(回执就存在 location 里);
	//   3. 回执是本次原文时,只有"回滚之后再无任何落点"的全部交叉校验成立才采纳:markEpoch 就是缓存值、epoch 恰好前进两格
	//      (本次铸造一格 + bump 回滚一格)、location 记的 epoch 与键一致、location 指回本节点本 zone。任一不成立即异常,不采纳;
	//   4. 其余按 location 指向分"回到本节点"与"在别处"。回执对不上时没有任何一条会走到采纳。
	constexpr Ownership ClassifyOwnership(const OwnershipFacts &facts)
	{
		if (facts.redisEpoch == facts.cachedEpoch)
		{
			return Ownership::kUnchanged;
		}
		if (!facts.locationParsed)
		{
			return Ownership::kLocationUnknown;
		}
		if (facts.receiptIsMine)
		{
			const bool consistent = facts.markEpoch != 0 && facts.markEpoch == facts.cachedEpoch &&
									facts.redisEpoch == facts.markEpoch + 2 &&
									facts.locationOwnerEpoch == facts.redisEpoch && facts.locationOnSelf;
			return consistent ? Ownership::kRolledBackToSelf : Ownership::kReceiptAnomaly;
		}
		return facts.locationOnSelf ? Ownership::kReturnedToSelf : Ownership::kMovedElsewhere;
	}
} // namespace travel_outcome

// EnterScene 应答的分发(PlayerLifecycleSystem::DispatchEnterSceneReply)。
//
// 为什么需要:应答回调里没有请求上下文。只按 player_id 对应答时,传送在途期间到达的**任何**一条 EnterScene
// 应答(组队跟随 / 疏散 / 镜像自动进场 / 普通换图的迟到应答)都会被当成交接证据:证据被污染、标记被 DEL 撤回、
// 跨 zone 时还可能被误判成"已放行却没收到应答"而踢线。
//
// 契约:scene 进程内所有 EnterScene 都经统一出口发出(player_lifecycle.cpp 的 SendCorrelatedEnterScene),
// 每条都带一个非 0 的线程内单调号(EnterSceneRequest.correlation_id),scene_manager 原样回显在
// EnterSceneResponse.correlation_id。因此:
//   应答号非 0 ⇔ 它一定是本线程某一次具体发送的应答,只有号与等待者记下的号相等才归那个等待者;
//   应答号为 0 ⇔ scene_manager 没回显(旧版)。此时退回今天按 player_id 对应答的行为,不 fail-closed:
//                fail-closed 会让滚动窗口内每次交接的失败都冻满 30s 看门狗、同 zone 放行后源端冻结实体的 AOI
//                消息串到新场景的客户端(gate 栅栏只比 player_id)、看门狗无条件 DEL 误删新节点的标记、
//                exit_wins 失真 —— 换来的只是窗口期内没有今天的串号 bug(cross-zone-scene-travel.md
//                「EnterScene 应答关联号」)。
// 等待者只有两种:交接(PlayerTravelHandoffComp.enterSceneCorrelationId,0 = 交接的 EnterScene 还没发)与
// 普通换图(PlayerSceneChangeInFlightComp.correlationId)。二者按构造不共存:StartTravelHandoff 摘在途换图组件,
// 发送侧闸 IsSceneChangeBusy 挡住交接期间的普通换图。
//
// 底层类型 uint8_t 数的是去向种类数(现 7 种)。纯内存使用,不进协议、不落库。kCount 固定为最后一项。
namespace enter_scene_reply
{
	enum class Route : uint8_t
	{
		kTravelHandoff,            // 号匹配本代交接 → 交接裁决(correlated = true;GO-2 从不凭应答回显的 epoch 采纳,回显只进日志)
		kLegacyTravelHandoff,      // 旧版 SM 没回显、交接的 EnterScene 已发 → 交接裁决(correlated = false,同今天)
		kSceneChange,              // 号匹配在途换图 → 普通换图收尾(18 起同 zone 交接 / 失败回 tip / 跟随只记日志)
		kLegacySceneChange,        // 旧版 SM 没回显、有在途换图 → 普通换图收尾(同今天,§11.2 的 18 链不断)
		kSceneChangeDuringHandoff, // 交接在途时号匹配在途换图:按构造不可达。丢弃 + LOG_ERROR,不走普通换图分支
								   // (否则会在交接中给客户端补一条失败 tip)
		kUnmatched,                // 有等待者但号对不上,或交接的 EnterScene 还没发:不是它的,丢弃
		kNoWaiter,                 // 既无交接也无在途换图(路由先到后的迟到应答、疏散 / 退出后重建的实体):no-op

		kCount
	};

	constexpr std::size_t kRouteCount = static_cast<std::size_t>(Route::kCount);

	// 只给日志用的名字,与枚举一一对应。数组长度由初始化项推导,漏加 / 多加一行都会被 static_assert 拦下。
	inline constexpr const char *kRouteNames[] = {
		"travel_handoff",
		"legacy_travel_handoff",
		"scene_change",
		"legacy_scene_change",
		"scene_change_during_handoff",
		"unmatched",
		"no_waiter",
	};
	static_assert(std::size(kRouteNames) == kRouteCount, "kRouteNames 必须与 Route 一一对应");

	constexpr const char *RouteName(Route route)
	{
		const auto index = static_cast<std::size_t>(route);
		return index < kRouteCount ? kRouteNames[index] : "?";
	}

	// 一条应答归谁。replyTag = 应答回显的号(0 = 旧版 SM 没回显);handoffTag / sceneChangeTag = 等待者记下的号。
	// 参数拆开传,单测不必构造组件。
	//   * 交接在途时只有本代交接自己的号能进交接段。handoffTag == 0(交接的 EnterScene 还没发,SET 在途或存盘在途)
	//     时任何应答都不可能是它的 —— 这也堵上了"requestedAtMs 在 SET 发出前就已置位"留下的错吃窗口;
	//   * 旧版 SM 下(replyTag == 0)退回按 player_id:有等待者就归等待者,与今天等价。
	constexpr Route Classify(uint64_t replyTag, bool handoffInFlight, uint64_t handoffTag, bool sceneChangeInFlight,
							 uint64_t sceneChangeTag)
	{
		if (handoffInFlight)
		{
			if (handoffTag != 0 && replyTag == handoffTag)
			{
				return Route::kTravelHandoff;
			}
			if (replyTag == 0)
			{
				return handoffTag != 0 ? Route::kLegacyTravelHandoff : Route::kUnmatched;
			}
			if (sceneChangeInFlight && replyTag == sceneChangeTag)
			{
				return Route::kSceneChangeDuringHandoff;
			}
			return Route::kUnmatched;
		}
		if (sceneChangeInFlight)
		{
			if (replyTag == 0)
			{
				return Route::kLegacySceneChange;
			}
			return replyTag == sceneChangeTag ? Route::kSceneChange : Route::kUnmatched;
		}
		return Route::kNoWaiter;
	}

	// 被丢弃的应答要不要计 reply_unmatched、打 INFO。只有"有等待者、号不符,并且是拒绝 / 重定向 / 发生在交接期间"
	// 的应答才是过去会被错吃的那一类;路由先到(EnterScene 3.1 已摘在途组件)之后才到、又撞上新一条换图的纯成功迟到
	// 应答是常态,计进来会让这个计数只反映换图频率,失去告警意义 —— 只打 DEBUG。
	constexpr bool IsSuspiciousUnmatched(Route route, bool handoffInFlight, bool rejected, bool hasRedirect)
	{
		if (route == Route::kSceneChangeDuringHandoff)
		{
			return true;
		}
		if (route == Route::kUnmatched)
		{
			return handoffInFlight || rejected || hasRedirect;
		}
		return false;
	}
} // namespace enter_scene_reply

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

	// 推进退出流程:挂 UnregisterPlayer + PlayerExitIntentComp(抄下退出那一刻的会话与原因)→ 摘场景 →
	// 存盘;落地后由 HandlePlayerAsyncSaved 的退出分支比对收敛再销毁(§12.6.3 第一步)。
	// cause 决定收尾时写不写断线释放标记(A1′,见 exit_release_mark.h);默认 kUnspecified = 来源不明 = 不写。已在退出中时不重挂,
	// 按 player_exit::MergeExitCause 合并原因后返回。
	static void HandleExitGameNode(entt::entity player, ExitCause cause = ExitCause::kUnspecified);

	// 退出即冻结运动学:把 Velocity / Acceleration 清零(只改已有组件,不 emplace)。HandleExitGameNode 在
	// 第一次存盘之前调用。不清的话 MovementSystem 会继续积分退出中实体的 Transform(它不排除 UnregisterPlayer,
	// 摘掉 SceneEntityComp 后导航夹持也失效),退出存盘的收敛比对每轮都判不一致。重连后客户端会重新上报速度。
	// 公开只为单测能直接验证;业务代码不要在退出流程之外调用。
	static void StopMotionForExit(entt::entity player);

	// 重连取消退出(EnterScene 第 0 步):UnregisterPlayer 与 PlayerExitIntentComp 成对摘掉
	// (player_exit_intent.h 的成对约定)。两者都不在时 no-op。公开只为单测能直接验证成对摘除。
	static void CancelExitOnReconnect(entt::entity player);

	// 进场路由命中本节点上的旧实体(退出中的,或不在交接中的活实体)、且 owner_epoch 显示中间有别的持有者
	// (player_exit::IsDeposedOnReentry)时,按"被废黜"不存盘销毁它并返回 true,调用方随后从盘上重载。
	// 其余情况返回 false、什么都不做(照常复用实体)。只由 SceneHandler::PlayerEnterGameNode 调用,
	// 排在 DiscardStaleHandoffEntity 之后(交接已发起的实体由它先处理,任何更新的 epoch 都丢弃)。
	static bool DiscardDeposedEntityOnReentry(entt::entity player, uint64_t incomingOwnerEpoch);

	// ── A2′:新载入前"先核归属再删"继承来的 handoff 标记(§12.6.3 第三步,M2 / M7 / M10)──
	// SceneHandler::PlayerEnterGameNode 的新载入分支在登记待入场上下文之后、AsyncLoad 之前调用。
	// ctx.ownerEpoch == N(非 0)且本生命周期还没针对 N 发过时,在 zone Redis 上发一段原子 Lua
	// (exit_release_mark::kLuaInheritClear):owner_epoch ≠ N 返回 -1、一个标记都不删;相等才删 epoch ≤ N 的标记。
	// 结果决定 HandlePlayerAsyncLoaded 的闸门(exit_release_mark::DecideInheritGate):已确认 → 建实体;
	// 在途 / 等重发 → 暂存载入结果;-1 / 重发耗尽 → 立即拒建实体、回 kEnterSceneFailed、擦掉预登记会话。
	// ctx.ownerEpoch == 0(旧版路由)改发 exit_release_mark::kLuaInheritClearUnknownEpoch:不核对归属、删 ≤ 当前
	// owner_epoch 的标记,闸门同样等它确认(复审 ownership-major)。待入场条目不存在时 no-op。
	static void BeginInheritedMarkClear(Guid playerId);

	// 有上限地重发失败的 A2′,并对超过截止时刻的(失败或在途)拒建实体。由 RedisSystem 驱动:
	//   reconnected = true  → Redis 重连回调里、playerRedis->OnReconnected() 之前调,忽略退避;
	//   reconnected = false → 1s 周期定时器里、RetryDuePending() 之前调,只发退避已到的。
	// 待入场表为空时零开销。
	static void RetryInheritedMarkClears(bool reconnected);

	// 已发出、应答未到的断线释放标记条件写数(A1′ 与 A2′ 放弃补写,R6)。计入停机 drain 谓词
	// (main.cpp 的 drained 条件)与 IsEmergencyRelocateDrained,受 Node drain 看门狗约束。
	static std::size_t ExitReleaseMarksInFlight();

	// 本节点身份当前是否确认有效(M3)的只读探针,由节点入口(nodes/scene/main.cpp)注入:scene 库不直接依赖
	// Node(单测宿主没有 Node,链进 node.cpp 会拖进整条 gRPC / etcd 依赖)。未注入 / 传空 = 身份不确认,
	// A1′ 一律不写(fail-closed)。探针只在 scene 逻辑线程上调用。
	static void SetNodeIdentityProbe(std::function<bool()> probe);

	// 该玩家是否处于"归属交接在途"的冻结态(PlayerFrozenComp 存在)。
	// 业务系统(AOI / 战斗 / 货币 / 背包 / 聊天 / 移动)写之前必须查它,为真就跳过:冻结中的实体
	// 虽然 entt 实体还在,但盘上那份才是要交给目标节点的真值,本节点再写就是制造分叉。
	// 名字里的 CrossZone 是历史遗留(最早只服务 player_migrate 搬数据链,该链已按
	// cross-zone-scene-travel.md CZ-1 下线);现在跨 zone 传送与同 zone 跨节点换图都走它。
	// 调用点有二十来处,不为改名去动它们。
	//
	// 不变量 I3(Battle 结算闸 player_battle.cpp IsSettlementApplicable 依赖它,签名与语义不改):对不在退出中的实体,
	// 本函数为 false 的任何时刻,本节点一定仍是属主。能摘掉 PlayerFrozenComp 又保留实体的出口只有三类
	// (cross-zone-scene-travel.md §12.8 第十节):
	//   A1  本次交接没写出任何标记(SET 未发出 / 确定没写)—— 换手门只凭 epoch 等于观察值的标记放行,E 代标记只有本节点能写;
	//   B4  取证脚本原子删掉本族标记后读到 epoch == 缓存值 —— 此前没放行、此后放不出去;
	//   B5  同一脚本读到本次标记原文的回滚回执,且 epoch == 标记 epoch + 2 == location.owner_epoch、location 指回本节点本 zone
	//       —— 采纳回滚后的 epoch 与解冻在同一回调里完成。
	// 其余摘冻结的路径(DestroyDeposedPlayer、ConcludeHandoffAfterMarkSent、存盘被 CAS 拒、B7–B11)都同时销毁实体;
	// "退出优先"只作用于带 UnregisterPlayer 的实体,战斗侧把它视同离线。新增任何解冻路径都必须重过这条论证。
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
	// HandleSceneChangeEnterSceneReply 据此起同 zone 交接。
	// 数值手抄自 go/scene_manager/internal/constants/errors.go 的 ErrHandoffPending:它走的是
	// scene_manager 自己的 error_code 轴(不是 tip 轴),Go 与 C++ 之间没有共享的生成物,
	// 改那边必须同步这里。全 C++ 只此一处定义,别处一律引用它,不许再写字面量 18。
	static constexpr uint32_t kSmErrHandoffPending = 18;

	// 客户端 RPC SceneSceneClientPlayer.TravelToZone 的系统层入口(CZ-7),handler 只委托到这里。
	// 校验 CZ-6(战斗 / 备战在途、组队在途不可传送)与参数,通过后调 StartTravelHandoff。
	// 返回 kTravelAccepted = 已受理(玩家已冻结、存盘已发起),**不代表已到达**。受理之后客户端只会看到三种结局之一:
	//   * 到达:随后收到 RedirectToGate;
	//   * 未成、原地恢复:随后收到 SendTipToClient,且已解冻(owner_epoch 没动;或 scene_manager 推路由失败、已把归属
	//     单调回滚到本节点并留下本次回执,本节点采纳回滚后的 epoch —— 两种都是玩家仍在本节点);
	//   * 未成、无法原地恢复(owner_epoch 已被推进、玩家已不在本节点,又没收到放行应答):SendTipToClient
	//     之后紧跟踢线 KickPlayer(34),实体销毁而不是解冻,客户端断线回选服重登(见 ResolveTravelOutcome)。
	//     handoff 标记已发出、但归属在本节点核实不了时同样是 tip + 34 + 不存盘销毁:冻结满 70s
	//     (travel_freeze_cap::kFreezeCap)、SET 的 OK 晚于 35s 晚发窗口才到,或 SET 之后才发现没有 gate 会话 /
	//     没有 scene_manager,或取证读到本次回滚回执却交叉校验不成立(ConcludeHandoffAfterMarkSent)。
	//     所以受理之后服务端最迟约 71s 一定给出结论。
	// 非 0 = 拒绝的 tip id,未改任何状态。
	// 目标 zone 是否真的存在由 scene_manager 判(C++ 侧没有 zone 表):不存在时走"受理后未成"。
	// sceneConfigId:0 = 由目标 zone 挑默认大世界;非 0 必须是 World 表登记的世界图,否则同步拒绝
	// (kEnterSceneSceneNotFound)。这张图在目标 zone 开没开只有 scene_manager 知道:没开时同样走
	// "受理后未成"(它在放行之前只读检查,未改任何状态)。
	static uint32_t RequestZoneTravel(entt::entity player, uint32_t targetZoneId, uint32_t sceneConfigId);

	// 发起一次归属交接的**唯一入口**:挂 PlayerTravelHandoffComp + PlayerFrozenComp,然后存盘;
	// 落地后由 BeginTravelHandoff 接手。两个调用方:RequestZoneTravel(跨 zone),以及
	// HandleSceneChangeEnterSceneReply 收到 18 时(同 zone 跨节点换图,targetZoneId = 本 zone)。
	// 返回 kTravelAccepted = 已进入交接;非 0 = tip id,且未改任何状态。
	// 自带一份最小校验(实体有效 / 未在退出 / 未在交接 / 不在战斗 / 会话活着 / 有可达的 scene_manager):18 到达时
	// 离发请求已经隔了一个往返,玩家可能刚进备战或刚断线,不能只信发请求那一刻的检查。scene_manager 预检的理由:
	// SET 之后才发现没有 SM 只能按"标记已发出"销毁并踢线,常见情形(请求时 SM 就不在)要在冻结之前挡住。
	// 冻结时同时在交接组件上打单调冻结起点(frozenAtSteady),冻结硬上限从这一刻起算。
	static uint32_t StartTravelHandoff(entt::entity player, uint32_t targetZoneId, uint64_t sceneId, uint32_t sceneConfigId);

	// 普通 EnterScene(客户端换图 / 镜像自动进场 / 队伍跟随)的发送侧闸:交接在途,或上一条
	// EnterScene 的应答 / 传输失败还没回来(TTL = SceneManager 的 gRPC deadline + 1s 内)时为真,调用方不得再发。应答已按 correlation_id 对号、不再串号;
	// 闸仍然必要,理由是在途记录只有一个槽(第二次登记会覆盖第一次的号,第一次的 18 就对不上号而被丢弃),
	// 以及旧版 scene_manager 不回显时仍按 player_id 对应答。详见 PlayerSceneChangeInFlightComp。
	// 该玩家有一份作废交接留下的 handoff 标记还没确认撤回时同样为真(按 player_id 判,见
	// handoff_mark_withdraw.h):标记可能还带着当前 owner_epoch,这时发跨节点 EnterScene 会免存盘过门。
	static bool IsSceneChangeBusy(entt::entity player);

	// 重试还没确认的 handoff 标记撤回,并丢弃过了截止时刻的条目。由 RedisSystem 驱动:
	//   reconnected = true  → Redis 重连回调里调,忽略重试间隔、全部立刻重发;
	//   reconnected = false → 1s 周期定时器里调,只发到了重试间隔的。
	// 表为空时零开销。没有初始化 RedisSystem 的宿主(单测)只登记、不重试。
	static void RetryPendingHandoffWithdrawals(bool reconnected);

	// 冻结硬上限扫描(travel_freeze_cap.h):遍历所有挂着 PlayerTravelHandoffComp 的实体(**包括退出中的**:
	// 骨架没有给退出开例外),冻结满 travel_freeze_cap::kFreezeCap 的按 travel_freeze_cap::DecideFreezeCap 处置 ——
	// handoff 标记没发出 → AbortTravelHandoff(解冻 + 失败 tip);已发出 → ConcludeHandoffAfterMarkSent
	// (tip + 踢线 34 + 不存盘销毁)。先收集、后处置(处置会改 registry)。
	// 由本文件的 1s 定时器(player_lifecycle.cpp 的 EnsureTravelFreezeCapTimer,第一次发起交接时挂上)与单测调用,
	// now 显式传入(单调时钟,AGENTS §11.2 显式依赖)。不是 per-tick 路径:平时只有 0 到几个交接实体。
	static void EnforceTravelFreezeCaps(travel_freeze_cap::Clock::time_point now);

	// 替在线玩家发普通 EnterScene(客户端换图 / 镜像自动进场 / 队伍跟随)的**唯一入口**:取一个关联号 →
	// 登记在途换图(PlayerSceneChangeInFlightComp:目标、发送时刻、playerRequested、号)→ 经统一出口带号发出 →
	// 返回号(只供调用方打日志)。应答回来时 DispatchEnterSceneReply 只把号相等的那条交给普通换图收尾
	// (18 起同 zone 交接 / 失败回 tip);旧版 scene_manager 不回显时退回按 player_id。
	// 在途目标直接取自 req(scene_id、scene_conf_id 窄化成 uint32):"记下的目标"与"发出的目标"只有一个来源。
	// 前置条件(入口内不重复判):调用方已过 IsSceneChangeBusy,且 smEntity 非空(已查 GetSceneManagerEntity)。
	// 实体无效时不登记、照样发出(与旧的"登记函数 + 直调生成发送函数"两步写法行为一致)。req 会被写入 correlation_id。
	// playerRequested = false:服务器替玩家发的(队伍跟随),被拒只记日志,不起交接、不回 tip
	// (见 PlayerSceneChangeInFlightComp)。
	// 取代旧的在途登记函数:旧名删除而不是保留,漏改的调用点在所有编译配置下都是硬错误(不依赖 /WX)。
	static uint64_t RequestSceneChange(entt::entity player, entt::entity smEntity, ::scene_manager::EnterSceneRequest& req,
									   bool playerRequested = true);

	// 每一条 EnterScene 应答的入口,由 rpc_replies/scene_manager_response_handler.cpp 无条件调用(那里只做适配)。
	// 分发规则见 enter_scene_reply::Classify:
	//   * player_id == 0(更老的 scene_manager 连 player_id 都不回显)→ LOG_DEBUG no-op;
	//   * correlation_id == 0 → reply_uncorrelated +1,本线程首次 LOG_WARN,随后按 player_id 退回旧行为;
	//   * 实体已不在(玩家在途中断线,退出优先;疏散 / 排空改派的应答)→ LOG_INFO no-op;
	//   * kTravelHandoff / kLegacyTravelHandoff        → HandleTravelEnterSceneReply(交接裁决);
	//   * kSceneChange / kLegacySceneChange            → 先按值抄、再摘在途换图组件,交给 HandleSceneChangeEnterSceneReply;
	//   * kSceneChangeDuringHandoff(不变量被破坏)     → 丢弃,LOG_ERROR,reply_unmatched +1;
	//   * kUnmatched                                   → 丢弃;可疑的(IsSuspiciousUnmatched)reply_unmatched +1 并 LOG_INFO,
	//                                                    其余 LOG_DEBUG。外来的 redirect 也只记录、不销毁:SM 若真放行了,
	//                                                    本代请求会被拒,由 ResolveTravelOutcome 按 epoch 收敛;
	//   * kNoWaiter                                    → LOG_DEBUG no-op。
	static void DispatchEnterSceneReply(const ::scene_manager::EnterSceneResponse& resp);

	// 每一条 EnterScene 传输失败(gRPC status 非 OK:deadline 到期 / scene_manager 不可达 / 服务端超时)的入口,
	// 由 rpc_replies/scene_manager_response_handler.cpp 的失败处理器调用;req 是生成的客户端保存的发出请求,
	// reason 是调用方拼好的方法名 + 状态码 + 消息(只用于日志,本系统不依赖 gRPC 头文件)。
	// 传输失败 = **结果未知**(scene_manager 可能已执行),只做不依赖对端结果的事
	// (docs/design/grpc-client-deadline-failure-callback.md §5 #1 / #2):
	//   * 按 req.correlation_id 走同一个 enter_scene_reply::Classify,**不按 player_id**;号为 0 → LOG_ERROR 丢弃;
	//   * 实体已不在(疏散 / 排空的发送、途中退出)→ LOG_INFO no-op;
	//   * kTravelHandoff                    → 只记日志:不当失败证据、不提前核实,去留仍由应答看门狗 / 冻结上限裁决;
	//   * kSceneChange                      → 先按值抄、再摘在途换图组件;playerRequested 回 kServiceUnavailable
	//                                         (不断言换图失败:路由事件可能随后到达),队伍跟随只记日志;
	//   * kSceneChangeDuringHandoff         → 不变量被破坏,LOG_ERROR 丢弃;
	//   * kUnmatched / kNoWaiter            → 记日志,不动任何等待者。
	static void DispatchEnterSceneTransportFailure(const ::scene_manager::EnterSceneRequest& req, const std::string& reason);

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
	// 请求 EnterScene(ZoneId=目标, SceneId, SceneConfId) → 应答经 DispatchEnterSceneReply 按号分到
	// HandleTravelEnterSceneReply。
	// 幂等:requestedAtMs 已非 0(交接已发起)则直接返回,不重复写标记、不重复请求。
	// 失败分支按"handoff 标记的 SET 发没发出去"分两类(travel_freeze_cap::IsHandoffMarkSent):
	//   * SET 发出之前(冻结已超过 35s 晚发窗口 / 无 epoch / Redis 断连 / 命令没发出去),以及 SET 收到 ERROR 应答
	//     (确定没写,只处置同一代交接,见 HandleTravelMarkWriteRejected)→ 一律 AbortTravelHandoff 解冻回 tip,不悬挂;
	//   * SET 之后在 RequestTravelEnterScene 里(SET 的 OK 晚于晚发窗口才到 / 无 gate 会话 / 无 scene_manager)→
	//     一律 ConcludeHandoffAfterMarkSent(tip + 踢线 34 + 不存盘销毁),不解冻。
	static void BeginTravelHandoff(Guid playerId);

	// handoff 标记的 SET 收到 ERROR 应答(Redis 明确没执行)。只由 BeginTravelHandoff 的 SET 回调调用;公开只为单测
	// 能直接验证代际判断,业务代码不要调。requestedAtMs = 发出这条 SET 的那一代交接(回调按值捕获)。
	// 只有实体上的交接组件仍是**同一代**(requestedAtMs 相等)才清 markEpoch(标记确定不存在,不登记撤回)并
	// AbortTravelHandoff;代际不符 / 组件不在 / 实体不在一律只记日志、什么都不动。
	// 为什么必须比代际:冻结硬上限让"销毁后同节点重登、再发起新一代交接"成为可能。旧一代 SET 的 ERROR 应答迟到时,
	// 新一代的 SET 可能已经发出 —— 不看代际就 Abort 会把新一代错误解冻(违反骨架 I2 / I3),还会按新一代的
	// markEpoch 登记撤回、删掉它刚写下的标记。今天 SET、A2′ 与取证走同一条 FIFO 连接,旧 ERROR 应当先于新实体的 A2′
	// 应答到达(新实体还建不出来),但那是连接实现的性质,不是这里可以依赖的契约。
	static void HandleTravelMarkWriteRejected(Guid playerId, uint64_t requestedAtMs, const char *err);

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
	// 存盘确实落地之后的统一收尾:紧急疏散改派(如果在疏散中)-> 断线释放标记(A1′,条件满足才写)
	// -> 摘会话 -> 销毁实体。异步存盘回调与"快路径没写盘"两条路径都走这里,避免两份收尾逻辑漂移。
	static void FinishExitAfterPersist(Guid playerId);

	// 向 SceneManager 请求把该玩家改派到大世界频道。只在疏散票据存在时生效,
	// 消费即删除(幂等)。返回 true = 本次消费了票据(改派自己写 handoff 标记,A1′ 不得再写)。
	// 标记是 owner_epoch 条件写(与 A1′ 同一段 exit_release_mark::kLuaWriteIfOwnerEpoch):epoch 已被回滚推进时不写,
	// 不覆盖 scene_manager 转写出来的 "E+2:t"(GO-2 §12.8);写没写成都照常发改派。
	static bool DispatchEmergencyRelocate(Guid playerId);

	// 抄改派票据 + 推进退出流程。整节点疏散与单场景排空共用。
	// 返回 true 表示登记了票据(玩家有活着的 gate 会话)。
	// cause:整节点疏散传 kIdentityConflict,单场景排空传 kSceneDrain(原样交给 HandleExitGameNode)。
	static bool EnqueueRelocateTicket(entt::entity playerEntity, const char *reasonTag, ExitCause cause);

	// SavePlayerToRedis 的本体。allowSkipWhenPersisted = false 时跳过 dirty-save 快路径、一定写盘
	// (交接已发起时的"不得再写"仍然生效)。快路径判"盘上已是同一份",但同 key 还有在途 / 排队中的存盘时,
	// 必须改走一次真实存盘、等落地回调收尾。只有三个调用方:
	//   * 退出流程的 M4 分支(HandleExitGameNode,exit_fastpath_deferred,§12.6.4 M4);
	//   * StartTravelHandoff 的快路径补写(handoff_fastpath_forced):保证写 handoff 标记之前没有更早的未落地存盘,
	//     也就是"SET 发出 ⇒ Redis ⊇ 冻结内存"(骨架 I1);
	//   * ResolveTravelOutcome 的 B5 采纳(rolled_back_adopted,GO-2 §12.8):采纳回滚后的 epoch(E+2)、解冻之后立即写一次。
	//     一是让 DBTask(E+2) 立刻越过兼容窗口里幽灵持有者可能落下的 DBTask(E+1);二是恢复"快照 == Redis"的前提 ——
	//     兼容窗口里旧版目标节点若在回滚前写过一份 Redis blob,不强制写的话 dirty-save 快路径会误判"盘上已是同一份"。
	// 不给 SavePlayerToRedis 加默认参数:它以函数指针形式被引用(asset_op_system.cpp 的 PersistFn)。
	static bool SavePlayerToRedisImpl(entt::entity player, bool allowSkipWhenPersisted);

	// 把玩家从所在场景摘掉:BeforeLeaveScene(AOI / crowd)→ ScenePlayers 摘除 → 摘 SceneEntityComp + Hex。
	// 退出流程与"被废黜"销毁共用;不在场景里则 no-op。
	static void DetachFromScene(entt::entity player);

	// "我已被废黜"的统一销毁:不存盘、不改派(疏散票据作废)、摘冻结 / 交接标记、摘场景、摘会话、销毁实体。
	// 入口:存盘被 epoch CAS 拒(HandlePlayerSaveRejected)、交接被放行(EnterScene 应答 Redirect /
	// ResolveTravelOutcome 判出 epoch 已变 —— 含 B7:epoch 变了、没有本次回滚回执、location 却指回本节点,
	// 同样不解冻而销毁)、进场路由撞上过期的交接实体(DiscardStaleHandoffEntity)、
	// handoff 标记已发出但归属核实不了(ConcludeHandoffAfterMarkSent,reasonTag = travel_freeze_cap::MarkSentSiteName)。
	// 最后这个入口下 routine=false 打出的原文 "(ownership moved away)" 是沿用下来的,实际含义是"归属未知";
	// 判读以它前一行的 [ZoneTravel][MarkSentDestroy](ownership UNKNOWN)为准。
	// routine = 这是交接成功的正常收尾,收尾日志打 INFO;否则打 WARN(异常信号,要留证据)。
	// 本函数自己**不给客户端发任何消息**,而且它会摘会话(RemovePlayerSession),之后再也发不出去。
	// 需要让客户端断线重登的调用方(ResolveTravelOutcome 的"跨 zone 已放行却没收到放行应答"分支)
	// 必须在调用本函数**之前**发失败 tip + 踢线 34,实体随后销毁而不是解冻。
	static void DestroyDeposedPlayer(Guid playerId, const char *reasonTag, bool routine = false);

	// 撤回本节点写下的 handoff 标记 "{markEpoch}:{requestedAtMs}"(交接作废的两处:"退出优先"与
	// AbortTravelHandoff)。先登记进待撤回表,再发按标记原文比对的条件删除;只有拿到 Redis 的整数应答
	// 才销账。Redis 不通 / 命令没发出去 / 应答丢失或报错:LOG_ERROR + withdraw_deferred,条目留表,由
	// RetryPendingHandoffWithdrawals 重试到标记 TTL 为止;期间 IsSceneChangeBusy 对该玩家返回 true。
	// site 只进日志,标明是哪个撤回点。
	static void WithdrawHandoffMark(Guid playerId, uint64_t markEpoch, uint64_t requestedAtMs, const char *site);

	// 交接未成 **且已确认没有被放行**:摘 PlayerTravelHandoffComp + PlayerFrozenComp、撤回 handoff
	// 标记(WithdrawHandoffMark)、按情形回 tip。只有两类调用方:
	//   * handoff 标记的 SET 没发出(或确定没写)的失败分支 —— 此时不可能被放行(不变量 I0):BeginTravelHandoff 里的
	//     各分支(含 set_mark 阶段的晚发闸、同一代 SET 的 ERROR 应答 HandleTravelMarkWriteRejected)、存盘阶段看门狗、
	//     冻结上限的"标记未发出"分支(ExpireTravelFreeze);
	//   * ResolveTravelOutcome 核实过的两类正向证据(判定表 B4 epoch 未变 / B5 采纳回滚回执),调用前都已把 markEpoch 清 0
	//     (取证脚本已删掉本族标记,不再登记撤回)。
	// RequestTravelEnterScene 已不再调用它(那里 SET 已 OK,走 ConcludeHandoffAfterMarkSent)。已无交接意图时幂等 no-op。
	//   notifyFailure = true  → 回失败 tip 让客户端收起遮罩:跨 zone 传送未成 kZoneTravelTargetBusy,
	//                           同 zone 换图未成 kEnterSceneFailed(按组件里的 targetZoneId 自己分);
	//   notifyFailure = false → 静默解冻。只用于"同 zone 重发后落回了本节点":换图其实成功了,场景由
	//                           PlayerEnterGameNode → EnterScene 就地切换,再发失败 tip 是误报。
	static void AbortTravelHandoff(Guid playerId, const char *reason, bool notifyFailure = true);

	// handoff 标记的 SET 发出之后(requestedAtMs != 0),凡是"应答本身不足以判定去留"的情形都走这里,先判清楚有没有被
	// 放行(GO-2 判定表,cross-zone-scene-travel.md §12.8)。取证是**一次** EVAL travel_outcome::kLuaJudgeTravelOutcome:
	// 在同一原子步骤里只删本次交接这一族标记("E:t" 与回滚转写出来的 "E+2:t"),再读 owner_epoch 与 location。
	// 删除之后 scene_manager 的铸造 Lua(要求标记原样还在)与回滚 Lua(凭标记时同样要求)都再也不可能凭这一族标记动手,
	// 所以读到的就是终局。结论由 travel_outcome::ClassifyOwnership 给出;SET 发出后**只有两种正向证据能解冻**,
	// 其余一律不存盘销毁:
	//   B2  Redis 未连接 / 命令发不出 / 脚本 ERROR(含只读副本、MISCONF、OOM,#!lua 让它们整体被拒)/ 应答形状不对
	//       → 判不清:保持冻结,带原证据重挂应答看门狗,终局交给冻结硬上限(ConcludeHandoffAfterMarkSent)。
	//       解冻一个可能已被放行的玩家 = 同一名玩家在两处同时活着;销毁也没意义(重登同样要读 Redis)。
	//   B4  kUnchanged        epoch == 缓存值 → 正向证据一:markEpoch 清 0(脚本已删本族标记,不再登记撤回 —— 否则撤回
	//                         应答之前 Redis 一断,IsSceneChangeBusy 会把玩家挡 ~305s),AbortTravelHandoff 解冻
	//                         (kSucceeded 静默,其余回失败 tip)。
	//   B5  kRolledBackToSelf 回执 == 本次标记原文、markEpoch == 缓存值、epoch == markEpoch+2 == location.owner_epoch、
	//                         location 指回本节点本 zone → 正向证据二:采纳 epoch(PlayerOwnerEpochComp 取 max)→ markEpoch 清 0
	//                         → rolled_back_adopted +1 → AbortTravelHandoff 解冻 → SavePlayerToRedisImpl(强制写一次)。
	//                         这是"节点不得自己读 Redis 取 epoch"(CZ-3)的唯一例外,三条前提写死:原子脚本先删本族标记;
	//                         回执逐字节等于本次标记原文;location 指回本节点本 zone。
	//   B6  kReceiptAnomaly   回执是本次原文,B5 其余条件任一不成立(键被淘汰后补种 / 新旧 SM 混跑 / 数据写坏)→ 判不清:
	//                         rollback_receipt_anomaly +1,ConcludeHandoffAfterMarkSent(kReceiptAnomaly,tip + 34 + 不存盘销毁)。
	//                         可以踢:回执是本次原文说明回滚之后再无新落点,能用本会话发 EnterScene 的只有冻结中的本节点。
	//   B7  kReturnedToSelf   epoch 变了、无本次回执,location 却指回本节点本 zone(放行后又回来 / 回滚链 / 键被淘汰读成 0)
	//                         → returned_after_grant +1,沿用下面"已放行"分支不存盘销毁;不踢(会话可能正合法地绑在本节点)。
	//   B8  kMovedElsewhere / B9 kLocationUnknown → "已放行"分支(不变)。
	// "已放行"分支:kSucceeded 是同 zone 放行的正常收尾(granted,routine 销毁);其它证据计 granted_without_reply 后销毁,
	// 此时若同时满足下面全部条件,**销毁之前**先回失败 tip(kZoneTravelTargetBusy)并紧跟踢线 34,让客户端断线回选服重登:
	//   a) travel_outcome::ShouldResetClientOnGrant(跨 zone,且证据是 kFailed / kNoReply);
	//   b) 实体上没有 UnregisterPlayer(玩家已在退出,会话由退出流程收尾;纵深防御);
	//   c) location 能解析,且是本次交接的等待落点(travel_outcome::IsAwaitingPlacementOfHandoff)。
	//   任一不满足只销毁,并 LOG_WARN 说明为何不踢。location 在回调里只解析一次,判定与这里的判据共用。
	// 应答回显的 owner_epoch_after_rollback 绝不参与判定(应答可能丢;回滚后第三方可凭转写标记铸出更新的 epoch,
	// 号码匹配的应答照样带着旧回显)。requestedAtMs 是交接代际,回调到达时不符即 no-op。
	//
	// evidence(不给默认值,调用方必须说清凭什么进来)。入口处非 kNoReply 的证据记到交接组件上;实际裁决
	// 用 travel_outcome::EffectiveEvidence(组件上记下的优先),取证回调里按回调那一刻的组件再取一次 ——
	// 应答在看门狗那次核实已发出、尚未回来时到达,同样以应答证据为准:
	//   kSucceeded:同 zone 放行的成功应答(无 redirect),或进场路由已经把交接中的玩家就地放进了新场景
	//               (EnterScene 3.2 步,不等应答 —— 应答可能丢)。归属没动 = 重发后落回了本节点
	//               (同物理节点不铸造 epoch),静默解冻;归属已动 = 正常放行。
	//               这条路也必须经过取证脚本的删除:否则标记会带着没变的 epoch 再活 300s,玩家解冻后继续
	//               产生新状态,下一次跨节点 EnterScene 会凭这份旧标记被直接放行,新节点读到的是旧档。
	//   其它:归属没动 = 交接未成,解冻并回失败 tip;归属已动按上面各行。
	static void ResolveTravelOutcome(Guid playerId, uint64_t requestedAtMs, const char *reason,
									 travel_outcome::Evidence evidence);

	// ResolveTravelOutcome 取证脚本应答的处置(判定表 B2 形状非法 / B4–B9),只由它的 EVAL 回调调用。
	// 参数都是回调按值捕获的:id + 代际 + 发起取证时的缓存 epoch + 原因文本 + 发起时的证据(§11.7,不绑对象)。
	// 回调到达时实体不在 / 代际不符即 no-op;reply 为空(连接断开)按 B2 处理。
	static void JudgeTravelOutcomeReply(Guid playerId, uint64_t requestedAtMs, uint64_t cachedEpoch,
										const std::string &reasonText, travel_outcome::Evidence dispatchedEvidence,
										const redisReply *reply);

	// 冻结上限到期的单个处置(EnforceTravelFreezeCaps 收集阶段之后逐个调)。按 id 重查实体与交接组件、重算冻结时长、
	// 重新 DecideFreezeCap,不信任收集阶段的判定(前一个玩家的处置可能已经改了 registry)。
	static void ExpireTravelFreeze(Guid playerId, travel_freeze_cap::Clock::time_point now);

	// "handoff 标记的 SET 已发出、归属在本节点核实不了"的统一收口。
	// 前置条件:SET 已发出(requestedAtMs != 0,travel_freeze_cap::IsHandoffMarkSent);被破坏时 LOG_ERROR 并退回
	// AbortTravelHandoff(SET 未发出一侧,按 I0 安全)。
	// 动作:
	//   * 该玩家还有未落地的存盘 → 不销毁,返回 false(每次交接只告警 / 计数一次),由下一拍上限扫描重判。
	//     这是冻结硬上限唯一允许的例外:此刻不存盘销毁,那笔在途存盘随后落地会把盘改回中间态、成为永久状态;
	//   * 否则:会话活着且实体不在退出中 → 先发失败 tip + 踢线 34(reason = 同一个 tip:跨 zone kZoneTravelTargetBusy,
	//     同 zone kEnterSceneFailed),再 DestroyDeposedPlayer(不存盘),返回 true。**必须先踢再销毁**:
	//     DestroyDeposedPlayer 会摘会话,之后两条都发不出去。
	// 为什么不解冻:标记已发出,scene_manager 随时可能(或已经)凭它放行并推进 epoch,解冻 = 同一玩家两处同时活着
	//   (SET 发出后只有 ResolveTravelOutcome 的两类正向证据能解冻:原子删掉本族标记后读到 epoch 未变,或读到本次标记
	//   原文的回滚回执且交叉校验成立。本函数手里两样都没有)。
	// 为什么不发任何 Redis 命令、不撤回标记:走到这里多半就是 Redis 不可用;销毁之后"标记时刻的状态已落盘、本节点
	//   不再持有"成了事实,标记与断线释放标记 A1′ 同义 —— 重登挑到别的节点凭它过换手门,同节点重登由 A2′ 删掉;
	//   撤回只会伤活性(18 循环)。
	// 为什么同 zone 也踢:此刻 epoch 状态未知(已放行且会话已改绑 / 已放行但路由丢了 / 没放行 / 铸造后又回滚),
	//   后三种不踢就是一条哑连接,scene 又没有强制 gate 断开的 RPC;只有第一种会多踢一次,数据安全。
	// 调用点(site 只进日志、并作 DestroyDeposedPlayer 的 reasonTag):冻结上限(ExpireTravelFreeze)、enter_scene 阶段
	// 晚发闸、SET 之后发现没有 gate 会话、SET 之后发现没有 scene_manager(这三处都在 RequestTravelEnterScene)、
	// 取证读到本次回执但交叉校验不成立(JudgeTravelOutcomeReply,判定表 B6)。
	static bool ConcludeHandoffAfterMarkSent(Guid playerId, travel_freeze_cap::MarkSentSite site,
											 travel_freeze_cap::Clock::time_point now);

	// 疏散 / 排空的改派请求本体(EnterScene(zone=本 zone, scene_id=0))。票据按值传入:
	// 调用时本地实体多半已经销毁。
	static void SendEmergencyRelocateEnterScene(Guid playerId, const EmergencyRelocateTicket &ticket);

	// handoff 标记落地后向 scene_manager 请求 EnterScene(目标 zone / 场景)。gate / session 从实体上现取
	// (与 DispatchEmergencyRelocate 不同:交接中实体还活着,不需要提前抄票据)。
	// 发送前取关联号写进 PlayerTravelHandoffComp.enterSceneCorrelationId:只有回显这个号的应答才算本代交接的应答。
	// 只由 SET 的 OK 回调调用:走到这里标记确定已写,按骨架**不许解冻**。冻结已超过晚发窗口
	// (travel_freeze_cap::kDispatchWindow)、没有 gate 会话、没有 scene_manager 三种情形都不发 EnterScene,
	// 走 ConcludeHandoffAfterMarkSent(tip + 踢线 34 + 不存盘销毁)。
	static void RequestTravelEnterScene(Guid playerId);

	// 交接的 EnterScene 应答(DispatchEnterSceneReply 分到 kTravelHandoff / kLegacyTravelHandoff 才会进来,
	// 调用时实体有效、交接组件在)。
	//   * requestedAtMs == 0 / 组件不在   → 不可达(号只在 SET 回调里、requestedAtMs 置位之后才写),防御性 LOG_ERROR 后返回;
	//   * error_code != 0               → 不能直接解冻:失败应答不证明 scene_manager 没铸造过 epoch
	//                                     (例如路由发送失败后的回滚本身也可能失败,或回滚已把 epoch 推进两格)。
	//                                     日志带上 owner_epoch_after_rollback 回显,然后以证据 kFailed 走
	//                                     ResolveTravelOutcome 核实后再决定解冻 / 采纳回滚 / 销毁;跨 zone 且确认已被
	//                                     放行时,销毁之前先回失败 tip 并紧跟踢线 34(客户端没拿到重定向,
	//                                     不踢就挂在哑连接上);
	//   * 带 redirect                   → 跨 zone 放行,scene_manager 已推进 epoch,本节点不再持有该玩家:
	//                                     与退出同款销毁(摘场景 / 摘会话 / 销毁实体),**不再存盘**;
	//   * 无错无 redirect、目标是本 zone → 同 zone 放行的正常形态。是交给了别的节点还是重发后落回了
	//                                     本节点,应答本身分不出来,以 kSucceeded 走 ResolveTravelOutcome 按 epoch 判;
	//   * 无错无 redirect、目标是别的 zone → 协议异常,LOG_ERROR 后以 kAnomalous 走 ResolveTravelOutcome
	//                                     (已被放行时只销毁、不踢线)。
	// correlated:true = 应答号与本代交接记下的号相等(kTravelHandoff);false = 旧版 SM 没回显、按 player_id
	// 退回来的(kLegacyTravelHandoff)。本项只用于日志。GO-2 **从不凭应答回显采纳** epoch —— correlated 为 true 也不采纳:
	// EnterSceneResponse.owner_epoch_after_rollback 只进日志,采纳只认 ResolveTravelOutcome 原子取证读到的 Redis 回执
	// (判定表 B5),"外来 epoch 进入裁决"在任何版本组合下都不会发生。
	static void HandleTravelEnterSceneReply(entt::entity playerEntity, Guid playerId,
											const ::scene_manager::EnterSceneResponse& resp, bool correlated);

	// 普通换图的 EnterScene 应答(DispatchEnterSceneReply 分到 kSceneChange / kLegacySceneChange 才会进来;
	// 在途组件已由 Dispatch 按值抄下并摘掉,target 按值传入,从类型上排除 remove 之后引用悬空):
	//   * playerRequested=false          → 队伍跟随的应答:被拒只记日志,到此为止(跟随不跨节点拉人,team-system.md DV-6);
	//   * error_code == 18               → 目标场景在别的节点:用记下的目标 StartTravelHandoff(本 zone)——§11.2 被动交接链;
	//   * 其它非 0                       → 换图失败,回 kEnterSceneFailed tip(EnterSceneC2S 的同步应答早已返回"已受理");
	//   * 0                              → 同节点换图成功,组件已摘,无事可做。
	static void HandleSceneChangeEnterSceneReply(entt::entity playerEntity, Guid playerId,
												 PlayerSceneChangeInFlightComp target,
												 const ::scene_manager::EnterSceneResponse& resp);

	// EnterScene 应答看门狗:一次性定时器,到期时若同一代(requestedAtMs 相同)的交接仍在途,
	// 带着挂载时给的证据与原因走 ResolveTravelOutcome。RequestTravelEnterScene 首次挂载传 kNoReply;
	// ResolveTravelOutcome 因 Redis 不可用重挂时传**当前**证据,不在重挂时翻成超时。
	// 首次挂的看门狗不取消、总是先于重挂的到期:它带进去的 kNoReply 会被交接组件上记下的应答证据覆盖
	// (travel_outcome::EffectiveEvidence),所以重挂那个随后多半是代际不符的 no-op,这是预期行为。
	// 只按值捕获 playerId + 代际 + 证据 + 原因文本,回调里按 id 回查实体(§11.7)。
	// 预算 travel_freeze_cap::kReplyBudget(30s):muduo 定时器唤醒(按墙钟),按单调截止时刻复核 —— 提前 1s 以上醒来
	// 就按剩余时间重挂(计 watchdog_early_fire),容差 1s 以内直接执行。重挂次数不封顶,整段冻结由冻结硬上限封顶。
	static void ArmTravelReplyWatchdog(Guid playerId, uint64_t requestedAtMs, travel_outcome::Evidence evidence,
									   std::string reason);

	// 存盘阶段看门狗:覆盖"已冻结、存盘在途、交接还没发起(requestedAtMs == 0)"这一段 ——
	// 应答看门狗要到 EnterScene 发出才挂,Redis 长时间不可用时本看门狗是这一段的主兜底;它没能按时触发时
	// (例如墙钟回拨推迟了 muduo 定时器),存盘晚于 35s 才落地的由晚发闸(BeginTravelHandoff 的 set_mark 阶段)
	// 拦下不写标记,一直不落地的由 70s 冻结上限的"标记未发出 → 解冻"分支(ExpireTravelFreeze)收住。
	// 到期仍未发起就直接 AbortTravelHandoff(标记没写,不可能已被放行)。代际用 PlayerFrozenComp.frozenAtMs。
	// 预算 travel_freeze_cap::kSaveBudget(30s),与应答看门狗同样按单调截止时刻复核(容差 1s)。
	static void ArmTravelSaveWatchdog(Guid playerId, int64_t frozenAtMs);
};
