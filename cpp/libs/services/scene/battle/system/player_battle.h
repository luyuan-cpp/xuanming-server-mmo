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
//   - GetRecentBattleResults / AckBattleResults:本人近期战斗结果的查询与已读确认,结果随结算同一次存档提交
//     (docs/design/battle-result-recovery.md);
//   - OnPlayerEnterScene:登录/重连后置钩子(按锁重建冻结,再给战斗中换会话的客户端推重连提示);
//   - reaper:低频扫描 InBattleComp,PREPARING 看 prepare_deadline_ms、FIGHTING 看 FreezeExpiresAtMs,
//     过期即作废解冻(§3.2 补偿矩阵)。
//
// 冻结语义:InBattleComp 存在 = 玩家有一场战斗在途,拒绝再次备战/切场景/跨 zone 迁移;
// 摘除条件 = 结算已随存档提交 / match 取消(仅 PREPARING)/ reaper 超时作废,是玩家再次进战斗的唯一放行条件。
//
// battle:lock 语义:值 = battle_id,所有删除/续期一律"值匹配才操作"(Lua 原子 GET+DEL /
// GET+EXPIRE),迟到的取消/结算/作废绝不会误伤玩家后续的新战斗锁。
// 伴生 battle:ctx:{player_id} = InBattleComp 序列化(scene 私有,与锁同一条 Lua 写入 / 续期 / 删除,同 TTL):
// 实体没了但锁还在的两条路径靠它重建冻结 —— ①玩家战斗中完整下线再登录(组件不落库);
// ②备战到期 reaper 只摘组件不删锁,迟到的 BattleConfirmedEvent 按锁值重建 FIGHTING。
// 写锁与续期没问到时有界重试,续期只延长不缩短(turn-battle-gap-closure.md §10);
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

// 定义在 battle_lock_rules.h(按锁重建的裁决、应答形状);底层类型必须与定义一致。
namespace battle_lock
{
	enum class LockRebuildDecision : uint8_t;
	enum class ReplyShape : uint8_t;
}

// 消息定义在 proto/scene/scene.proto(无 package → 全局命名空间),
// muduo Scene 服务与 SceneNodeGrpc 控制面共享同一份类型。
class PrepareBattleRequest;
class PrepareBattleResponse;
class CancelBattlePrepareRequest;
class BattleConfirmedEvent;
class BattleSettlementData;
class BattlePlayerSnapshot;
// 近期战斗结果的客户端协议(proto/scene/player_battle_result.proto,无 package → 全局命名空间)。
class GetRecentBattleResultsResponse;
class AckBattleResultsRequest;
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
	// 同时公开这一局的近期战斗结果(结果在应用时已随同一次存档写进去,确认提交之前不对客户端公开)。
	static bool CompleteInboxSettlement(entt::entity player, const std::string& payload);

	// ── 近期战斗结果(docs/design/battle-result-recovery.md)──────────────────────────────
	// 结果在 ApplySettlementToEntity 的同一帧写进持久组件 PlayerBattleResultComp(随同一次存档提交),
	// CompleteInboxSettlement 之后才对客户端公开。两个入口都只认会话里的本人(player 由会话解析),只在 loop 线程调用。
	//
	// 查询:按 result_seq 降序填至多 10 条已公开的结果。只读,不改变实体的组件集合。
	// 返回 kSuccess,或 kEntityIsNull(实体无效)/ kServiceUnavailable(结果组件缺失 = 加载不变量被破坏,记 ERROR)。
	static uint32_t GetRecentBattleResults(entt::entity player, ::GetRecentBattleResultsResponse& response);
	// 确认已展示:把请求里已公开的局标成已读;不认识的局忽略;幂等。不触发存盘,已读标记随下一次存盘落地。
	// 返回 kSuccess,或 kArraySizeTooLargeInMessage(超过 10 个 battle_id)/ kFeatureUnavailable(冻结、交接、
	// 退出中或没有归属围栏:本节点此刻不改这名玩家的存档)/ kEntityIsNull / kServiceUnavailable(同上)。
	static uint32_t AckBattleResults(entt::entity player, const ::AckBattleResultsRequest& request);
	// match -> scene:冻结玩家并返回战斗快照。
	// 校验:在线(实体在本节点且有 gate 会话)/ 无 InBattleComp / 无换图在途
	// (PlayerLifecycleSystem::IsSceneChangeBusy:交接冻结、普通 EnterScene / 镜像建场景应答未回等,回 kFeatureUnavailable)。
	// 成功:挂 InBattleComp{state=PREPARING, deadline_ms, prepare_deadline_ms(0 时取 deadline_ms)}
	// + 一条 Lua 写 battle:lock:{player_id}=battle_id 与 ctx(EX 按 prepare_deadline_ms+60s,没问到有界重试;
	// 本实体上的权威判定仍是 InBattleComp)+ 返回 BattlePlayerSnapshot(含 table_fingerprint)
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
	// deadline_ms=event.deadline_ms、battle:lock 条件续期 + ctx 覆写(值==battle_id 才延长到
	// 冻结到期时刻 - now + 60s,只延长不缩短,冻结到期见 FreezeExpiresAtMs);
	// 已 FIGHTING -> 状态与期限不动,按组件上的期限再做一次条件续期(补上首次续期的丢失);
	// battle_id 不匹配 -> 幂等忽略;
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
	// 锁脚本观察点(单测用)。写锁 / 续期 / 重建复核每次准备发送时(算出 TTL 与 ctx 之后、判断 Redis 是否就绪之前)各回调一次,
	// 单测不连 Redis 也能盯住「这一次会发什么」:op 为 "write_lock" / "renew_lock" / "rebuild_recheck"。
	// 只有 renew_lock 去重:首发让给同 (player_id, battle_id) 的在途续期链时不回调 —— 那一次什么都不发;
	// 其余两种不去重,每一次发送(首发与重试)都回调。生产不注入(nullptr);传 nullptr 恢复。
	using LockScriptObserver = void (*)(const char* op, uint64_t playerId, uint64_t battleId, uint64_t ttlSec,
										const std::string& ctxPayload);
	static void SetLockScriptObserverForTest(LockScriptObserver observer);
	// 锁写入 / 续期重试的调度器注入点(单测用)。生产不注入(nullptr):没问到之后的重试经当前线程的 EventLoop::runAfter 排,
	// 当前线程没有 loop 就放弃重试(metric=battle_lock_write_gave_up)。
	// 注入时改调 fn(delayMs, fire),即使当前线程没有 loop 也照常排;单测可以记下 delayMs、按需调用 fire 推进重试链。
	// fire 只捕获 id 与字节,被调用时按 player_id 现取实体重跑门禁与发送,可以在任何时刻(含实体已销毁之后)调用,
	// 但必须在排它的同一线程上调用(实体注册表、续期在途登记都是 thread_local)。
	// 注意:续期(renew_lock)链从首发起就登记为在途,直到链走到终态才摘 —— 注入实现若丢弃 fire 不调用,
	// 同 (player_id, battle_id) 之后的续期首发会一直让路。传 nullptr 恢复为 EventLoop::runAfter。
	using LockRetrySchedulerFn = bool (*)(uint64_t delayMs, std::function<void()> fire);
	static void SetLockRetrySchedulerForTest(LockRetrySchedulerFn fn);
    // 与生产 Lua 发送同一接缝。返回 REDIS_OK 才承诺回调，回复仅在回调期间借用。
    using LockScriptSender = int (*)(const char*, uint64_t, uint64_t, uint64_t, const std::string&, std::function<void(redisReply*)>);
    static void SetLockScriptSenderForTest(LockScriptSender fn);
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
	// 「整笔已应用」含按规则跳过的子效果,跳过的项逐项记日志、其余照常:被获得封禁的币种与掉落(封禁是终局业务决定,
	// 解封后不补发)、配置行已删的物品、已不可用的宠物效果。
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

	// 备战写锁(PrepareBattle 唯一调用点,单测走同一个入口):实体上 InBattleComp 仍是这一局才写,TTL 与 ctx 按组件现状算;
	// 没问到时有界重试,重发前重跑这道门禁。返回这一次算出的 TTL 秒(只给调用方打日志),门禁不过时返回 0。
	static uint64_t WriteBattleLockOnPrepare(uint64_t playerId, uint64_t battleId);

	// 重建复核应答的判定与撤销,生产的 hiredis 回调与单测走同一个函数。
	// integer 只在 shape == kInteger 时有意义;attemptNumber 是这次应答对应第几次发送(从 1 起):
	//  - 非整数形状(没问到):不是结论,什么都不做 —— 重试由发送方排(重发要带首发时刻算预算);
	//  - 整数非 0:锁仍是这一局的,重建成立,什么都不做;
	//  - 整数 0 且 attemptNumber == 1(首发):锁在读锁与复核之间被删或易主 —— 按 player_id 现取实体,有效、身份仍是
	//    playerId、InBattleComp 仍是 battleId 才 RemoveInBattleComp(metric=battle_freeze_rebuild_reverted);
	//  - 整数 0 且 attemptNumber != 1(重试):可能只是锁按 TTL 自然过期,同样复核之后只记 WARN、保留冻结交给 reaper。
	static void OnRebuiltFreezeRecheckReply(uint64_t playerId, uint64_t battleId, uint32_t attemptNumber,
											battle_lock::ReplyShape shape, int64_t integer);
};
