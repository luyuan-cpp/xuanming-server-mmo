#pragma once

// 玩家持久收件箱消费端的纯规则(零 ECS / 零表 / 零全局状态 / 零时钟,可单测),
// 设计 docs/design/player-inbox-service.md §4.4、§7.3、§7.4、§7.6、§7.7、§8 #19。
//
// 这里只回答下面几个问题,不碰实体、不改资产、不触发存盘、不发请求:
//   1) 账本里某个域的游标在哪、怎么按 seq 升序记一条结局(最近结局环 ≤ 32,超出挤掉最旧的);
//   2) 一条投递是新条目,还是重复投递 —— 重复投递时能不能回放原结局(IB-9 三项核对);
//      以及分类与指令合起来,这一条接下来是执行、跳过、回放、报 UNKNOWN 还是停用(判定顺序见 PlanItem);
//      战斗结算域里同一局换了新 seq 的重投能否按最近结局环认出来(有界缓解,HasRecentAppliedBattleSettlement);
//   3) 一条已定的结局此刻算不算已落盘(含 UNKNOWN 的专门规则);
//   4) 从存档读出来的账本是否自洽(损坏就 fail-closed,由调用方挂标签停用该玩家的收件箱);
//   5) 一次落地算不算"新落地";
//   6) 退避(含只读 SQL 确认探测的退避)与在线兜底的时长(抖动由调用方以参数传入,本文件不取随机数),
//      以及卡住时周期性告警的节拍。
//
// 线程模型:纯函数,状态全在入参里,由调用方(scene loop 线程)保证独占访问。
// 单测:cpp/tests/currency_test/player_inbox_rules_test.cpp。

#include <cstdint>
#include <string>

#include "proto/common/component/player_inbox_ledger_comp.pb.h"

namespace player_inbox_rules
{
	// 每域最近结局环的容量。能装下"已处理、未确认"的全部条目的前提是:每批 ≤ kFetchBatchMax,
	// 且上一批全部确认之前不拉下一批(设计 §4.4、§7.4)。下面的 static_assert 钉住这条推导。
	constexpr int kRecentCapacity = 32;
	// 每批最多拉多少条(FetchPlayerInboxRequest.max_items)。
	constexpr uint32_t kFetchBatchMax = 16;
	static_assert(static_cast<int>(kFetchBatchMax) * 2 <= kRecentCapacity,
				  "最近结局环必须装得下至少两批:一批在途确认、一批刚落盘");

	constexpr uint64_t kMinCheckGapMs = 1000;            // 去抖:两次检查至少间隔 1 秒(战斗解冻除外)
	constexpr uint64_t kBackoffMinMs = 1000;             // 前置闸 / 传输失败的退避下限
	constexpr uint64_t kBackoffMaxMs = 60000;            // 退避上限
	constexpr uint64_t kUnimplementedBackoffMs = 600000; // 收件箱还是旧版本(UNIMPLEMENTED):10 分钟
	constexpr uint64_t kStaleOwnerBackoffMs = 60000;     // 围栏判 STALE:60 秒
	constexpr uint32_t kTickCheckBudget = 200;           // 单个 tick 最多发起的检查数
	constexpr uint64_t kResaveIntervalMs = 5000;         // 等落盘期间补请求存盘的间隔
	constexpr uint64_t kPersistThrottleMs = 500;         // 记账后请求存盘的节流(照 kAssetOpResaveMinIntervalMs)
	// 快照与本实体对不上(RoleAdmissionSystem::IsSnapshotCommitted 回 false)时隔多久再看。
	constexpr uint64_t kSnapshotRecheckMs = 1000;
	constexpr uint32_t kJitterPermilleMax = 1000;        // 抖动参数取值 [0, 1000]
	constexpr uint32_t kPeriodicJitterPercent = 20;      // 在线兜底 ±20%
	// 卡住时周期性告警的节拍(评审 L13、F3)。仓库现有的 scene 日志告警一律写成
	// count_over_time({app=~"scene.*"} |= "metric=..." [10m]) > 0(deploy/k8s/player-inbox-alerts.yaml)。
	// 问题还在时,相邻两条告警行的间隔必须落在这个窗口之内;间隔更长,告警就会"亮 10 分钟、灭几十分钟"地
	// 反复自愈,值班容易当成已经好了,而这名玩家的处置其实一直没执行。改告警窗口或下面的节拍时两边一起改,
	// static_assert 与 player_inbox_rules_test 守住这条推导。
	constexpr uint64_t kSceneLogAlertWindowMs = 10ULL * 60ULL * 1000ULL;
	// 节拍之外留给 tick 粒度(1 秒一次)与单 tick 检查预算(kTickCheckBudget)造成的顺延的余量。
	constexpr uint64_t kStuckWarnSlackMs = 2ULL * 60ULL * 1000ULL;
	// 不会自愈的延后(账本损坏)每连续失败这么多次再打一次 WARN。退避封顶 kBackoffMaxMs(加抖动之后仍夹在封顶之内),
	// 所以封顶之后两条 WARN 最多相隔 8 × 60 秒,每个卡住的在线玩家每小时约 7~9 行。
	// 不取更大的值:原先的 60 次约 50 分钟才打一条,对 [10m] 窗口的告警是抖动的(评审 F3)。
	constexpr uint32_t kStuckWarnEveryFailures = 8;
	static_assert(kStuckWarnEveryFailures * kBackoffMaxMs + kStuckWarnSlackMs <= kSceneLogAlertWindowMs,
				  "卡住时两条 WARN 的最长间隔必须落在 scene 日志告警窗口之内");
	// 整个节点的停用(注册表自检失败 metric=inbox_registry_broken、消费开关关着 metric=inbox_consumer_disabled,
	// 评审 H1)没有逐玩家的退避可数,改按时间补打:每 5 分钟一次。两者共用这一个节拍。
	constexpr uint64_t kNodeStuckRelogIntervalMs = 5ULL * 60ULL * 1000ULL;
	static_assert(kNodeStuckRelogIntervalMs + kStuckWarnSlackMs <= kSceneLogAlertWindowMs,
				  "节点级停用的补打间隔必须落在 scene 日志告警窗口之内");

	// ── 账本 ────────────────────────────────────────────────────────────────

	// 找某个域的游标;不存在返回 nullptr(该域从未处理过)。按 domain 字段查找,不靠下标(AGENTS §11.6)。
	const PlayerInboxDomainCursor *FindCursor(const PlayerInboxLedgerComp &ledger, uint32_t domain);

	// 缺则按 domain 升序插入一条空游标(decided_through_seq 0、recent 空)。
	// **调用约定**:只在确定接下来要 RecordDecision 时调用。空游标本身能过 ValidateLedger,
	// 但它会随存盘落地,无端多出一条记录。
	PlayerInboxDomainCursor &MutableCursor(PlayerInboxLedgerComp &ledger, uint32_t domain);

	// 结局能不能进账本:只有 APPLIED / REJECTED / SKIPPED。UNKNOWN 只进确认、不入环。
	bool IsRecordableOutcome(uint32_t outcome);

	// 记一条结局。前置:decision.seq() > cursor.decided_through_seq() 且结局可记账;
	// 前置不满足时返回 false 且**不改动任何状态**(调用方 LOG_ERROR 并停用该域)。
	// 通过后:压入环尾(超过 kRecentCapacity 挤掉最旧的),decided_through_seq = seq。
	bool RecordDecision(PlayerInboxDomainCursor &cursor, const PlayerInboxDecision &decision);

	// 加载校验。返回空串 = 通过;非空串是可直接进日志的损坏原因。检查项(设计 §8 #19):
	//   各域 domain != 0 且唯一;recent 条数 ≤ kRecentCapacity;recent 的 seq 非 0、严格升序、都 ≤ decided_through_seq;
	//   recent 的 outcome 可记账。不检查 request_id / msg_type 的取值:那两项只用于回放核对,0 也能比。
	std::string ValidateLedger(const PlayerInboxLedgerComp &ledger);

	// ── 投递分类(设计 §7.6 第 3 步,IB-9)──────────────────────────────────

	// 最后一项固定为 kCount,同时用作名字表长度(AGENTS §11.2)。
	enum class DeliveryClass : uint8_t
	{
		kNew,      // seq > decided_through:交给执行器
		kReplay,   // seq ≤ decided_through,环里有同一 seq 且 request_id、msg_type 都一致:回放原结局
		kMismatch, // seq ≤ decided_through,环里有同一 seq 但 request_id 或 msg_type 不一致:什么都不确认,停用
		kUnknown,  // seq ≤ decided_through,环里已没有这一条:报 UNKNOWN
		kCount
	};
	const char *DeliveryClassName(DeliveryClass deliveryClass);

	// cursor 为 nullptr 时一律 kNew。replayed 可为 nullptr;不为空时:
	//   kReplay   拷进环里的原结局,调用方原样回放;
	//   kMismatch 拷进环里同一 seq 的那一条,**只供日志对照**(值班据它的 request_id / msg_type 预判能否走
	//             VoidItem 的 FORCE_MISMATCH,评审 F4),绝不能拿它确认:它属于另一个请求;
	//   kNew / kUnknown 不动 replayed(环里没有可拷的)。
	DeliveryClass ClassifyDelivery(const PlayerInboxDomainCursor *cursor, uint64_t seq, uint64_t requestId,
								   uint32_t msgType, PlayerInboxDecision *replayed);

	// 战斗结算域的同局重投识别(C-1,2026-10-30)。ClassifyDelivery 只按 seq 去重:Match 首投的回包丢了(它记的
	// inbox_seq 仍是 0)、之后收件箱库又回档时,Match 重新 Enqueue 拿到的是一个新 seq,这一局在这里被判 kNew,
	// 奖励就会再发一次。本函数回答:本域最近结局环里是否已有 request_id 相同(战斗结算的 request_id = battle_id)、
	// msg_type 为 BATTLE_SETTLEMENT、结局 APPLIED 的条目。命中 = 同一局换了新 seq 重投,调用方不调执行器,
	// 直接给新 seq 记 APPLIED 并照常确认。
	// **有界缓解**:只看最近结局环(≤ kRecentCapacity 条),早已挤出环的旧局换新 seq 重投仍识别不出来。设计上刻意
	// 不另造永久 battle-id 集合(docs/design/cross-zone-matchmaking.md「不另造永久 battle-id 集合」),
	// 不要为了补这个上界在这里加持久索引。
	// cursor 为 nullptr、requestId 为 0(没有身份可比)时返回 false。纯函数,不改任何状态。
	bool HasRecentAppliedBattleSettlement(const PlayerInboxDomainCursor *cursor, uint64_t requestId);

	// 一条投递接下来怎么处理(设计 §7.6 第 3 步、§7.9,评审 M1)。最后一项固定为 kCount(AGENTS §11.2)。
	enum class ItemAction : uint8_t
	{
		kExecute,              // 新条目、无指令:交给执行器;本二进制没有该类型的执行器时由调用方停用该域
		kSkipVoided,           // 新条目、VOID:记 SKIPPED(reason = void_reason),不查执行器、不解码载荷
		kReplay,               // 回放账本环里的原结局
		kAckUnknown,           // 报 UNKNOWN(只进确认、不入环)
		kHaltMismatch,         // 什么都不确认,停用(IB-9)
		kHaltUnknownDirective, // 新条目带本二进制不认识的指令:版本错位,停用,不猜
		kCount
	};
	const char *ItemActionName(ItemAction action);

	// 判定顺序是这个函数存在的全部理由:**先按账本分类,只有新条目才看指令,执行器留给调用方最后查**。
	//   * 回放 / UNKNOWN / MISMATCH 只核对或报告账本里已有的事实,与指令、与本二进制认不认识这个类型都无关。
	//     反过来不能把 VOID 判定提到分类之前:已执行过的条目被补挂 VOID 后重投,必须回放原结局(IB-4、IB-9),
	//     报成 SKIPPED 会让服务端把已生效的处置终结成"跳过"。
	//   * 新条目带 VOID 时直接记 SKIPPED,不需要执行器:否则一条本二进制不认识类型的条目即使被运维作废,
	//     每次登录仍会先撞上"未知类型"再次停用该域,作废指令永远解不开它。
	// deliveryClass 只应来自 ClassifyDelivery;越界值按 kHaltMismatch(什么都不确认)处理,fail-closed。
	// directive 是 player_inbox::PlayerInboxDirective 的数值。
	ItemAction PlanItem(DeliveryClass deliveryClass, uint32_t directive);

	// ── 落盘判定(设计 §7.7)───────────────────────────────────────────────

	enum class Durability : uint8_t
	{
		kDurable,        // 可以确认
		kNotYet,         // 快照还没追上:继续等落盘
		kOutcomeDiffers, // 快照里同一 seq 的结局与内存不同:结局固定被破坏,只可能是缺陷,不报 durable
		kRingMissing,    // 快照水位已过这一条,但环里找不到它(按"环装得下"的推导不可能发生):不报 durable
		kCount
	};
	const char *DurabilityName(Durability durability);

	// persisted 是最后一次**确实写进 Redis** 的那份账本(PlayerLastPersistedSnapshotComp)。
	//   APPLIED / REJECTED / SKIPPED:快照里该域 decided_through ≥ seq,且环里同一 seq 的 outcome、reason、
	//                                 request_id、msg_type 都与内存一致;
	//   UNKNOWN:只要快照里该域 decided_through ≥ seq 就算落盘,**不要求**环里有这一条 ——
	//            UNKNOWN 按定义就不在环里,要求它在环里会让这条流永远确认不了(v0.1 的缺陷)。
	Durability ClassifyDurability(const PlayerInboxLedgerComp &persisted, uint32_t domain,
								  const PlayerInboxDecision &decision);

	// ── 触发与时长 ───────────────────────────────────────────────────────────

	// 落地钩子据此决定要不要无条件拉一次(设计 §7.3):实体上还没有运行时组件(在本节点新建),
	// 或 owner_epoch 与上次落地时不同(归属变过)。同节点换图、gate 补发的重复路由都返回 false。
	bool IsNewLanding(bool hasRuntime, uint64_t landedEpoch, uint64_t currentEpoch);

	// 第 failStreak 次连续失败后的退避:1s、2s、4s … 封顶 60s,再乘 [0.8, 1.2] 的抖动,
	// 结果夹在 [kBackoffMinMs, kBackoffMaxMs]。failStreak == 0 按 1 处理。jitterPermille 取 [0, 1000]。
	uint64_t BackoffMs(uint32_t failStreak, uint32_t jitterPermille);

	// 第 streak 次连续「只读 SQL 确认没证明提交」之后到下一次探测的等待:BackoffMs(streak) 再封顶到

	// 在线有界兜底的间隔:minutes 分钟 ±20%。minutes == 0 = 关闭,返回 0。
	uint64_t PeriodicDelayMs(uint32_t minutes, uint32_t jitterPermille);

	// 不会自愈的延后原因是否到了再打一次 WARN 的时候:failStreak 为 kStuckWarnEveryFailures 的正整数倍。
	// "首次"由调用方按延后原因是否刚变化判定,不归这里管;本函数不改 failStreak,也不影响退避。
	bool IsStuckWarnDue(uint32_t failStreak);
} // namespace player_inbox_rules
