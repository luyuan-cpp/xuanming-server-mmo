#pragma once

// 玩家持久收件箱的 scene 消费端(docs/design/player-inbox-service.md §7)。
//
// 一句话:收件箱服务(go/player_inbox)为每个 (玩家, 域) 存一条按 seq 升序的待办流;本系统在玩家的 owner scene 上
// 主动拉取(FetchItems)、按 seq 逐条交给执行器、把结局写进与货币同记录的账本(player_database.inbox_ledger,
// 与效果同一次落盘,IB-3)、**落盘之后**再确认(AckItems)。重复投递按 (seq, request_id, msg_type) 核对一致后
// 回放原结局,不一致 fail-closed(IB-9)。
//
// 触发入口(设计 §7.3),全部只是"安排一次检查",真正的拉取在 Tick 里做:
//   * OnPlayerLanded        PlayerLandingSystem::OnEnteredScene 一行调用。实体在本节点新建、或 owner_epoch 变了 =
//                           新落地,无条件拉一次;同节点换图、gate 补发的重复路由直接跳过。**唯一**创建运行时组件的地方。
//   * OnNotifyEvent         PlayerInboxNotifyEvent(Kafka scene-cmd)的生成 handler 守护段一行调用。只是提醒:
//                           找不到实体 / 实体还没落地就丢弃,下次落地会拉。
//   * OnBattleFreezeCleared PlayerBattleSystem 摘 InBattleComp 的唯一入口里一行调用。之前因战斗延后的,立即重查
//                           (不合并、不去抖)。
//   * Tick                  scene main 每秒调一次:到期的检查(单个 tick ≤ 200 次)、在线有界兜底、等落盘与补确认、
//                           在途超时。节点级停用(注册表自检失败 metric=inbox_registry_broken、配置开关关着
//                           metric=inbox_consumer_disabled)每 kNodeStuckRelogIntervalMs 补打一行,与有没有在线玩家无关。
//
// 离开入口(不安排检查):
//   * OnPlayerLeaving       PlayerLifecycleSystem::FinishExitAfterPersist 一行调用(退出存盘已收敛、实体随后销毁)。
//                           对最后一次落盘快照里已有的待确认结局尽力补发一次 Ack,不等回包;没落盘就什么都不做,
//                           等下次落地补确认(设计 §7.7、§8 #7,评审 M4)。不创建组件。
//                           管不到已投递、还没定结局的行(退出时 Fetch 在途、退出期被前置闸丢掉的回包、DEFER 剩下的
//                           本地条目):只打 metric=inbox_leave_ack result=fetch_inflight / delivered_undecided 供对账,
//                           这些行仍靠下次落地重拉,或 Go 侧 24h 陈旧窗口(deliveredStaleAfter)移出告警。
//
// 契约:
//   * **线程**:全部入口只在 scene 的 loop 线程上调用(落地钩子、tick、Kafka 事件、gRPC 完成回调都在这个线程,
//     设计 §10)。状态在 thread_local 的 ECS 组件里,没有锁。
//   * **回调携带调用身份**:gRPC 完成回调按值捕获 player_id / domain / correlation_id,不捕获实体句柄;
//     按 player_id 重新找实体后仍核对 domain、关联号与 phase,迟到调用直接丢弃。响应中的每项回显非 0 且
//     与捕获值不同即丢弃;回显为 0 时按捕获值处理。PING 回调也只捕获 player_id / domain。
//   * **不阻断进场**(IB-7):收件箱不进 DependencyGate;不可用时只退避(1s→60s,UNIMPLEMENTED 10 分钟)。
//   * **前置闸**(设计 §7.5)在拉取前与应用前各判一次:consumer_enabled、未停用、可改(player_mutability.h)、
//     账本有效、域规格要求时不在战斗中、有 READY 节点。**确认不受前置闸限制**:Ack 只报告已落盘的事实。
//     应用前不过闸就丢掉本地条目、下次重拉(看得到期间新挂的 VOID);只有执行器 DEFER 才在本地续做(评审 L1)。
//   * **判定顺序**(评审 M1):先按账本分类(回放 / UNKNOWN / MISMATCH),新条目再看指令(VOID 直接 SKIPPED),
//     最后才查执行器;见 player_inbox_rules::PlanItem。战斗结算域的新条目在调执行器之前再按最近结局环认一次同局重投
//     (同一 request_id = battle_id 已 APPLIED → 只记 APPLIED、不重放效果,metric=inbox_battle_duplicate_request);
//     环只有 32 条,是有界缓解(player_inbox_rules::HasRecentAppliedBattleSettlement)。
//   * **SQL 确认探测有退避**:只读确认没证明两张 SQL 行已提交时,按 1s→5s 退避再探(nextAckMs),不在每个 Tick 都探。
//   * **单飞**:同一 (玩家, 域) 同一时刻至多一个在途请求;上一批全部确认之前不拉下一批(设计 §7.4)。
//     回到 Idle 时 ackQueue 必为空:丢弃本地条目的路径若还有已定前缀,先确认再重拉(评审 L2)。
//     Ack 回 kInvalidParameter 是确定性协议错误,按协议异常停用,不当故障重发。
//   * **协议异常即停用**(halt):本实体不再为该域拉取,直到实体下次在本节点新建。不重试、不空转。

#include <cstdint>
#include <string>

#include "entt/src/entt/entity/entity.hpp"

namespace player_inbox
{
	class FetchPlayerInboxRequest;
	class FetchPlayerInboxResponse;
	class AckPlayerInboxRequest;
	class AckPlayerInboxResponse;
} // namespace player_inbox

class PlayerInboxSystem
{
public:
	// Tick 的周期(秒)。scene main 用它注册定时器。
	static constexpr double kTickIntervalSec = 1.0;

	// 启动时调一次(Node 构造之后、配置已加载):注册表自检,失败则本节点停用消费(不阻断启动,IB-7),
	// 并打一行生效配置。配置开关关着时这一行是 WARN `metric=inbox_consumer_disabled`(评审 H1),之后由 Tick 补打。
	static void Initialize();

	// ── 触发入口 ──
	static void OnPlayerLanded(entt::entity player);
	static void OnBattleFreezeCleared(entt::entity player);
	static void OnNotifyEvent(uint64_t playerId, uint32_t domain);
	static void Tick();

	// ── 离开入口 ──
	// 实体即将离开本节点、随后销毁时调一次(调用方保证退出存盘已收敛,owner_epoch 组件还在)。
	// 契约:只在某个域的 ackQueue 非空、没有 Ack 在途、且每条结局都已出现在最后一次落盘快照里时,发一次 AckItems;
	// 不等回包、不额外登记等待状态、不压存盘、不看 Ack 退避。任何条件不满足都什么都不做 —— 绝不在落盘之前确认(IB-3)。
	// 只是减少"已投递未确认"的残留,正确性仍靠下次落地补确认。无效实体 / 没有运行时组件时直接返回。
	static void OnPlayerLeaving(entt::entity player);

	// ── gRPC 回包(由默认传输的每次调用回调转进来,单测可直接调用)──
	// 身份参数来自调用时的捕获值;grpcCode 是线上 grpc-status 的数值(kGrpcStatus*),非 OK 即结果未知。
	static void OnFetchReply(uint64_t playerId, uint32_t domain, uint64_t correlationId,
							 const player_inbox::FetchPlayerInboxResponse &response);
	static void OnFetchFailed(uint64_t playerId, uint32_t domain, uint64_t correlationId, uint32_t grpcCode,
							  const std::string &detail);
	static void OnAckReply(uint64_t playerId, uint32_t domain, uint64_t correlationId,
						   const player_inbox::AckPlayerInboxResponse &response);
	static void OnAckFailed(uint64_t playerId, uint32_t domain, uint64_t correlationId, uint32_t grpcCode,
							const std::string &detail);

	// ── 测试注入点(AGENTS §11.2 显式依赖:存盘、时钟、传输都要能替换)──
	// 全部传 nullptr 恢复默认实现。生产代码**不调用**这些 setter。

	// 默认 &PlayerLifecycleSystem::SavePlayerToRedis。true = 确实压了一次写;false 不代表任何具体原因,
	// 是否已落盘一律只看 PlayerLastPersistedSnapshotComp。
	using PersistFn = bool (*)(entt::entity);
	static void SetPersistFnForTest(PersistFn fn);

	// 默认单调时钟毫秒(steady_clock)。进程内的退避、去抖、超时都用它(AGENTS §11.3)。
	using NowMsFn = uint64_t (*)();
	static void SetNowFnForTest(NowMsFn fn);

	// 默认:挑一个 READY 的 PlayerInboxNodeService 节点,用自带完成回调的生成客户端发出;false = 没有可用节点。
	using SendFetchFn = bool (*)(const player_inbox::FetchPlayerInboxRequest &);
	using SendAckFn = bool (*)(const player_inbox::AckPlayerInboxRequest &);
	static void SetTransportForTest(SendFetchFn sendFetch, SendAckFn sendAck);

	// 默认读 GameConfig.player_inbox。测试用它绕开配置文件;传 false 的 overrideConfig 恢复读配置。
	static void SetConsumerConfigForTest(bool overrideConfig, bool consumerEnabled, uint32_t periodicFetchMinutes);
};
