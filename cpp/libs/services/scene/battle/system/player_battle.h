#pragma once

// 回合制战斗 scene 侧集成(设计文档 docs/design/turn-based-battle-server.md §5.3)。
//
// PlayerBattleSystem 是 scene 节点上回合制战斗的唯一业务入口:
//   - PrepareBattle / CancelBattlePrepare:match 服务 gather 管线的冻结/解冻(D4 结算串行化);
//   - ConfirmBattle:battle 节点 CreateBattle 成功后的确认事件,PREPARING -> FIGHTING
//     并把作废期限从 prepare_deadline_ms 切到正式 deadline_ms(cross-zone-matchmaking.md §10);
//   - ApplySettlement:battle 节点结算事件的唯一应用者(宪法 §7 新增不变量 4);
//   - OnPlayerEnterScene:登录/重连后置钩子(先应用离线挂起结算,再给战斗中换会话的客户端推重连提示);
//   - reaper:低频扫描 InBattleComp,PREPARING 看 prepare_deadline_ms、FIGHTING 看 deadline_ms,
//     过期即作废解冻(§3.2 补偿矩阵)。
//
// 冻结语义:InBattleComp 存在 = 玩家有一场战斗在途,拒绝再次备战/切场景/跨 zone 迁移;
// 摘除条件 = 结算已应用 / match 取消(仅 PREPARING)/ reaper 超时作废,是玩家再次进战斗的唯一放行条件。
//
// battle:lock 语义:值 = battle_id,所有删除/续期一律"值匹配才操作"(Lua 原子 GET+DEL /
// GET+EXPIRE),迟到的取消/结算/作废绝不会误伤玩家后续的新战斗锁。
// 伴生 battle:ctx:{player_id} = InBattleComp 序列化(scene 私有,与锁同 SET / 同 TTL / 同 DEL):
// 实体没了但锁还在的两条路径靠它重建冻结 —— ①玩家战斗中完整下线再登录(组件不落库);
// ②备战到期 reaper 只摘组件不删锁,迟到的 BattleConfirmedEvent 按锁值重建 FIGHTING。
// 结算在线路径无 InBattleComp 时同样按锁值匹配应用而不是丢弃。
//
// 线程模型:所有方法都必须在 scene 节点 loop 线程调用(gRPC 入口:muduo_grpc 服务端在 loop 线程上派发)。

#include <cstdint>
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

// 定义在 services/battle/settlement/settlement_outbox.h(按锁重建的裁决);底层类型必须与定义一致。
namespace battle_settlement
{
	enum class LockRebuildDecision : uint8_t;
}

// 消息定义在 proto/scene/scene.proto(无 package → 全局命名空间),
// muduo Scene 服务与 SceneNodeGrpc 控制面共享同一份类型。
class PrepareBattleRequest;
class PrepareBattleResponse;
class CancelBattlePrepareRequest;
class BattleSettlementEvent;
class BattleConfirmedEvent;
class BattleSettlementData;
class BattlePlayerSnapshot;
struct PlayerInboxExecResult;

class PlayerBattleSystem
{
public:
	// v1只由Match持久Inbox投递；效果与Inbox永久域水位同快照提交，不碰legacy有限账本。
	// 暂缺任一已实现效果的前置条件时整笔DEFER零副作用；未知嵌套字段按版本错位拒执行。
	static PlayerInboxExecResult ApplyInboxSettlement(entt::entity player, const std::string& payload);
	// 仅在原完整快照经同E/q双SQL ACK确认后调用；不重复效果，不动另一局冻结。
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
	// deadline-now+60s);已 FIGHTING / battle_id 不匹配 -> 幂等忽略;
	// 在线但无 InBattleComp(备战到期被摘 / 登录重建未命中)-> 锁值==battle_id 则重建 FIGHTING 冻结;
	// 玩家不在本节点 -> 锁值匹配才续期,并把 FIGHTING/deadline 落进 ctx 供登录重建。
	static void ConfirmBattle(const ::BattleConfirmedEvent& event);

	// battle --Kafka--> scene:应用战斗结算。
	// InBattleComp.battle_id 匹配才应用(HP/MP 终值 + 金钱;经验/道具见 open_issues),
	// 然后摘组件 + DEL 锁 + 推 BattleEndS2C;InBattleComp 存在但 id 不匹配丢弃并告警;
	// 在线但无 InBattleComp -> 锁值==battle_id 才应用(下线再登录 / 备战到期后的迟到结算),
	// 否则按 at-least-once 重复投递丢弃;
	// 玩家实体不在 -> 校验锁值后写 battle:settlement:pending:{player_id}(不过期,并进积压索引;
	// landing-recovery-sync-digest.md §7)。
	static void ApplySettlement(const ::BattleSettlementEvent& event);

	// 登录/重连后置钩子(player_lifecycle.cpp EnterScene 末尾调用):
	//   1) 查 Redis 挂起结算,有则应用并清理(先应用挂起结算再放开排队,§3.2);
	//      没有挂起结算且无 InBattleComp -> 读 battle:lock + ctx,锁在则重建 InBattleComp,
	//      FIGHTING 时推 BattleReconnectS2C;
	//      任一步没问到(空应答 / 读记录时 ERROR / 命令没发出去 / Redis 不可用)-> 记入登录复查,
	//      reaper 节拍重跑这一步,复查完成前不按锁重建(2026-10-16,CheckPendingSettlementOnEnter);
	//   2) RECONNECT / REPLACE 且仍有 InBattleComp -> 推 BattleReconnectS2C。
	//   只推提示(turn-based §22 D72):客户端凭 battle_id 补签票据、重建直连后 GetBattleState 补拉。
	static void OnPlayerEnterScene(entt::entity player, uint32_t enterGsType);

	// 冻结拦截查询:InBattleComp 存在即在战(切场景/跨 zone 等入口用)。
	static bool IsInBattle(entt::entity player);

	// reaper:30s 低频定时器,扫描 InBattleComp:PREPARING 按 prepare_deadline_ms 过期只摘组件、
	// 锁保留到 TTL(prepare_deadline+60s)自然过期给迟到确认留重建余量;FIGHTING 按 deadline_ms
	// 过期摘组件 + 条件删锁,并对仍在本节点的实体补读一次待结算记录(RecoverPendingSettlementAfterFreezeExpiry);
	// 之后排空已应用未销账的账本条目,最后重跑登录复查(RunLoginRechecks)。
	// 结构化日志 metric=battle_prepare_expired / battle_freeze_expired。
	// 不进 20FPS 帧循环;在 main.cpp 里随节点启动注册、随节点停止注销。
	static void StartReaper(muduo::net::EventLoop* loop);
	static void StopReaper();

	// reaper 扫描间隔(秒)。战斗作废的时间精度要求是"分钟级",30s 足够。
	static constexpr double kReaperIntervalSec = 30.0;

	// 存盘注入点。与通用资产通道同形状(PlayerAssetOpSystem::SetPersistFnForTest):
	// 生产恒为 PlayerLifecycleSystem::SavePlayerToRedis,单测用它把「结算应用后立刻压一次
	// 存盘」这一步替掉,避免单测被迫连 Redis。传 nullptr 恢复生产实现。
	using PersistFn = bool (*)(entt::entity);
	static void SetPersistFnForTest(PersistFn fn);
	// battle:lock 的 EX 在 deadline 之上的余量(秒):锁必须活得比 InBattleComp 久,
	// 避免"组件还在、锁先没了"让 match 放进第二场。
	static constexpr uint32_t kLockExtraTtlSec = 60;
	// 结算应用后锁至少续到的秒数:锁要留到这一局落盘销账才删(见 player_battle.cpp 的
	// ReleaseFreezeKeepLock),在那之前不能自然过期,否则重投会把「锁不在」读成「已作废」。
	// 取值 = battle 发件箱重投窗口(battle_room_manager.h:kSettlementRetryIntervalSec 10s ×
	// kSettlementRetryMaxAttempts 12 = 120s)+ 60s 余量。发件箱窗口改了这里要跟着改。
	// 正常路径下锁在落盘后一次 Redis 往返内就被删掉,这个值只在「应用后、落盘前进程崩溃」时起作用。
	static constexpr uint32_t kSettlementLockHoldSec = 180;
	// 待结算记录不再过期(landing-recovery-sync-digest.md §7):原来的 kPendingSettlementTtlSec
	// (与 battle 侧同值的 7 天)随 TTL 一起删除。记录一直保留到应用并销账为止,积压由 battle 节点报数。

private:
	friend class PlayerBattleSettlementTestAccess; // 内存测试共用应用入口，无网络测试钩子
	// 组 BattlePlayerSnapshot(属性/等级/技能/参战 buff/道具副本/路由信息/配表指纹)。
	// 失败时把拒绝原因写进 errorTipId 并返回 false(缺基础属性组件/缺会话快照)。
	static bool BuildBattleSnapshot(entt::entity player, ::BattlePlayerSnapshot& snapshot, uint32_t& errorTipId);

	// 把结算终值落到玩家实体；统一按 (player_id,battle_id) 有限去重。
	// true=本次应用成功；false=重复已条件销账，或失败/处理中须保留 pending。
	// 返回 true = 这一局已经应用(本次应用成功,或此前已应用过),调用方可以解冻并尝试销账。
	// alreadyAppliedOut 置 true 表示「此前就已应用」—— 奖是上一次发的,调用方**不要**再推一次
	// 战斗结束面板,否则重投/重登会让客户端反复弹结算。
	// 实体缺结算幂等账本(BattleSettlementLedgerComp,加载时挂上的必需组件)时返回 false 且零副作用:
	// 判不了这一局应用过没有,整笔不应用、pending 保留;本函数不会补建账本。
	static bool ApplySettlementToEntity(entt::entity player, const ::BattleSettlementData& settlement,
										bool* alreadyAppliedOut = nullptr, bool fromInbox = false);

	// 战斗中换会话 / 登录重建为 FIGHTING:经大厅会话推 BattleReconnectS2C{battle_id}
	// (scene→gate TCP,不经 Kafka)。gate 不中继战斗,没有绑定要重建(turn-based §22 D72);
	// 客户端凭 battle_id 经 MatchService.RequestBattleTicket 补签并重建直连。无 InBattleComp 时 no-op。
	static void NotifyBattleReconnectToClient(entt::entity player);

	// 应用挂起结算并清理 pending key + 锁(登录钩子 / reaper 补读的 Redis 回调侧)。
	static void ApplyPendingSettlement(entt::entity player, const ::BattleSettlementEvent& event);

	// reaper 判定 FIGHTING 到期、只解冻之后,对仍在本节点的实体补读一次待结算记录(设计 §7)。
	// 改造前这类奖励要等玩家下一次 EnterScene 才补:battle 发件箱只重投 120 秒,
	// 玩家一直在线就一直等不到登录钩子。命中走 ApplyPendingSettlement —— 与 OnPlayerEnterScene
	// 同一个函数、同一种身份复核(回调里实体仍有效且 Guid 仍是这个 player_id);未命中什么都不做。
	// 没读到(Redis 不可用 / 空应答 / ERROR / 意外类型 / 命令没发出去)记入登录复查(2026-10-16),不再只留痕等下次进场。
	// 解不开的记录不在这里删:留给登录钩子按原有规则清理,补读路径不扩大删除面。
	static void RecoverPendingSettlementAfterFreezeExpiry(entt::entity player, uint64_t playerId);

	// 按锁值匹配重建 FIGHTING 冻结(迟到确认 / 在线无组件路径):锁值==battleId 才读 ctx 重建
	// InBattleComp、条件续期并覆写 ctx;deadlineMsHint 非 0 时覆盖 ctx 的 deadline。reason 只进日志。
	// 与登录重建共用裁决 ClassifyLockRebuild:已应用推进销账;本实体已判作废的局不重建、补删锁(2026-10-19)。
	static void RebuildBattleFreezeFromLock(entt::entity player, uint64_t playerId, uint64_t battleId,
											uint64_t deadlineMsHint, const char* reason);

	// 登录重建:读 battle:lock + ctx(不预设 battle_id),锁在则按 ctx 重建 InBattleComp;
	// FIGHTING 时推重连提示(不要求 battle_node_id 已知)。只在无挂起结算且无 InBattleComp 时调用。
	// 读不了锁(Redis 不可用 / 命令没发出去 / 空应答)记入登录复查;EVAL 的 ERROR 仍按无锁处理(既有取舍,
	// 见 settlement_outbox.h 的 ClassifyLoginLockRead)。
	// 锁指向本实体上 reaper 已判作废的那一局时不重建、补一次条件删锁(ClassifyLockRebuild,2026-10-19)。
	static void RestoreBattleFreezeOnLogin(entt::entity player, uint64_t playerId);

	// reaper 判定 FIGHTING 到期之后的收尾:记下这一局已作废(BattleFreezeExpiredComp)→ 条件删锁 + 摘冻结 →
	// 对仍在本节点、不在退出中的实体补读一次待结算记录。从 reaper 循环里抽出来只为能单测;只由 reaper 调用。
	static void ReleaseExpiredFightingFreeze(entt::entity player, uint64_t playerId, uint64_t battleId);

	// 登录钩子第一步(读挂起结算 → 没有再按锁重建冻结)。OnPlayerEnterScene 与登录复查共用。
	// 没问到时记入登录复查(player_battle.cpp 的 BattleLoginRecheckComp),不按锁重建。
	static void CheckPendingSettlementOnEnter(entt::entity player, uint64_t playerId);

	// reaper 第三遍:Redis 可用时,对记入登录复查的玩家逐个摘标记并重跑 CheckPendingSettlementOnEnter;
	// Redis 不可用时标记保留。退出中(UnregisterPlayer)的实体摘标记不重跑,留给下次进场;
	// 跨 zone 冻结 / 交接中的实体标记保留、本轮不跑(交接完成随实体销毁,撤销则下个节拍再跑)。
	static void RunLoginRechecks();

	// 是否记着登录复查(只读;单测与排障用)。
	static bool IsLoginRecheckPending(entt::entity player);
	// 本实体上 reaper 最近判作废的 FIGHTING 局;没有返回 0(只读;单测与排障用)。
	static uint64_t ExpiredFightingBattleId(entt::entity player);
	// 两个按锁重建入口共用的裁决接线(本实体的作废槽位 + 活账本 → ClassifyLockRebuild),给单测走同一个函数。
	// 实体无效或缺账本时返回 false(重建入口在这种情况下跳过重建)。只读。
	static bool DecideLockRebuildFor(entt::entity player, uint64_t lockBattleId,
									 battle_settlement::LockRebuildDecision& out);
};
