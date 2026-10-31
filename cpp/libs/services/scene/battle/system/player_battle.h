#pragma once

// 回合制战斗 scene 侧集成(设计文档 docs/design/turn-based-battle-server.md §5.3)。
//
// PlayerBattleSystem 是 scene 节点上回合制战斗的唯一业务入口:
//   - PrepareBattle / CancelBattlePrepare:match 服务 gather 管线的冻结/解冻(D4 结算串行化);
//   - ConfirmBattle:battle 节点 CreateBattle 成功后的确认事件,PREPARING -> FIGHTING
//     并把作废期限从 prepare_deadline_ms 切到正式 deadline_ms(cross-zone-matchmaking.md §10);
//   - ApplyInboxSettlement / CompleteInboxSettlement:结算的唯一应用者(宪法 §7 新增不变量 4)。结算只走一条路:
//     battle 提交 match SQL,match 发件箱投 player_inbox,这里应用后随存档提交,确认之后才解冻放锁
//     (player-data-tidb-scaling-plan.md §5.4);
//   - OnPlayerEnterScene:登录/重连后置钩子(按锁重建冻结,再给战斗中换会话的客户端推重连提示);
//   - reaper:低频扫描 InBattleComp,PREPARING 看 prepare_deadline_ms、FIGHTING 看 FreezeExpiresAtMs,
//     过期即作废解冻(§3.2 补偿矩阵)。
//
// 冻结语义:InBattleComp 存在 = 玩家有一场战斗在途,拒绝再次备战/切场景/跨 zone 迁移;
// 摘除条件 = 结算已随存档提交 / match 取消(仅 PREPARING)/ reaper 超时作废,是玩家再次进战斗的唯一放行条件。
//
// battle:lock 语义:值 = battle_id,所有删除/续期一律"值匹配才操作"(Lua 原子 GET+DEL /
// GET+EXPIRE),迟到的取消/结算/作废绝不会误伤玩家后续的新战斗锁。
// 伴生 battle:ctx:{player_id} = InBattleComp 序列化(scene 私有,与锁同 SET / 同 TTL / 同 DEL):
// 实体没了但锁还在的两条路径靠它重建冻结 —— ①玩家战斗中完整下线再登录(组件不落库);
// ②备战到期 reaper 只摘组件不删锁,迟到的 BattleConfirmedEvent 按锁值重建 FIGHTING。
// 谁在什么时机放锁见 battle_lock_rules.h 文件头。
//
// 线程模型:所有方法都必须在 scene 节点 loop 线程调用(gRPC 入口:muduo_grpc 服务端在 loop 线程上派发)。

#include <cstdint>
#include <functional>
#include <string>

#include "entt/src/entt/entity/entity.hpp"
#include "engine/core/type_define/type_define.h"

namespace muduo
{
	namespace net
	{
		class EventLoop;
	}
}

// hiredis 的应答结构(hiredis.h 里是 typedef struct redisReply {...} redisReply),测试注入点的签名只用到指针。
struct redisReply;

// 定义在 battle_lock_rules.h(按锁重建的裁决);底层类型必须与定义一致。
namespace battle_lock
{
	enum class LockRebuildDecision : uint8_t;
}

// 消息定义在 proto/scene/scene.proto(无 package → 全局命名空间),
// muduo Scene 服务与 SceneNodeGrpc 控制面共享同一份类型。
class PrepareBattleRequest;
class PrepareBattleResponse;
class CancelBattlePrepareRequest;
class BattleConfirmedEvent;
class BattleSettlementData;
class BattlePlayerSnapshot;
struct PlayerInboxExecResult;

class PlayerBattleSystem
{
public:
	// 只由 Match 持久 Inbox 投递；效果与 Inbox 永久域水位同快照提交。
	// 暂缺任一已实现效果的前置条件时整笔DEFER零副作用；未知嵌套字段按版本错位拒执行。
	static PlayerInboxExecResult ApplyInboxSettlement(entt::entity player, const std::string& payload);
	// 仅在原完整快照经同E/q双SQL ACK确认后调用(PlayerInboxSystem);不重复效果,不动另一局冻结。
	// 这是结算路径上**唯一**的解冻放锁点:条件删 battle:lock + ctx、摘同一局的 InBattleComp、推 BattleEndS2C。
	// 组件已不在(reaper 已判作废 / 实体是新建的)时照样条件删锁与推面板,锁确实删掉了就补一次组队跟随。
	static bool CompleteInboxSettlement(entt::entity player, const std::string& payload);
	// match -> scene:冻结玩家并返回战斗快照。
	// 校验:在线(实体在本节点且有 gate 会话)/ 无 InBattleComp / 无换图在途
	// (PlayerLifecycleSystem::IsSceneChangeBusy:交接冻结、普通 EnterScene / 镜像建场景应答未回等,回 kFeatureUnavailable)。
	// 成功:挂 InBattleComp{state=PREPARING, deadline_ms, prepare_deadline_ms(0 时取 deadline_ms)}
	// + SET battle:lock:{player_id}=battle_id(EX 按 prepare_deadline_ms+60s,咨询性锁,
	// 权威判定仍是 InBattleComp)+ 返回 BattlePlayerSnapshot(含 table_fingerprint)
	// + response.table_fingerprint。
	// 失败:response.error_message 填拒绝原因,不留任何冻结痕迹。
	static void PrepareBattle(const ::PrepareBattleRequest& request, ::PrepareBattleResponse& response);

	// match -> scene:gather 失败补偿。battle_id 与 InBattleComp 匹配且 state==PREPARING 才
	// 摘组件 + 条件删锁;不匹配(迟到取消)忽略;已 FIGHTING(确认已到 = 房间建成过)拒绝并记
	// metric=battle_cancel_rejected_fighting —— 这是 match CreateBattle 超时后的过期回滚,解冻会让
	// 玩家同时进两局。玩家实体不在时按锁值读 ctx,ctx 未 FIGHTING 才条件删锁(幂等)。
	static void CancelBattlePrepare(const ::CancelBattlePrepareRequest& request);

	// battle --Kafka--> scene:CreateBattle 成功确认(battle 侧开局后一段时间内周期补发,幂等)。
	// 在线且 InBattleComp.battle_id 匹配且 state==PREPARING -> state=FIGHTING、
	// deadline_ms=event.deadline_ms、battle:lock 条件续期 + ctx 覆写(值==battle_id 才 EXPIRE 到
	// 冻结到期时刻 - now + 60s,冻结到期见 FreezeExpiresAtMs);已 FIGHTING / battle_id 不匹配 -> 幂等忽略;
	// 在线但无 InBattleComp(备战到期被摘 / 登录重建未命中)-> 锁值==battle_id 则重建 FIGHTING 冻结;
	// 玩家不在本节点 -> 锁值匹配才续期,并把 FIGHTING/deadline 落进 ctx 供登录重建。
	static void ConfirmBattle(const ::BattleConfirmedEvent& event);

	// 登录/重连后置钩子(player_lifecycle.cpp EnterScene 末尾调用):
	//   1) 无 InBattleComp -> 读 battle:lock + ctx,锁在则重建 InBattleComp,FIGHTING 时推 BattleReconnectS2C;
	//      没问到(空应答 / 命令没发出去 / Redis 不可用)-> 记入登录复查,reaper 节拍重读(2026-10-16);
	//   2) RECONNECT / REPLACE 且仍有 InBattleComp -> 推 BattleReconnectS2C。
	//   只推提示(turn-based §22 D72):客户端凭 battle_id 补签票据、重建直连后 GetBattleState 补拉。
	// 结算不在这里读:它只经收件箱,PlayerInboxSystem::OnPlayerLanded 落地即拉。
	static void OnPlayerEnterScene(entt::entity player, uint32_t enterGsType);

	// 冻结拦截查询:InBattleComp 存在即在战(切场景/跨 zone 等入口用)。
	static bool IsInBattle(entt::entity player);

	// reaper:30s 低频定时器,扫描 InBattleComp:PREPARING 按 prepare_deadline_ms 过期只摘组件、
	// 锁保留到 TTL(prepare_deadline+60s)自然过期给迟到确认留重建余量;FIGHTING 按 FreezeExpiresAtMs
	// (deadline_ms + 终局提交宽限 + 收件箱投递余量)过期摘组件 + 条件删锁;最后重跑登录复查(RunLoginRechecks)。
	// 结构化日志 metric=battle_prepare_expired / battle_freeze_expired。
	// 不进 20FPS 帧循环;在 main.cpp 里随节点启动注册、随节点停止注销。
	static void StartReaper(muduo::net::EventLoop* loop);
	static void StopReaper();

	// reaper 扫描间隔(秒)。战斗作废的时间精度要求是"分钟级",30s 足够。
	static constexpr double kReaperIntervalSec = 30.0;

	// ── 冻结何时算到期(reaper 判作废与按锁重建续锁共用,2026-10-30)──────────────────────
	// Battle 在 deadline_ms 才强制平局,Match 还允许 deadline 之后 kTerminalOutcomeGraceMs 内提交 COMPLETE,
	// 结算再经 Match 发件箱入箱、本节点拉取应用又要一段时间。FIGHTING 冻结若在 deadline 一过就摘,reaper 节拍
	// 很可能落在「deadline 之后、收件箱结算应用之前」:资产通道的战斗闸(只看 InBattleComp)提前放开,玩家能先把
	// 本局用掉的药挂托管扣走,结算晚到时消耗只能按当时持有量夹紧、HP/MP 按终值覆盖 —— 可逐局刷的有界资产复制。
	// 所以 FIGHTING 的到期 = deadline_ms + 终局提交宽限 + 收件箱投递余量;PREPARING 判据不变(备战期没有终局可等)。
	//
	// 终局提交宽限:须与 go/match/internal/authority/completion.go 的 terminalOutcomeGraceMs 及
	// Java GlobalMatchStore.TERMINAL_OUTCOME_GRACE_MS 同值(三处任一改动,其余两处同批改)。
	static constexpr uint64_t kTerminalOutcomeGraceMs = 60ULL * 1000ULL;
	// 收件箱投递余量:COMPLETE 落库之后 Match 发件箱入箱、收件箱提醒、本节点 Fetch 并应用结算所需的余量。
	static constexpr uint64_t kInboxSettlementDeliveryMarginMs = 60ULL * 1000ULL;
	// 冻结的到期时刻(Unix 毫秒,与 InBattleComp 的期限同一时钟)。返回 0 = 没有期限,reaper 不判到期。
	//   preparing = true:prepare_deadline_ms,为 0(旧版 match / 存量组件)时退回 deadline_ms;
	//   否则(FIGHTING):deadline_ms + kTerminalOutcomeGraceMs + kInboxSettlementDeliveryMarginMs,上溢时饱和。
	// reaper 在 nowMs 严格大于它时才摘冻结;按锁重建与确认续期按它算锁 TTL(锁必须活得比冻结久)。纯函数。
	static constexpr uint64_t FreezeExpiresAtMs(bool preparing, uint64_t deadlineMs, uint64_t prepareDeadlineMs)
	{
		if (preparing)
		{
			return prepareDeadlineMs != 0 ? prepareDeadlineMs : deadlineMs;
		}
		if (deadlineMs == 0)
		{
			return 0;
		}
		constexpr uint64_t kWindowMs = kTerminalOutcomeGraceMs + kInboxSettlementDeliveryMarginMs;
		return deadlineMs > UINT64_MAX - kWindowMs ? UINT64_MAX : deadlineMs + kWindowMs;
	}

	// 条件删锁命令的发送注入点。生产恒为空:走 tlsPlayerRedis 发条件删锁脚本。
	// 单测注入后,删锁在判 Redis 可用之前优先交给它:返回 true = 已接管,回包由调用方经 onReply 回灌 —— 进的是与生产
	// 同一个具名回包处理函数(锁确实删掉时的跟随唤醒);返回 false = 没发出去,onReply 不会被调。
	// 传 nullptr 恢复生产实现。生产代码**不调用**这些 setter。
	using BattleLockReleaseSender = bool (*)(uint64_t playerId, uint64_t battleId, std::function<void(redisReply*)> onReply);
	static void SetBattleLockReleaseSenderForTest(BattleLockReleaseSender sender);
	// 战斗系统的组队跟随唤醒(摘冻结、放锁之后补一次跟随检查)注入点。生产恒为 PlayerTeamSystem::OnBattleFreezeCleared;
	// 单测用它观察「锁真正放掉之后补一次跟随」有没有发生、落在哪个实体上。传 nullptr 恢复生产实现。
	using FollowWakeFn = void (*)(entt::entity);
	static void SetFollowWakeFnForTest(FollowWakeFn fn);
	// battle:lock 的 EX 在冻结到期时刻(FreezeExpiresAtMs:PREPARING 是备战期限,FIGHTING 是 deadline + 宽限 + 余量)
	// 之上的余量(秒):锁必须活得比 InBattleComp 久,避免"组件还在、锁先没了"让 match 放进第二场,
	// 也保证宽限期内重登仍能按锁重建冻结。
	static constexpr uint32_t kLockExtraTtlSec = 60;

private:
	friend class PlayerBattleSettlementTestAccess; // 内存测试共用应用入口，无网络测试钩子
	// 组 BattlePlayerSnapshot(属性/等级/技能/参战 buff/道具副本/路由信息/配表指纹)。
	// 失败时把拒绝原因写进 errorTipId 并返回 false(缺基础属性组件/缺会话快照)。
	static bool BuildBattleSnapshot(entt::entity player, ::BattlePlayerSnapshot& snapshot, uint32_t& errorTipId);

	// 把结算终值落到玩家实体。true = 整笔已应用;false = 整笔零副作用,调用方按 DEFER 处理。
	// 去重不在这里:ApplyInboxSettlement 由收件箱状态机驱动,同一条目按永久域水位只执行一次。
	static bool ApplySettlementToEntity(entt::entity player, const ::BattleSettlementData& settlement);

	// 战斗中换会话 / 登录重建为 FIGHTING:经大厅会话推 BattleReconnectS2C{battle_id}
	// (scene→gate TCP,不经 Kafka)。gate 不中继战斗,没有绑定要重建(turn-based §22 D72);
	// 客户端凭 battle_id 经 MatchService.RequestBattleTicket 补签并重建直连。无 InBattleComp 时 no-op。
	static void NotifyBattleReconnectToClient(entt::entity player);

	// 按锁值匹配重建 FIGHTING 冻结(迟到确认 / 在线无组件路径):锁值==battleId 才读 ctx 重建
	// InBattleComp、条件续期并覆写 ctx;deadlineMsHint 非 0 时覆盖 ctx 的 deadline。reason 只进日志。
	// 与登录重建共用裁决 ClassifyLockRebuild:本实体已判作废的局不重建、补删锁(2026-10-19)。
	static void RebuildBattleFreezeFromLock(entt::entity player, uint64_t playerId, uint64_t battleId,
											uint64_t deadlineMsHint, const char* reason);

	// 登录重建:读 battle:lock + ctx(不预设 battle_id),锁在则按 ctx 重建 InBattleComp;
	// FIGHTING 时推重连提示(不要求 battle_node_id 已知)。只在无 InBattleComp 时才有事可做。
	// 读不了锁(Redis 不可用 / 命令没发出去 / 空应答)记入登录复查;EVAL 的 ERROR 仍按无锁处理(既有取舍,
	// 见 battle_lock_rules.h 的 ClassifyLoginLockRead)。
	// 锁指向本实体上 reaper 已判作废的那一局时不重建、补一次条件删锁(ClassifyLockRebuild,2026-10-19)。
	static void RestoreBattleFreezeOnLogin(entt::entity player, uint64_t playerId);

	// reaper 判定 FIGHTING 到期之后的收尾:记下这一局已作废(BattleFreezeExpiredComp)→ 条件删锁 + 摘冻结。
	// 这一局的结算若之后才到,收件箱照常应用(CompleteInboxSettlement 组件不在也能收尾)。只由 reaper 调用。
	static void ReleaseExpiredFightingFreeze(entt::entity player, uint64_t playerId, uint64_t battleId);

	// reaper 第一遍:按 FreezeExpiresAtMs 收集 nowMs 时已到期的冻结,PREPARING 只摘组件(锁留到 TTL),
	// FIGHTING 走 ReleaseExpiredFightingFreeze。nowMs 由调用方给(reaper 取 TimeSystem::NowMillisecondsUTC),
	// 抽出来只为单测能用显式时刻驱动;只由 reaper 调用。
	static void ReapExpiredFreezes(uint64_t nowMs);

	// reaper 第二遍:Redis 可用时,对记入登录复查的玩家逐个摘标记并重新读锁;
	// Redis 不可用时标记保留。退出中(UnregisterPlayer)的实体摘标记不重跑,留给下次进场;
	// 跨 zone 冻结 / 交接中的实体标记保留、本轮不跑(交接完成随实体销毁,撤销则下个节拍再跑)。
	static void RunLoginRechecks();

	// 是否记着登录复查(只读;单测与排障用)。
	static bool IsLoginRecheckPending(entt::entity player);
	// 本实体上 reaper 最近判作废的 FIGHTING 局;没有返回 0(只读;单测与排障用)。
	static uint64_t ExpiredFightingBattleId(entt::entity player);
	// 两个按锁重建入口共用的裁决接线(本实体的作废槽位 → ClassifyLockRebuild),给单测走同一个函数。只读。
	static battle_lock::LockRebuildDecision DecideLockRebuildFor(entt::entity player, uint64_t lockBattleId);
};
