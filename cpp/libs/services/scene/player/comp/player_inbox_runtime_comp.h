#pragma once

#include <cstddef>
#include <cstdint>
#include <iterator>
#include <vector>

#include "proto/common/component/player_inbox_ledger_comp.pb.h" // PlayerInboxDecision
#include "proto/player_inbox/player_inbox.pb.h"                 // player_inbox::PlayerInboxItem

// 玩家持久收件箱在 scene 侧的运行时状态(docs/design/player-inbox-service.md §7.2)。
//
// 纯运行时:不入 PlayerAllData、不序列化、不跨进程,所以是普通 C++ struct 而不是 proto。
// 持久的一半是 PlayerInboxLedgerComp(player_database.inbox_ledger),只由加载创建。
//
// 组件类别(docs/design/ecs-component-access-rules.md):**按需状态**。唯一的延迟创建入口是
// PlayerInboxSystem::OnPlayerLanded(该文件里的 StoreInboxRuntimeForWrite);读路径(tick、回包、提醒、
// 战斗解冻)一律 try_get,拿不到就当"本实体还没在本节点落地过",直接丢弃 —— 下次落地会无条件拉一次。
// "实体上还没有本组件"本身就是"新落地"判据的一半(设计 §7.3),所以绝不能在别处预挂。
//
// 线程:只在 scene loop 线程读写;没有锁。

// 单个域的一轮处理所处的阶段。一轮 = 拉一批 → 逐条应用 → 等落盘 → 确认。
// 最后一项固定为 kCount,同时用作名字表长度(AGENTS §11.2:不用宏生成声明)。
enum class PlayerInboxPhase : uint8_t
{
	kIdle,         // 没有在途请求,也没有待处理的条目;可能挂着 nextCheckMs
	kFetching,     // FetchItems 在途(correlationId)
	kApplying,     // 已拉到、尚未全部应用的条目在 pendingItems(执行器 DEFER,按 nextCheckMs 续做;
	               // 前置闸未过时不停在这里:丢掉本地副本重拉,有已定前缀就先确认,评审 L1)
	kAwaitDurable, // 本批结局都已记账,等它们出现在最后一次落盘快照里(ackQueue)
	kAcking,       // AckItems 在途(correlationId)
	kCount
};

// 本域上一次没能继续的原因,只用于退避与日志(`[PlayerInbox] metric=inbox_deferred reason=`,设计 §9.4)。
// 前五项与 player_mutability::PlayerMutability 的原因同名。
enum class PlayerInboxDeferReason : uint8_t
{
	kNone,
	kDisabled,         // GameConfig.player_inbox.consumer_enabled = false,或注册表自检失败
	kFrozen,
	kHandoff,
	kExiting,
	kEpochZero,
	kLedgerInvalid,    // PlayerInboxLedgerInvalidComp,或持久账本组件缺失(加载不变量被破坏)。
	                   // 不会自愈:除首次外还按 failStreak 周期性打 WARN `metric=inbox_ledger_invalid`(评审 L13);
	                   // 节拍对齐 scene 日志告警的 [10m] 窗口(player_inbox_rules::kStuckWarnEveryFailures,评审 F3)
	kInBattle,         // 域规格 requires_out_of_battle 且 InBattleComp 在
	kNoNode,           // 没有 READY 的 PlayerInboxNodeService 通道
	kTransport,        // gRPC 非 OK(UNAVAILABLE / DEADLINE …)或在途超时
	kUnimplemented,    // 收件箱还是旧版本:长退避 10 分钟,不计故障
	kStaleOwner,       // 围栏判 STALE:本节点已不是 owner
	kServerError,      // 回包带非成功的 tip 码,或回包形状不认识
	kExecutorDefer,    // 执行器返回 DEFER:在这一条停下,本地退避后续做,不重新 Fetch
	kCount
};

// 协议异常导致的停用原因。停用 = 本实体不再为这个域 Fetch,直到实体下次在本节点新建(设计 §7.2、§7.7)。
enum class PlayerInboxHaltReason : uint8_t
{
	kNone,
	kEpochRequired,    // Fetch / Ack 回 EPOCH_REQUIRED:本端发了 0,只可能是缺陷
	kEpochAhead,       // Fetch / Ack 回 EPOCH_AHEAD:超出 Redis 上界,缺陷或伪造
	kUnknownType,      // 本二进制不认识的 msg_type,或类型不属于本域(版本错位):停在队头,不记 REJECTED
	kReplayMismatch,   // 重复投递与账本里的同一 seq 对不上(IB-9)
	kBatchMalformed,   // 回包超过一批上限、seq 不严格递增或为 0
	kLedgerWrite,      // 记账前置被破坏(分类说未见、记账却拒绝)
	kAckAnomaly,       // Ack 结果为 CONFLICT / UNKNOWN_SEQ / NOT_DELIVERED / SKIP_NOT_REQUESTED / MISMATCH,
	                   // 或整次 Ack 回 kInvalidParameter(报告列表不合法,重发不会变)
	kVersionSkew,      // 执行器回 kVersionSkew:认识类型,但本二进制不能安全执行这一条(混版本保护,
	                   // 例如同币种欠款带本版本不认识的字段)。不记账、不确认这一条,实体重建后重拉
	kCount
};

// 单个域的运行时状态。按 domain 字段查找,不靠下标(AGENTS §11.6)。
//
// 时间字段一律是**单调时钟**毫秒(PlayerInboxSystem 的 NowMs,AGENTS §11.3),0 = 未设置。
struct PlayerInboxDomainState
{
	uint32_t domain{0}; // player_inbox::PlayerInboxDomain
	PlayerInboxPhase phase{PlayerInboxPhase::kIdle};

	// 在途请求(Fetch 或 Ack)的关联号:取自本节点 thread_local 单调计数器,不按实体从头计,
	// 已销毁实体的在途回包因此不可能与新实体的请求同号(设计 §7.2)。0 = 没有在途。
	uint64_t correlationId{0};
	uint64_t inflightSinceMs{0}; // 在途起点;超过 gRPC deadline + 余量仍无完成通知时按传输失败收场
	uint64_t sentEpoch{0};       // 最近一次 Fetch 带出去的 owner_epoch(日志用)

	// 已拉到、尚未应用的条目(按 seq 升序)。只在 kApplying 阶段非空。
	std::vector<player_inbox::PlayerInboxItem> pendingItems;
	// 本批已经定下的结局,等落盘后一次性确认。按 seq 升序,≤ 一批上限。
	// 复用账本的 PlayerInboxDecision 形状;outcome 可以是 UNKNOWN(那一种只进确认、不入账本环)。
	std::vector<PlayerInboxDecision> ackQueue;

	bool hasMore{false};          // 本轮 Fetch 回包的 has_more:确认完立刻拉下一批
	bool recheckRequested{false}; // 非 Idle 期间收到的新触发(单飞,设计 §7.4)
	uint64_t nextCheckMs{0};      // 到点做一次检查(Idle → Fetch,Applying → 续做);0 = 不安排
	uint64_t nextAckMs{0};        // kAwaitDurable 里下一次允许发 Ack 的时刻(Ack 失败后的退避)
	uint64_t lastFetchMs{0};      // 最近一次发出 Fetch 的时刻(去抖:两次检查至少间隔 1 秒)
	uint64_t nextPeriodicMs{0};   // 在线有界兜底的下一次到点;0 = 关闭
	uint64_t lastPersistRequestMs{0}; // 最近一次为本域请求存盘的时刻(500ms 节流 / 5s 补请求)
	uint32_t failStreak{0};       // 连续失败次数,决定 1s→60s 的退避
	uint32_t ackFailStreak{0};    // Ack 连续失败次数
	PlayerInboxDeferReason deferReason{PlayerInboxDeferReason::kNone};
	bool durableMismatchReported{false}; // "快照里的结局与内存不同"每批只报一次 ERROR

	bool halted{false};
	PlayerInboxHaltReason haltReason{PlayerInboxHaltReason::kNone};
};

struct PlayerInboxRuntimeComp
{
	// 上一次被判为"新落地"时的 PlayerOwnerEpochComp.epoch。与当前 epoch 不同 = 归属变过 = 再拉一次(设计 §7.3)。
	uint64_t landedEpoch{0};
	// 每个消费域一份;在 OnPlayerLanded 里按注册表的消费域列表补齐,此后只改不增删。
	std::vector<PlayerInboxDomainState> domains;
};

// 加载时 ValidateInboxLedger 判定账本损坏的标签(空结构,不入库)。挂上之后该玩家的收件箱整体停用,
// **不改写**原数据,不阻断登录(设计 §8 #19,照 PlayerAssetOpLedgerInvalidComp)。
struct PlayerInboxLedgerInvalidComp
{
};

// 名字表:日志与指标用。顺序与枚举一致,static_assert 防脱节。
inline constexpr const char *kPlayerInboxPhaseNames[] = {"idle", "fetching", "applying", "await_durable", "acking"};
static_assert(std::size(kPlayerInboxPhaseNames) == static_cast<std::size_t>(PlayerInboxPhase::kCount),
			  "阶段名字表与 PlayerInboxPhase 脱节");

inline constexpr const char *kPlayerInboxDeferReasonNames[] = {
	"none", "disabled", "frozen", "handoff", "exiting", "epoch_zero", "ledger_invalid", "in_battle",
	"no_node", "transport", "unimplemented", "stale_owner", "server_error", "executor_defer"};
static_assert(std::size(kPlayerInboxDeferReasonNames) == static_cast<std::size_t>(PlayerInboxDeferReason::kCount),
			  "延后原因名字表与 PlayerInboxDeferReason 脱节");

inline constexpr const char *kPlayerInboxHaltReasonNames[] = {
	"none", "epoch_required", "epoch_ahead", "unknown_type", "replay_mismatch",
	"batch_malformed", "ledger_write", "ack_anomaly", "version_skew"};
static_assert(std::size(kPlayerInboxHaltReasonNames) == static_cast<std::size_t>(PlayerInboxHaltReason::kCount),
			  "停用原因名字表与 PlayerInboxHaltReason 脱节");

// 越界回 "?"。返回 const char*:muduo 的 LogStream 没有 string_view 重载。
inline const char *PlayerInboxPhaseName(PlayerInboxPhase phase)
{
	const auto index = static_cast<std::size_t>(phase);
	return index < std::size(kPlayerInboxPhaseNames) ? kPlayerInboxPhaseNames[index] : "?";
}

inline const char *PlayerInboxDeferReasonName(PlayerInboxDeferReason reason)
{
	const auto index = static_cast<std::size_t>(reason);
	return index < std::size(kPlayerInboxDeferReasonNames) ? kPlayerInboxDeferReasonNames[index] : "?";
}

inline const char *PlayerInboxHaltReasonName(PlayerInboxHaltReason reason)
{
	const auto index = static_cast<std::size_t>(reason);
	return index < std::size(kPlayerInboxHaltReasonNames) ? kPlayerInboxHaltReasonNames[index] : "?";
}
