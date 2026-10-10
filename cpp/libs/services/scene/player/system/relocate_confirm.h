#pragma once

#include <algorithm>
#include <array>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <iterator>
#include <limits>
#include <optional>
#include <string>
#include <string_view>
#include <unordered_map>
#include <utility>
#include <vector>

// 疏散 / 排空改派的"待确认表"—— 纯常量、纯判定与纯容器,不含 entt / redis / muduo / protobuf,不读任何时钟,
// 可单测(cpp/tests/cross_zone_test 的 RelocateConfirm*)。风格同 handoff_mark_withdraw.h / exit_release_mark.h /
// travel_freeze_cap.h。粘合代码在 player_lifecycle.cpp(DispatchEmergencyRelocate / TrackRelocateDispatch /
// SendEmergencyRelocateEnterScene / ReconcileRelocateOnReentry / SweepRelocateConfirms,以及同文件里的认领、
// 核实、踢线函数)。
// 设计:docs/design/cross-zone-scene-travel.md「CPP-3 疏散 / 排空改派的待确认表」一节(按标题引用,章节号以文档为准)。
//
// ── 为什么需要 ──
// 单场景排空(BeginSceneDrain)与整节点疏散(BeginEmergencyRelocateAll)在玩家退出存盘收敛、实体销毁的那一刻,
// 凭票据替他向 scene_manager 发一条 EnterScene,让他带着原来的 gate 会话落到别的场景 / 节点。原先这条请求发完即忘:
// scene_manager 拒绝、请求根本没发出去、落回本节点后载入被放弃时,实体已销毁、location 仍指向源场景,客户端的
// gate 会话却还挂着 —— 玩家在线却没有实体,只能自己发现并重登。
// 本文件把改派改成"登记、确认、收口":票据被消费时登记一条条目;应答 / 传输失败 / 进场路由回到本节点 / 各阶段
// 截止时刻只负责推动条目;只有在 zone Redis 上读到 location 仍指向源场景或本节点(或已不存在)、并且 scene_manager
// 不可能再处理本节点替他发的请求时,才按票据里的会话直推 gate:tip + 踢线 34,让客户端走正常重登。
//
// ── 契约(粘合代码必须守住,改任何一条都要重过一遍单测与设计)──
//   * 键 = player_id,同一玩家同时最多一条条目。条目在销毁实体之前的同一个调用栈里登记;条目存在期间,本节点上
//     该玩家要么没有实体,要么条目处于 kLanding(进场路由已回到本节点,等载入把实体建出来)。
//   * 回调凭 (player_id, seq) 对号。seq 由表内单调发号,从 1 起;0 = 这次改派没被跟踪(表满)。
//   * 应答只触发核实,从不直接定案:成功不直接结清,拒绝不直接踢。裁决只看核实读到的 location 的
//     zone / node / scene,不看 owner_epoch、不读回滚回执(回执只证明"凭这份标记的某条请求被回滚",证明不了本次
//     改派已处理完)。读到的 owner_epoch 只当条件补写的参数与日志,不得写进任何组件。
//   * 应答与传输失败都按关联号认领(DecideReplyClaim / DecideTransportFailureClaim)。只有回显号为 0 的应答
//     (旧版 scene_manager 不回显)才退回按 player_id 认领;传输失败没有这条退路。
//   * 传输失败 = 结果未知(kTransportFailed),与"等不到完成通知"(kNoReply)同侧:settle 之前不读、不提前踢,
//     读不到时也不盲踢。
//   * 核实与凭证补写只走不重放的 zone Redis 连接(tlsRedis.GetZoneRedis() 的 muduo hiredis::Hiredis::command,
//     无队列、无重放)。改走带重试队列的接口,补写可能被重放到某次载入的标记清理之后,等于给活持有者发放行证。
//   * 本节点有该玩家实体、或他的载入在途时绝不补写(DecideCredential 的 kSkipLocalHolder);实体已建出时也不踢。
//   * 核实应答只在 AcceptsVerifyReply 为真时受理(阶段 + 在途 + 代际,连同 Find 的 seq 共四项)。
//   * 无法核实(连续 kVerifyBudget 读不到)时,只有"本节点替他发的请求都确定没写过落点"才踢
//     (ShouldKickUnverified)。本节点读不到 ≠ scene_manager 写不了。
//   * 疏散中的确认阶段只读 Redis、只推 gate,不写任何玩家键(补写被身份闸挡住,kSkipIdentity)。
//   * 表只活在 scene 逻辑线程的 thread_local 里,无锁。进程退出丢表无害:只是少踢,玩家自己重登。
//
// 时间一律由调用方传入(显式依赖,AGENTS §11.2),本文件不读任何时钟。
// 本文件不写任何 scene_manager 错误码的字面量:哪些拒绝码"发生在任何落点写之前"由 player_lifecycle.h 的
// PlayerLifecycleSystem::SmRejectedBeforeAnyPlacementWrite 判,结果以 bool 传进来(该头包含本头,反向引用不了)。
namespace relocate_confirm
{
	using Clock = std::chrono::steady_clock;

	// ── 预算(全部按单调时钟)────────────────────────────────────────────────

	// 改派标记的条件写(DispatchEmergencyRelocate 里那条 EVAL)最迟多久回调。正常是毫秒级,回调一直不来只会是
	// 半开连接;给宽一些,Redis 只是慢的时候不把"还没发"误判成"发不出去"。到期按 kNotSent 转核实。
	inline constexpr std::chrono::seconds kMarkWriteBudget{30};

	// scene_manager 一次 Kafka 同步写的上限(scene_manager_service.yaml 的 KafkaWriteTimeoutSeconds,默认 5)。
	// C++ 侧 gRPC deadline 到期之后服务端的 handler 仍在跑(go-zero 的超时拦截器只是提前返回、不等 handler,
	// 推路由用的又是 context.Background()),最多再做一次 Kafka 写与失败回滚。
	// **跨配置契约:调大 KafkaWriteTimeoutSeconds 必须同步本常量。** zrpc Timeout 的变化经 C++ deadline
	// (部署门禁守着 deadline ≥ Timeout + 2000)自动带进 settle 窗口,不用改这里。
	inline constexpr std::chrono::seconds kSettleAfterCallDeadline{5};

	// settle 窗口的下限。settle 窗口 = 发出改派之后 scene_manager 还可能改 location 的最长时间:发出后 deadline
	// (zrpc Timeout 8s + 2s)之内必有应答或传输失败,其后 handler 最多再跑一个 kSettleAfterCallDeadline,所以
	// sentAt + deadline + 5s 之后它不会再动 location。10 + 5 = 15。实际取值由 BudgetsFor 从 deadline 派生,
	// 本常量只是下限(deadline 被配小时窗口不跟着缩)。
	// 已知残余:回滚 EVAL 在 go-redis 重试下可能更慢;请求在客户端通道里排队到 deadline 前一刻才被服务端收到时,
	// handler 也可能越过本窗口。后果是误踢一次、location 停在目标上,只伤活性(重登即恢复)。
	inline constexpr std::chrono::seconds kSmSettleWindow{15};

	// 等完成通知(应答或传输失败)的兜底下限。每次调用都带 deadline,完成通知正常一定在 deadline 内到;本预算只兜
	// "完成通知永远不来"(例如 scene_manager 节点在调用途中被摘除)。到期按 kNoReply 转核实。
	// 不叫 kReplyBudget:travel_freeze_cap::kReplyBudget 是交接应答看门狗,同名异义。
	inline constexpr std::chrono::seconds kReplyWaitBudget{30};

	// 进场路由回到本节点之后等实体建出来的上限:路由传递 + AsyncLoad 6 次退避约 31.5s(redis_client.h 的
	// kMaxLoadRetries = 6)+ 载入前清继承标记 5s(exit_release_mark::kInheritClearDeadline)+ 余量。
	inline constexpr std::chrono::seconds kLandingBudget{60};

	// 一轮核实(从转入 kVerifying 起)最多重试多久;到期仍读不到按"无法核实"处置(ShouldKickUnverified)。
	inline constexpr std::chrono::seconds kVerifyBudget{10};
	// 核实读不到时的重试间隔(由 RedisSystem 的 1s 定时器驱动,实际间隔按 1s 取整)。
	inline constexpr std::chrono::seconds kVerifyRetryInterval{1};

	// 上限只为给内存封顶。表满时**不淘汰**在途条目(淘汰 = 放弃对某个玩家的确认),新的改派按旧行为发出、不跟踪。
	inline constexpr std::size_t kMaxTracked = 8192;

	static_assert(kSmSettleWindow < kReplyWaitBudget,
				  "等完成通知的兜底必须晚于 settle 窗口:kNoReply 出现时 scene_manager 已不可能再处理本次改派");
	static_assert(kVerifyRetryInterval < kVerifyBudget, "一轮核实里至少要能重试一次");

	// 一次改派用到的两个随 gRPC deadline 变化的预算。粘合层在登记与发送处各算一次(BudgetsFor)传给
	// Table::Add / Table::MarkSent;本文件不依赖引擎头,不自己读 deadline。
	struct Budgets
	{
		Clock::duration settleWindow{kSmSettleWindow};
		Clock::duration replyWait{kReplyWaitBudget};

		bool operator==(const Budgets &) const = default;
	};

	// smCallDeadline = C++ 发往 scene_manager 的 unary 调用 deadline
	// (grpc_call_deadline::Get(eNodeType::SceneManagerNodeService);该头要求依赖调用结果的预算一律从它派生)。
	//   settleWindow = max(kSmSettleWindow, deadline + kSettleAfterCallDeadline)
	//   replyWait    = max(kReplyWaitBudget, settleWindow + kSettleAfterCallDeadline)
	// deadline = 10000ms 时得 {15s, 30s}。replyWait 恒大于 settleWindow,所以 kNoReply 出现时一定已过 settleAt。
	constexpr Budgets BudgetsFor(std::chrono::milliseconds smCallDeadline)
	{
		// seconds 与 milliseconds 直接喂给 std::max 时模板实参推导失败,先各自转成 Clock::duration 再比。
		// 本文件里的 std::max 与 std::numeric_limits<T>::max 一律加括号写成 (std::max)(…) / (…::max)():不依赖
		// "包含它的工程定义了 NOMINMAX、或前面某个头恰好 #undef 过 max"(cross_zone_test.vcxproj 就没定义;
		// 同 scene_route_helper.h 对 std::min 的写法)。
		const Clock::duration settleFloor = kSmSettleWindow;
		const Clock::duration settleDerived = smCallDeadline + kSettleAfterCallDeadline;
		Budgets budgets;
		budgets.settleWindow = (std::max)(settleFloor, settleDerived);
		const Clock::duration replyFloor = kReplyWaitBudget;
		const Clock::duration replyDerived = budgets.settleWindow + kSettleAfterCallDeadline;
		budgets.replyWait = (std::max)(replyFloor, replyDerived);
		return budgets;
	}

	// 核实用的只读脚本:一次读回三个键。
	//   KEYS[1] = player:{id}:owner_epoch   KEYS[2] = player:{id}:location   KEYS[3] = player:{id}:handoff
	// 应答必须是三元素数组、每个元素 STRING 或 NIL;空应答 / ERROR / 其它形状一律按"这次没读到"推迟重试。
	// 为什么带 "#!lua"(Redis ≥ 7,全环境 7.2):不声明 no-writes 的带 shebang 脚本,在只读副本 / MISCONF / OOM 下
	//   **整体被拒**。裸 MGET 在主从切换后的旧主上照样读成功、读到滞后值:证据是成功应答或结果未知时,把滞后的
	//   "未变"当真会踢掉一条已合法改绑的会话;脚本被拒时这两类证据落到"放弃、不踢"。**不得**加 flags=no-writes,
	//   也不得去掉 shebang(同 travel_outcome::kLuaJudgeTravelOutcome)。
	// 为什么不删任何键、也不复用 kLuaJudgeTravelOutcome:那段脚本"先删本族标记再读"是为了让活着的冻结实体能安全
	//   解冻;这里没有实体可解冻,删标记只会毁掉被踢玩家重登要出示的凭证(换手门回 18)。
	// 为什么用 MGET 不用 GET:类型不对的键给 nil,不抛 WRONGTYPE。
	// 三个键形如 "player:<id>:owner_epoch",**没有**哈希标签({}),在 Redis Cluster 下不同槽,本脚本会被 CROSSSLOT
	// 整体拒绝(所有核实都落到"读不到")。当前部署不是 Cluster;交接链上其它多键脚本(取证、条件写、标记清理)同样
	// 依赖这一点,要上 Cluster 得先统一给这三个键加哈希标签。
	inline constexpr const char *kLuaReadPlacement =
		"#!lua\n"
		"return redis.call('MGET', KEYS[1], KEYS[2], KEYS[3])";

	// 日志里 verdict= 在"这次结清 / 踢线不是由一次核实读数裁决出来的"时的取值(由读数裁决的用 VerdictName)。
	inline constexpr const char *kVerdictUnread = "unread";

	// ── 枚举 ──────────────────────────────────────────────────────────────
	// 底层类型一律 uint8_t:数的是枚举项个数(AGENTS §11.2)。kCount 固定为最后一项,同时是名表长度。名表原文同时是
	// 日志取值与 [RelocateConfirm] 汇总行的 key,runbook 按原文 grep,改名要同步 runbook。全部纯内存使用,不进协议、
	// 不落库。数组长度由初始化项推导,漏加 / 多加一行都会被 static_assert 拦下。

	// 条目所处阶段(现 4 种)。
	enum class Phase : uint8_t
	{
		kMarkWriting,	// 已登记;改派标记的条件写在途(或不写标记、正要发 EnterScene)
		kAwaitingReply, // EnterScene 已交给 gRPC,等完成通知(应答或传输失败)
		kVerifying,		// 有了证据,正在(或等着)读 Redis 核实
		kLanding,		// 进场路由已回到本节点、或核实读到"落在本节点的另一个场景":等载入把实体建出来

		kCount
	};

	constexpr std::size_t kPhaseCount = static_cast<std::size_t>(Phase::kCount);

	inline constexpr const char *kPhaseNames[] = {
		"mark_writing",
		"awaiting_reply",
		"verifying",
		"landing",
	};
	static_assert(std::size(kPhaseNames) == kPhaseCount, "kPhaseNames 必须与 Phase 一一对应");

	constexpr const char *PhaseName(Phase phase)
	{
		const auto index = static_cast<std::size_t>(phase);
		return index < kPhaseCount ? kPhaseNames[index] : "?";
	}

	// 触发核实的证据(现 7 种)。证据只决定"能不能提前踢 / 读不到时踢不踢",踢不踢的主判据永远是核实读数。
	enum class Evidence : uint8_t
	{
		kNotSent,		   // 改派没发出去(scene_manager 注册表为空),或写标记的回调等到 kMarkWriteBudget 也没来
		kReplyRejected,	   // 认领到的应答 error_code 非 0
		kReplySucceeded,   // 认领到的应答 error_code 为 0(仍要核实:可能是放回了同一个场景、路由还在路上)
		kNoReply,		   // 等到 replyWait 也没有完成通知
		kTransportFailed,  // gRPC 传输失败(deadline 到期 / 连接被重置):结果未知,scene_manager 可能仍在处理
		kLandingAbandoned, // 进场路由回到本节点之后,载入被放弃(继承标记核对不过 / 载入失败 / 会话在载入中取消)
		kLandingTimeout,   // 落地等到 kLandingBudget 实体仍没建出来(路由一直没到,或载入卡住)

		kCount
	};

	constexpr std::size_t kEvidenceCount = static_cast<std::size_t>(Evidence::kCount);

	inline constexpr const char *kEvidenceNames[] = {
		"not_sent",
		"reply_rejected",
		"reply_succeeded",
		"no_reply",
		"transport_failed",
		"landing_abandoned",
		"landing_timeout",
	};
	static_assert(std::size(kEvidenceNames) == kEvidenceCount, "kEvidenceNames 必须与 Evidence 一一对应");

	constexpr const char *EvidenceName(Evidence evidence)
	{
		const auto index = static_cast<std::size_t>(evidence);
		return index < kEvidenceCount ? kEvidenceNames[index] : "?";
	}

	// 核实时"没变"指的是什么(现 2 种)。
	enum class Expectation : uint8_t
	{
		kAtSource,	 // location 仍指向本节点上的源场景(改派没生效)
		kAtThisNode, // location 仍指向本节点(落回本节点、载入却没成;不比场景)

		kCount
	};

	constexpr std::size_t kExpectationCount = static_cast<std::size_t>(Expectation::kCount);

	inline constexpr const char *kExpectationNames[] = {
		"at_source",
		"at_this_node",
	};
	static_assert(std::size(kExpectationNames) == kExpectationCount, "kExpectationNames 必须与 Expectation 一一对应");

	constexpr const char *ExpectationName(Expectation expectation)
	{
		const auto index = static_cast<std::size_t>(expectation);
		return index < kExpectationCount ? kExpectationNames[index] : "?";
	}

	// 一次核实读数的结论(现 5 种)。与 travel_outcome::Ownership 是两张表:那张管活着的冻结实体解不解冻,
	// 这张以"本节点已无实体"为前提、只决定踢不踢,不要合并,也不要把 kUnchanged 当成"被回滚到我"去采纳任何东西。
	enum class Verdict : uint8_t
	{
		kAbsent,		 // location 键不存在(含不凭标记的回滚把它删掉了)
		kUnchanged,		 // 仍是期望里的"没变"
		kMovedHere,		 // 落在本节点的另一个场景(只在期望 kAtSource 下出现):等进场路由
		kMovedElsewhere, // 已在别的节点 / 别的 zone
		kIndeterminate,	 // 判不清(location 解析失败、zone 为 0、源场景未知、疏散中的同号节点):不踢

		kCount
	};

	constexpr std::size_t kVerdictCount = static_cast<std::size_t>(Verdict::kCount);

	inline constexpr const char *kVerdictNames[] = {
		"absent",
		"unchanged",
		"moved_here",
		"moved_elsewhere",
		"indeterminate",
	};
	static_assert(std::size(kVerdictNames) == kVerdictCount, "kVerdictNames 必须与 Verdict 一一对应");

	constexpr const char *VerdictName(Verdict verdict)
	{
		const auto index = static_cast<std::size_t>(verdict);
		return index < kVerdictCount ? kVerdictNames[index] : "?";
	}

	// 核实读到结论之后做什么(现 5 种)。
	enum class VerifyAction : uint8_t
	{
		kResolveMoved,		   // 已在别处:结清,不踢
		kAwaitLanding,		   // 落在本节点的另一个场景:转 kLanding 等路由
		kResolveIndeterminate, // 判不清:结清,不踢
		kWaitSettle,		   // 没变,但现在还不许踢:等到 settleAt 再读一次
		kKick,				   // 没变(或已不存在),且允许踢

		kCount
	};

	constexpr std::size_t kVerifyActionCount = static_cast<std::size_t>(VerifyAction::kCount);

	inline constexpr const char *kVerifyActionNames[] = {
		"resolve_moved",
		"await_landing",
		"resolve_indeterminate",
		"wait_settle",
		"kick",
	};
	static_assert(std::size(kVerifyActionNames) == kVerifyActionCount, "kVerifyActionNames 必须与 VerifyAction 一一对应");

	constexpr const char *VerifyActionName(VerifyAction action)
	{
		const auto index = static_cast<std::size_t>(action);
		return index < kVerifyActionCount ? kVerifyActionNames[index] : "?";
	}

	// kLanding 条目每拍的判定结果(现 5 种)。
	enum class LandingAction : uint8_t
	{
		kWait,				// 继续等
		kResolveLanded,		// 实体已建出且会话就是票据那一条:结清
		kResolveSuperseded, // 实体已建出但会话换了(玩家已重登):结清,不踢
		kVerifyAbandoned,	// 进场路由到过、载入却已不在途:转核实(kLandingAbandoned)
		kVerifyTimeout,		// 到截止时刻实体仍没建出来:转核实(kLandingTimeout)

		kCount
	};

	constexpr std::size_t kLandingActionCount = static_cast<std::size_t>(LandingAction::kCount);

	inline constexpr const char *kLandingActionNames[] = {
		"wait",
		"resolve_landed",
		"resolve_superseded",
		"verify_abandoned",
		"verify_timeout",
	};
	static_assert(std::size(kLandingActionNames) == kLandingActionCount,
				  "kLandingActionNames 必须与 LandingAction 一一对应");

	constexpr const char *LandingActionName(LandingAction action)
	{
		const auto index = static_cast<std::size_t>(action);
		return index < kLandingActionCount ? kLandingActionNames[index] : "?";
	}

	// 票据取出之后发不发改派(现 3 种)。
	enum class TicketDecision : uint8_t
	{
		kDispatch,			  // 发
		kVoidClientGone,	  // 作废:本次退出期间客户端已断线,会话已死
		kVoidSessionReplaced, // 作废:实体当前的会话已不是票据那一条

		kCount
	};

	constexpr std::size_t kTicketDecisionCount = static_cast<std::size_t>(TicketDecision::kCount);

	inline constexpr const char *kTicketDecisionNames[] = {
		"dispatch",
		"void_client_gone",
		"void_session_replaced",
	};
	static_assert(std::size(kTicketDecisionNames) == kTicketDecisionCount,
				  "kTicketDecisionNames 必须与 TicketDecision 一一对应");

	constexpr const char *TicketDecisionName(TicketDecision decision)
	{
		const auto index = static_cast<std::size_t>(decision);
		return index < kTicketDecisionCount ? kTicketDecisionNames[index] : "?";
	}

	// 条目怎么收口的(现 8 种)。只有两个 kKick* 会推 tip + 34,其余都不踢。
	enum class Outcome : uint8_t
	{
		kGranted,		   // 成功应答,且读到已在别处
		kMovedElsewhere,   // 没有成功应答,但读到已在别处(别的请求把他放走了)
		kLandedHere,	   // 落回本节点,实体已建出且会话一致(不保证已进场景)
		kSuperseded,	   // 被更新的事实取代:会话已换、同玩家又登记了新条目
		kIndeterminate,	   // 读到了,但判不清
		kKickVerified,	   // 核实"没变"后踢
		kKickUnverified,   // 读不到,但本节点替他发的请求都确定没写过落点:踢
		kGaveUpUnverified, // 读不到,且不能排除已被放行:放弃,不踢

		kCount
	};

	constexpr std::size_t kOutcomeCount = static_cast<std::size_t>(Outcome::kCount);

	inline constexpr const char *kOutcomeNames[] = {
		"granted",
		"moved_elsewhere",
		"landed_here",
		"superseded",
		"indeterminate",
		"kick_verified",
		"kick_unverified",
		"gave_up_unverified",
	};
	static_assert(std::size(kOutcomeNames) == kOutcomeCount, "kOutcomeNames 必须与 Outcome 一一对应");

	constexpr const char *OutcomeName(Outcome outcome)
	{
		const auto index = static_cast<std::size_t>(outcome);
		return index < kOutcomeCount ? kOutcomeNames[index] : "?";
	}

	// 按会话直推 gate 的结果(现 3 种)。汇总行里的 key 加前缀 push_。
	enum class GatePushResult : uint8_t
	{
		kSent,		   // 已交给 RpcSession(底层发送无返回值,不代表客户端收到)
		kGateGone,	   // 找不到 gate / 没有 RpcSession / 连接已断 / 票据没有有效会话
		kGateReplaced, // gate 实例已不是发票时那一个(会话随旧 gate 消失)

		kCount
	};

	constexpr std::size_t kGatePushResultCount = static_cast<std::size_t>(GatePushResult::kCount);

	inline constexpr const char *kGatePushResultNames[] = {
		"sent",
		"gate_gone",
		"gate_replaced",
	};
	static_assert(std::size(kGatePushResultNames) == kGatePushResultCount,
				  "kGatePushResultNames 必须与 GatePushResult 一一对应");

	constexpr const char *GatePushResultName(GatePushResult result)
	{
		const auto index = static_cast<std::size_t>(result);
		return index < kGatePushResultCount ? kGatePushResultNames[index] : "?";
	}

	// 派发时那次改派标记条件写的结局(现 5 种)。**只进日志**,不得进任何判定:kEpochMoved 时本次没写、
	// scene_manager 凭的是别人的标记,从"写没写成"推不出"有没有被放行 / 被回滚"。
	enum class MarkWrite : uint8_t
	{
		kNotAttempted, // 没尝试(owner_epoch 为 0 / Redis 不可用)
		kPending,	   // 已发出,回调未到
		kWritten,	   // 写成了
		kEpochMoved,   // owner_epoch 已不是派发时缓存的值,没写
		kFailed,	   // 命令发不出去 / 空应答 / ERROR / 其它

		kCount
	};

	constexpr std::size_t kMarkWriteCount = static_cast<std::size_t>(MarkWrite::kCount);

	inline constexpr const char *kMarkWriteNames[] = {
		"not_attempted",
		"pending",
		"written",
		"epoch_moved",
		"failed",
	};
	static_assert(std::size(kMarkWriteNames) == kMarkWriteCount, "kMarkWriteNames 必须与 MarkWrite 一一对应");

	constexpr const char *MarkWriteName(MarkWrite markWrite)
	{
		const auto index = static_cast<std::size_t>(markWrite);
		return index < kMarkWriteCount ? kMarkWriteNames[index] : "?";
	}

	// 一条应答 / 传输失败归不归待确认表(现 3 种)。
	enum class ClaimAction : uint8_t
	{
		kNotMine, // 不是本次改派的:落回调用方原有的处理(不计任何 relocate_confirm_stats)
		kIgnore,  // 是本次改派的,但条目已不在等它:只记日志
		kVerify,  // 是本次改派的,且条目正在等它:转核实

		kCount
	};

	constexpr std::size_t kClaimActionCount = static_cast<std::size_t>(ClaimAction::kCount);

	inline constexpr const char *kClaimActionNames[] = {
		"not_mine",
		"ignore",
		"verify",
	};
	static_assert(std::size(kClaimActionNames) == kClaimActionCount, "kClaimActionNames 必须与 ClaimAction 一一对应");

	constexpr const char *ClaimActionName(ClaimAction action)
	{
		const auto index = static_cast<std::size_t>(action);
		return index < kClaimActionCount ? kClaimActionNames[index] : "?";
	}

	// 踢线前补不补写重登凭证(handoff 标记)(现 6 种)。除 kAttempt 外每一项都是一个"不写"的原因,按原因计数。
	// 汇总行里的 key 加前缀 credential_。
	enum class CredentialAction : uint8_t
	{
		kNone,			  // 不需要也不能写:读数不是"没变",或 owner_epoch 读成 0
		kKeepExisting,	  // 已有一份 scene_manager 认得的同代标记,不重复写
		kSkipLocalHolder, // 本节点此刻有该玩家的实体或载入在途:绝不写
		kSkipIdentity,	  // 本节点身份未确认(含疏散中)
		kSkipDevBypass,	  // dev 不安全旁路开着:owner_epoch 条件拦不住旁路放行
		kAttempt,		  // 按读到的 owner_epoch 条件补写

		kCount
	};

	constexpr std::size_t kCredentialActionCount = static_cast<std::size_t>(CredentialAction::kCount);

	inline constexpr const char *kCredentialActionNames[] = {
		"none",
		"keep_existing",
		"skip_local_holder",
		"skip_identity",
		"skip_dev_bypass",
		"attempted",
	};
	static_assert(std::size(kCredentialActionNames) == kCredentialActionCount,
				  "kCredentialActionNames 必须与 CredentialAction 一一对应");

	constexpr const char *CredentialActionName(CredentialAction action)
	{
		const auto index = static_cast<std::size_t>(action);
		return index < kCredentialActionCount ? kCredentialActionNames[index] : "?";
	}

	// ── 派发前:票据作废 ─────────────────────────────────────────────────────

	// 票据取出之后发不发改派。
	//   clientDisconnected      意图组件上的粘性位:本次退出期间收到过客户端断线(会话已死);
	//   currentSessionBound     实体当前的会话快照是一条有效会话(player_exit::IsBoundSession);
	//   currentIsTicketSession  快照会话 == 票据会话。
	// 给已死或已换掉的会话改派,scene_manager 放行后路由会在 gate 被丢掉,location 停在一个从未载入该玩家的节点上,
	// 租约内重登被挑到别处时拿不出那一代的标记(回 18)。作废后由 A1′ 按第一次退出原因决定写不写释放标记。
	// 快照里已没有会话(未绑定)时仍按票据派发:那不是"换了一条会话"的证据。
	constexpr TicketDecision DecideTicket(bool clientDisconnected, bool currentSessionBound,
										  bool currentIsTicketSession)
	{
		if (clientDisconnected)
		{
			return TicketDecision::kVoidClientGone;
		}
		if (currentSessionBound && !currentIsTicketSession)
		{
			return TicketDecision::kVoidSessionReplaced;
		}
		return TicketDecision::kDispatch;
	}

	// 进场路由把玩家带回了本节点,而他手里还有一张没派发的票据:作废不作废。
	//   evacuating              整节点疏散中(BeginEmergencyRelocateAll 之后);
	//   reentrySessionBound     这次进场带来的会话是一条有效会话(player_exit::IsBoundSession);
	//   reentryIsTicketSession  这次进场带来的会话 == 票据里抄下的会话;
	//   exitSawClientDisconnect 被这次进场打断的那次退出,期间已经记过客户端断线(意图组件上的粘性位)。
	// 平时一律作废:这次进场要么复用实体并取消退出,要么废黜实体后重载,那次退出都不会再正常收尾;留着票据,它会在
	// 下一次、会话已换的退出里被误消费。
	// 整节点疏散中,**同一条有效会话**的进场例外,票据留着:进场若取消了他的退出,他就留在这个将死的节点上,而疏散
	// 只发一轮票;留着的票据在他下一次退出收敛时被消费(通常就是节点最后的退出收尾;票据判定不看退出原因,疏散期间
	// 别的原因发起的退出同样会消费它),把同一条会话改派出去,删掉的话这名玩家既不改派也不踢。
	// 带着另一条会话或会话 0 的进场照旧作废 —— 票据里的旧会话已死;尤其会话 0:进场会把实体的会话快照清成无效,
	// 之后的票据判定(DecideTicket)把"没有会话"当成"不是换了会话的证据"而照发,所以必须在这里就作废。
	// 那次退出已经记过客户端断线的也作废:取消退出会连同意图组件上的断线位一起摘掉,下一次退出就不知道这条会话
	// 死过了,留着票据等于给一条本节点自己判过"已断线"的会话改派。
	constexpr bool CancelsTicketOnReentry(bool evacuating, bool reentrySessionBound, bool reentryIsTicketSession,
										  bool exitSawClientDisconnect)
	{
		return !(evacuating && reentrySessionBound && reentryIsTicketSession && !exitSawClientDisconnect);
	}

	// 实体上那条在途的普通换图 EnterScene 是否还可能被 scene_manager 处理:发出不满一个 settle 窗口就算。
	// 墙钟回拨(nowMs < sentAtMs)按"可能"处理 —— 判错的一侧只是多等到 settle,不会误踢。
	constexpr bool SceneChangeReplyMayArrive(bool hasInFlight, uint64_t nowMs, uint64_t sentAtMs,
											 uint64_t settleWindowMs)
	{
		if (!hasInFlight)
		{
			return false;
		}
		if (nowMs < sentAtMs)
		{
			return true;
		}
		return nowMs - sentAtMs < settleWindowMs;
	}

	// ── 应答 / 传输失败的认领 ────────────────────────────────────────────────

	// 被认领的完成通知是否触发核实:只有还在等它的条目。其余阶段的结论由已排好的路径给出
	// (kMarkWriting:自己的请求还没发;kVerifying:已有证据在核实;kLanding:由落地路径定案)。
	constexpr bool VerifiesOnReply(Phase phase)
	{
		return phase == Phase::kAwaitingReply;
	}

	// 应答的 error_code → 证据。非 0 一律只是"被拒",这里不区分码:哪些码可以在读不到时盲踢,由
	// ShouldKickUnverified 的 rejectedBeforeAnyPlacementWrite 入参决定。
	constexpr Evidence EvidenceForReply(uint32_t errorCode)
	{
		return errorCode == 0 ? Evidence::kReplySucceeded : Evidence::kReplyRejected;
	}

	// 一条 EnterScene 应答(实体已不在本节点时)归不归待确认表。replyTag = 应答回显的 correlation_id。
	//   回显号非 0:只有与条目记下的号相等才是本次改派的。条目还没发出(号为 0)时任何带号应答都不是它的。
	//   回显号为 0(旧版 scene_manager 不回显):退回按 player_id —— 有条目就认领。这种认领归属不精确,
	//   粘合层不得据此记"完成通知已到"(Table::MarkCompletion)。
	constexpr ClaimAction DecideReplyClaim(bool hasEntry, uint64_t entryCorrelationId, Phase phase,
										   uint64_t replyTag)
	{
		if (!hasEntry)
		{
			return ClaimAction::kNotMine;
		}
		if (replyTag != 0 && entryCorrelationId != replyTag)
		{
			return ClaimAction::kNotMine;
		}
		return VerifiesOnReply(phase) ? ClaimAction::kVerify : ClaimAction::kIgnore;
	}

	// 一条 EnterScene 传输失败归不归待确认表。requestTag = 失败回调交回的请求里的 correlation_id。
	// 只按号精确匹配,没有 player_id 退路:号为 0 的失败只能来自绕过统一出口的发送,不是改派发的。
	constexpr ClaimAction DecideTransportFailureClaim(bool hasEntry, uint64_t entryCorrelationId, Phase phase,
													  uint64_t requestTag)
	{
		if (!hasEntry || requestTag == 0 || entryCorrelationId != requestTag)
		{
			return ClaimAction::kNotMine;
		}
		return VerifiesOnReply(phase) ? ClaimAction::kVerify : ClaimAction::kIgnore;
	}

	// ── settle 规则与踢线时机 ────────────────────────────────────────────────

	// "本节点替他发的请求里,还有结局未定的"。两种来源:
	//   earlierEnterSceneMayReply            登记时就在途的更早请求可能稍后才被放行 —— scene_manager 的换手门只比
	//                                        标记的代际,它可以凭改派刚写的标记过门。粘合层从三处取它:退出优先作废的
	//                                        交接 EnterScene、实体上在途的普通换图、同一玩家上一次改派尚未收口的条目
	//                                        名下结局未定的请求;
	//   correlationId != 0 && !completionSeen 本次改派已发出、还没认领到它**带应答体**的完成通知(号精确匹配的应答)
	//                                        —— 条目可能已被别的路由提前转进 kLanding,而自己的请求还在 scene_manager 排队。
	//                                        传输失败**不算**:它是"结果未知",deadline 到期之后服务端的 handler 仍可能
	//                                        铸造 / 推路由 / 回滚,所以认领到传输失败不置 completionSeen。
	// 为真时:首次核实推迟到 settle(MustReadAfterSettle)、settle 之前不踢(MayKickBeforeSettle)、读不到时不盲踢
	// (ShouldKickUnverified)。前两道保护以 settleAt 为界;不盲踢没有时间界 —— 带着更早请求登记的条目、
	// 从没拿到应答体的改派,读不到时永远不盲踢(证据后来被落地路径换成 kLandingAbandoned / kLandingTimeout 也一样)。
	constexpr bool RequestOutcomeUnsettled(bool earlierEnterSceneMayReply, uint64_t correlationId,
										   bool completionSeen)
	{
		return earlierEnterSceneMayReply || (correlationId != 0 && !completionSeen);
	}

	// settle 之前是否不许读。为真且 now < settleAt 时,粘合层不发核实命令,直接 Table::WaitForSettle。
	// settle 之前 scene_manager 可能正处在"铸造 → 推路由 / 失败回滚"的窗口里:此刻读到"在别处"会被结清为不踢,
	// 随后的回滚把 location 写回源场景,会话就永远挂在没有实体的玩家上。自己的应答到手时不用等(带应答体的返回都在
	// handler 末尾,回滚已同步做完);结果未知、或还有请求结局未定时,那个窗口可能还开着。
	constexpr bool MustReadAfterSettle(Evidence evidence, bool requestUnsettled)
	{
		return evidence == Evidence::kTransportFailed || requestUnsettled;
	}

	// 核实读到"没变 / 已不存在"时,能否不等 settleAt 就踢。只有"没有请求结局未定,且证据不是成功应答 / 结果未知"才行:
	//   自己的拒绝、没发出去、落地两类 → 可以(不会再有人替他放行);
	//   成功应答却读到没变 → 等:可能是把他放回了正在排空的同一个场景、路由还在路上;settle 之内路由不到才算丢了;
	//   传输失败 → 等(结果未知。MustReadAfterSettle 已挡在前面,这里是纵深防御)。
	// kNoReply 出现时已过 settleAt(replyWait > settleWindow),本函数对它的取值不起作用。
	constexpr bool MayKickBeforeSettle(Evidence evidence, bool requestUnsettled)
	{
		if (requestUnsettled)
		{
			return false;
		}
		return evidence != Evidence::kReplySucceeded && evidence != Evidence::kTransportFailed;
	}

	// 核实读到结论之后做什么。kickAllowedNow = now >= settleAt || MayKickBeforeSettle(...)。
	// 越界的结论按判不清处理(不踢)。
	constexpr VerifyAction DecideAfterVerify(Verdict verdict, bool kickAllowedNow)
	{
		switch (verdict)
		{
		case Verdict::kMovedElsewhere:
			return VerifyAction::kResolveMoved;
		case Verdict::kMovedHere:
			return VerifyAction::kAwaitLanding;
		case Verdict::kUnchanged:
		case Verdict::kAbsent:
			return kickAllowedNow ? VerifyAction::kKick : VerifyAction::kWaitSettle;
		default:
			return VerifyAction::kResolveIndeterminate;
		}
	}

	// 一轮核实(kVerifyBudget)到期仍读不到时踢不踢。本节点读不到 ≠ scene_manager 写不了(带 shebang 的脚本在被降级
	// 的旧主上整体被拒、本节点与 Redis 之间的分区),所以只有"本节点替他发的请求里没有一条可能写过落点"才踢:
	//   1. 还有请求结局未定 → 不踢;
	//   2. 没发出去 / 落地后载入被放弃 / 落地超时 → 踢。这条会话要合法地在别处,只能经过一次新登录,而新登录换了
	//      会话号,按旧会话号踢是空操作;
	//   3. 被拒 → 只有拒绝码证明拒绝发生在本请求任何落点写之前才踢(rejectedBeforeAnyPlacementWrite,由
	//      PlayerLifecycleSystem::SmRejectedBeforeAnyPlacementWrite 给出)。推路由失败那一类的拒绝在回滚没成时
	//      等于已放行到别处,盲踢会打到一条合法会话;
	//   4. 成功应答 / 等不到完成通知 / 传输失败 → 不踢(可能已放行,并把同一条会话改绑到了别处)。
	// 不踢的结清为 kGaveUpUnverified。
	constexpr bool ShouldKickUnverified(Evidence evidence, bool requestUnsettled,
										bool rejectedBeforeAnyPlacementWrite)
	{
		if (requestUnsettled)
		{
			return false;
		}
		switch (evidence)
		{
		case Evidence::kNotSent:
		case Evidence::kLandingAbandoned:
		case Evidence::kLandingTimeout:
			return true;
		case Evidence::kReplyRejected:
			return rejectedBeforeAnyPlacementWrite;
		default:
			return false;
		}
	}

	// ── 落回本节点 ──────────────────────────────────────────────────────────

	// kLanding 条目每拍的判定。
	//   entityValid           本节点此刻有该玩家的有效实体;
	//   entitySessionMatches  实体的会话快照 == 票据会话(entityValid 为假时不看);
	//   reentered             进场路由确实到过本节点(ReconcileRelocateOnReentry 置位);
	//   loadPending           他的载入还在途(待入场表里有他);
	//   pastDeadline          now >= 条目的落地截止时刻。
	// "路由还没到"(!reentered)时不能把"没有载入在途"当成载入被放弃,只能等到截止。
	constexpr LandingAction DecideLanding(bool entityValid, bool entitySessionMatches, bool reentered,
										  bool loadPending, bool pastDeadline)
	{
		if (entityValid)
		{
			return entitySessionMatches ? LandingAction::kResolveLanded : LandingAction::kResolveSuperseded;
		}
		if (reentered && !loadPending)
		{
			return LandingAction::kVerifyAbandoned;
		}
		if (pastDeadline)
		{
			return LandingAction::kVerifyTimeout;
		}
		return LandingAction::kWait;
	}

	// ── 核实读数的解释 ──────────────────────────────────────────────────────

	// 非空、纯十进制数字、数值不超过 maxValue 时返回数值,否则返回空。手写解析(不用 strtoull / from_chars):
	// 要在编译期可求值,并且不接受符号、空白、十六进制前缀这些 C 库会放过的写法。前导零允许。
	constexpr std::optional<uint64_t> ParseDecimalUpTo(std::string_view text, uint64_t maxValue)
	{
		if (text.empty())
		{
			return std::nullopt;
		}
		uint64_t value = 0;
		for (const char ch : text)
		{
			if (ch < '0' || ch > '9')
			{
				return std::nullopt;
			}
			const auto digit = static_cast<uint64_t>(ch - '0');
			if (digit > maxValue || value > (maxValue - digit) / 10)
			{
				return std::nullopt; // value * 10 + digit 会超过 maxValue
			}
			value = value * 10 + digit;
		}
		return value;
	}

	// owner_epoch 键的文本 → 数值。不是完整的十进制串(含空串、溢出)时返回 0;键不存在(NIL)时调用方同样取 0。
	// 0 在凭证判定里就是"不写"(DecideCredential):条件写按字符串比较,对写坏的值本来也写不进去,这样只是让日志
	// 与计数如实。不把 "7a" 截成 7。
	constexpr uint64_t ParseOwnerEpoch(std::string_view text)
	{
		const auto value = ParseDecimalUpTo(text, (std::numeric_limits<uint64_t>::max)());
		return value.has_value() ? *value : 0;
	}

	// handoff 键的原文是不是一份 scene_manager 认得的、代际等于 epoch 的标记:**整串**形如 "数字:数字"。
	// 按第一个 ':' 切开;前缀是非空纯十进制数字且数值等于 epoch;后缀是非空纯十进制数字(不含第二个 ':'、符号或
	// 任何其它字符)且不超过 int64 上限。与 go/shared/ownerepoch 的 ParseHandoff(后缀走 strconv.ParseInt)、
	// exit_release_mark::kLuaInheritClear('^(%d+):%d+$')同一口径,取三者里最严的:本函数为真 ⇒ scene_manager
	// 一定认这份标记。写坏的标记(前缀对、后缀不是数字)换手门会按"未落盘"回 18,所以这里返回 false、按没有处理,
	// 让后面的条件补写把它覆盖掉;判错的一侧只是多写一次同代标记。
	constexpr bool MarkCarriesEpoch(std::string_view mark, uint64_t epoch)
	{
		const auto colon = mark.find(':');
		if (colon == std::string_view::npos)
		{
			return false;
		}
		const auto markEpoch = ParseDecimalUpTo(mark.substr(0, colon), (std::numeric_limits<uint64_t>::max)());
		if (!markEpoch.has_value() || *markEpoch != epoch)
		{
			return false;
		}
		return ParseDecimalUpTo(mark.substr(colon + 1),
								static_cast<uint64_t>((std::numeric_limits<int64_t>::max)()))
			.has_value();
	}

	// ClassifyPlacement 的输入。全部由粘合层从同一次核实应答与本节点状态里采集好传入,本文件不读任何全局状态。
	// 默认值取"不踢"的一侧:漏填时判 kIndeterminate(present 为真、parsed 为假),不是可踢的 kAbsent。
	struct PlacementFacts
	{
		Expectation expectation{Expectation::kAtSource};
		bool present{true};			// location 元素是 STRING(键存在)。必须按元素类型取:把解析失败当成不存在会误踢
		bool parsed{false};			// present 且 PlayerLocation 解析成功
		uint32_t locationZoneId{0}; // location.zone_id
		uint32_t selfZoneId{0};		// 本节点的 zone(GetZoneId())
		bool nodeIsSelf{false};		// location.node_id 是本节点号(node_id 只在 zone 内唯一,还要再比 zone)
		uint64_t locationSceneId{0};
		uint64_t sourceSceneId{0}; // 退出发起时所在的场景;0 = 未知
		bool evacuating{false};	   // 整节点疏散中(本节点号可能已被新进程复用)
	};

	// 核实读到的 location 说明了什么。只看 zone / node / scene:scene_manager 的回滚恢复的是节点与场景,
	// owner_epoch 与回执会变,所以它们不是输入。
	//   键不存在 → kAbsent;解析失败 → kIndeterminate;
	//   节点号不是本节点 → kMovedElsewhere;
	//   任一 zone 为 0 → kIndeterminate(zone 0 证明不了物理节点身份:把它当成本 zone,会把别的 zone 里的同号节点读成
	//     本节点,随后的条件补写等于给那边的活持有者发放行证。与归属取证、scene_manager 的同节点判定同向);
	//   zone 不同 → kMovedElsewhere;
	//   期望 kAtThisNode:疏散中 → kIndeterminate(本节点号可能已被新进程复用,指向这个号什么也证明不了),否则 kUnchanged;
	//   期望 kAtSource:源场景未知 → kIndeterminate;场景相同 → kUnchanged;疏散中 → kMovedElsewhere(将死节点不等路由);
	//     其余 → kMovedHere。
	constexpr Verdict ClassifyPlacement(const PlacementFacts &facts)
	{
		if (!facts.present)
		{
			return Verdict::kAbsent;
		}
		if (!facts.parsed)
		{
			return Verdict::kIndeterminate;
		}
		if (!facts.nodeIsSelf)
		{
			return Verdict::kMovedElsewhere;
		}
		if (facts.locationZoneId == 0 || facts.selfZoneId == 0)
		{
			return Verdict::kIndeterminate;
		}
		if (facts.locationZoneId != facts.selfZoneId)
		{
			return Verdict::kMovedElsewhere;
		}
		switch (facts.expectation)
		{
		case Expectation::kAtThisNode:
			return facts.evacuating ? Verdict::kIndeterminate : Verdict::kUnchanged;
		case Expectation::kAtSource:
			if (facts.sourceSceneId == 0)
			{
				return Verdict::kIndeterminate;
			}
			if (facts.locationSceneId == facts.sourceSceneId)
			{
				return Verdict::kUnchanged;
			}
			return facts.evacuating ? Verdict::kMovedElsewhere : Verdict::kMovedHere;
		default:
			return Verdict::kIndeterminate; // 越界的期望:判不清,不踢
		}
	}

	// DecideCredential 的输入。默认值全部取"不写"的一侧(fail-closed):漏填任何一项都只会少写。
	struct CredentialFacts
	{
		Verdict verdict{Verdict::kIndeterminate}; // 本次核实的结论
		uint64_t redisOwnerEpoch{0};			  // 同一次核实读到的 owner_epoch(ParseOwnerEpoch;键不存在 / 写坏 = 0)
		bool markCarriesEpoch{false};			  // 同一次核实读到的 handoff 是同代有效标记(MarkCarriesEpoch)
		bool localHolderPossible{true};			  // 本节点此刻有该玩家的有效实体,或待入场表里有他
		bool identityConfirmed{false};			  // 本节点身份当前确认有效(疏散中 / 租约推定过期 = 否)
		bool devUnsafeCrossNode{true};			  // dev 不安全跨节点旁路开着
	};

	// 踢线前补不补写重登凭证。补写 = 以"owner_epoch 仍等于读到的 X"为条件写一份 "X:now"(exit_release_mark 的
	// kLuaWriteIfOwnerEpoch),让被踢的玩家租约内重登到别的节点时过得了换手门。按顺序判,第一个命中的就是结果:
	//   1. 读数不是"没变",或 X 为 0 → kNone(已不存在 / 在别处时没有凭证可言;X 为 0 写出来的 "0:now" 没有意义);
	//   2. 已有同代有效标记 → kKeepExisting。scene_manager 的回滚自己会把所凭标记转写到新代际,改派时写的那份
	//      多半还在;重复写只会改掉后缀与 TTL。不需要写的时候不看后面的闸;
	//   3. 本节点有该玩家实体或载入在途 → kSkipLocalHolder。补写排在那次载入的标记清理之后,新持有期间会留着一份
	//      有效标记,下一次跨节点落点不存盘就过门(回档)。这是硬前提,不靠"条目存在即无实体"的可达性论证;
	//   4. 身份未确认 → kSkipIdentity。新进程可能复用本节点号,同号落点不铸造,晚到的标记会给它开放行口;
	//   5. dev 旁路开着 → kSkipDevBypass。旁路下跨节点落点不铸造,owner_epoch 条件拦不住"读完之后被放到别处",
	//      而旁路下没有标记也能过门,不写没有损失;
	//   6. 其余 → kAttempt。
	constexpr CredentialAction DecideCredential(const CredentialFacts &facts)
	{
		if (facts.verdict != Verdict::kUnchanged || facts.redisOwnerEpoch == 0)
		{
			return CredentialAction::kNone;
		}
		if (facts.markCarriesEpoch)
		{
			return CredentialAction::kKeepExisting;
		}
		if (facts.localHolderPossible)
		{
			return CredentialAction::kSkipLocalHolder;
		}
		if (!facts.identityConfirmed)
		{
			return CredentialAction::kSkipIdentity;
		}
		if (facts.devUnsafeCrossNode)
		{
			return CredentialAction::kSkipDevBypass;
		}
		return CredentialAction::kAttempt;
	}

	// ── 条目 ──────────────────────────────────────────────────────────────

	// 登记时由调用方给出的部分(票据 + 派发那一刻抄下来的事实)。
	struct Registration
	{
		uint64_t playerId{0};
		uint32_t sessionId{0};		// 票据里的 gate 会话:核实之后按它直推 tip + 34
		uint32_t gateNodeId{0};
		std::string gateInstanceId; // 发票时 gate 的实例号;空 = 推送时不比对实例
		uint64_t ownerEpoch{0};		// 派发时实体缓存的 owner_epoch。可能落后于 Redis,**只进日志**
		uint64_t sourceSceneId{0};	// 退出发起时所在的场景(PlayerExitIntentComp.sceneIdAtExit);0 = 未知
		std::string markValue;		// 本次**尝试**写的标记原文 "E:ms";没尝试为空。**只进日志**(见 MarkWrite)
		// 本节点替他发出的更早 EnterScene 还可能被处理(见 RequestOutcomeUnsettled)。默认取保守的一侧:
		// 漏填只会多等到 settle、读不到时不盲踢。
		bool earlierEnterSceneMayReply{true};
	};

	struct Entry : Registration
	{
		uint64_t seq{0}; // 表内单调发号,从 1 起;回调凭 (playerId, seq) 对号
		Phase phase{Phase::kMarkWriting};
		Evidence evidence{Evidence::kNotSent}; // 进入 kVerifying 时写入;此前是初值,没有含义
		Expectation expectation{Expectation::kAtSource};
		uint64_t correlationId{0};					   // 本次改派 EnterScene 的关联号;0 = 没发出
		MarkWrite markWrite{MarkWrite::kNotAttempted}; // 派发时条件写的结局,只进日志
		bool completionSeen{false}; // 已认领到本次改派带应答体的完成通知(号精确匹配的应答);传输失败不置位
		// 被认领的那条应答的 error_code;未认领(含只认领到传输失败)为 0。只供盲踢的白名单判定(ShouldKickUnverified 的
		// 第三个入参)与日志。
		uint32_t replyErrorCode{0};
		bool reentered{false};		// kLanding:进场路由确实到过本节点
		bool verifyInFlight{false}; // 有一条核实命令在途(只由 MarkVerifySent 置位)
		bool retryPending{false};	// 上一次核实没读到、等重试(重连时可以提前重发)
		uint32_t verifyGen{0};		// 核实代际:只由 MarkVerifySent 自增
		Clock::time_point createdAt{};
		Clock::time_point sentAt{};
		// 当前阶段的截止时刻。各阶段含义不同:写标记的回调 / 等完成通知 / 本轮核实 / 落地等载入。
		Clock::time_point deadline{};
		// 过了它 scene_manager 不会再处理本次改派。登记时先按登记时刻算,发出时(MarkSent)按发送时刻重算。
		Clock::time_point settleAt{};
		Clock::time_point nextVerifyAt{}; // kVerifying 且不在途时,最早何时(再)发核实
	};

	// 见上面的 RequestOutcomeUnsettled。
	constexpr bool RequestOutcomeUnsettled(const Entry &entry)
	{
		return RequestOutcomeUnsettled(entry.earlierEnterSceneMayReply, entry.correlationId, entry.completionSeen);
	}

	// 一条核实应答(发出时的代际为 gen)现在还作不作数。调用方先用 Table::Find(playerId, seq) 对上条目,再过本判据;
	// 不通过的应答一律丢弃(空应答 / ERROR 也一样,不得拿去 MarkVerifyDeferred)。
	// 为什么 gen 相等还不够:verifyGen 只在 MarkVerifySent 时自增,条目被转去落地(BeginLanding)或重新开始一轮核实
	// (BeginVerify)而新命令还没发时,在途旧应答的 gen 仍然对得上 —— 它读到的"仍指向源场景"会踢掉正在本节点载入的
	// 合法会话。BeginVerify / BeginLanding / WaitForSettle / MarkVerifyDeferred 都清 verifyInFlight、不动 verifyGen,
	// 所以"在途且 gen 相等"只可能对应最近一次 MarkVerifySent 发出的那条命令。
	constexpr bool AcceptsVerifyReply(const Entry &entry, uint32_t gen)
	{
		return entry.phase == Phase::kVerifying && entry.verifyInFlight && entry.verifyGen == gen;
	}

	// ── 待确认表 ────────────────────────────────────────────────────────────

	class Table
	{
	public:
		explicit Table(std::size_t capacity = kMaxTracked) : capacity_(capacity) {}

		// 登记一条改派,返回它的 seq(>= 1)。初始:kMarkWriting,deadline = now + kMarkWriteBudget,
		// settleAt = now + budgets.settleWindow。
		// 同玩家已有条目时替换,旧条目经 supersededOut 带出(由调用方按 kSuperseded 结清并记日志;按契约它只可能是
		// kLanding 条目);其余情况 supersededOut 置空。
		// 表满(且不是替换)时返回 0、**不淘汰**任何条目:调用方计数后按旧行为发出、不跟踪。
		uint64_t Add(const Registration &registration, Clock::time_point now, const Budgets &budgets,
					 std::optional<Entry> &supersededOut)
		{
			supersededOut.reset();
			const auto existing = entries_.find(registration.playerId);
			if (existing != entries_.end())
			{
				supersededOut = std::move(existing->second);
				entries_.erase(existing);
			}
			else if (entries_.size() >= capacity_)
			{
				return 0;
			}
			Entry entry;
			static_cast<Registration &>(entry) = registration;
			entry.seq = nextSeq_++;
			entry.phase = Phase::kMarkWriting;
			entry.createdAt = now;
			entry.deadline = now + kMarkWriteBudget;
			entry.settleAt = now + budgets.settleWindow;
			const uint64_t seq = entry.seq;
			entries_.insert_or_assign(registration.playerId, std::move(entry));
			return seq;
		}

		// 按 (playerId, seq) 精确找:回调用它对号,seq 对不上(条目已结清 / 已被新一次改派替换)返回 nullptr。
		// 返回的指针只在本次调用栈里用:任何可能结清条目的调用(含发 Redis 命令)之后必须重新 Find。
		Entry *Find(uint64_t playerId, uint64_t seq)
		{
			Entry *entry = FindPlayer(playerId);
			return entry != nullptr && entry->seq == seq ? entry : nullptr;
		}

		// 只按 playerId 找(应答 / 传输失败的认领、进场 Reconcile:它们手里没有 seq)。
		Entry *FindPlayer(uint64_t playerId)
		{
			const auto it = entries_.find(playerId);
			return it == entries_.end() ? nullptr : &it->second;
		}

		// 取走并删除。结清 / 踢线都先 Take 再做副作用,重入时拿不到第二次。
		std::optional<Entry> Take(uint64_t playerId)
		{
			const auto it = entries_.find(playerId);
			if (it == entries_.end())
			{
				return std::nullopt;
			}
			std::optional<Entry> taken{std::move(it->second)};
			entries_.erase(it);
			return taken;
		}

		std::size_t size() const { return entries_.size(); }

		std::array<std::size_t, kPhaseCount> CountByPhase() const
		{
			std::array<std::size_t, kPhaseCount> counts{};
			for (const auto &item : entries_)
			{
				const auto index = static_cast<std::size_t>(item.second.phase);
				if (index < kPhaseCount)
				{
					++counts[index];
				}
			}
			return counts;
		}

		// 全部条目的副本,按 seq(登记顺序)排好。只给排障日志(drain 剩余条目的逐条列表)用,不在任何定时路径上。
		std::vector<Entry> Snapshot() const
		{
			std::vector<Entry> entries;
			entries.reserve(entries_.size());
			for (const auto &item : entries_)
			{
				entries.push_back(item.second);
			}
			std::sort(entries.begin(), entries.end(),
					  [](const Entry &lhs, const Entry &rhs) { return lhs.seq < rhs.seq; });
			return entries;
		}

		// ── 阶段迁移(静态:只改传入的条目,不碰表)──

		// EnterScene 即将交给 gRPC:**先记号、后发**(SendCorrelatedEnterScene 的前置条件)。应答与 settle 的钟都从
		// 发送时刻重新起算。
		static void MarkSent(Entry &entry, Clock::time_point now, uint64_t correlationId, const Budgets &budgets)
		{
			entry.phase = Phase::kAwaitingReply;
			entry.correlationId = correlationId;
			entry.sentAt = now;
			entry.deadline = now + budgets.replyWait;
			entry.settleAt = now + budgets.settleWindow;
		}

		// 已认领到本次改派带应答体的完成通知。只由应答的认领调,且只在号精确匹配时(传它的 error_code)。
		// **不调**的两种:回显号为 0 的退路认领(归属不精确);传输失败(结果未知,不是结局 —— 把它记成"已完成",
		// 条目随后经落地路径重新核实时,"结果未知"的三道保护就全没了,见 RequestOutcomeUnsettled)。
		// 只认第一次:一次 unary 调用只会完成一次,第二次调用按构造不可达;真到了也不让它改写已记下的拒绝码
		// (盲踢的白名单判定与日志都看它)。不改阶段,不改任何时间点。
		static void MarkCompletion(Entry &entry, uint32_t replyErrorCode)
		{
			if (entry.completionSeen)
			{
				return;
			}
			entry.completionSeen = true;
			entry.replyErrorCode = replyErrorCode;
		}

		// 开始一轮核实。清 verifyInFlight(在途的旧应答随之作废,见 AcceptsVerifyReply),**不动** verifyGen。
		// nextVerifyAt = now:紧接着的发送若没发生,1s 节拍会把它捡起来。
		static void BeginVerify(Entry &entry, Clock::time_point now, Evidence evidence, Expectation expectation)
		{
			entry.phase = Phase::kVerifying;
			entry.evidence = evidence;
			entry.expectation = expectation;
			entry.deadline = now + kVerifyBudget;
			entry.nextVerifyAt = now;
			entry.verifyInFlight = false;
			entry.retryPending = false;
		}

		// 等到 settleAt 再读(可以在"还没读过"时调用:结果未知 / 还有请求结局未定)。本轮核实的截止时刻顺延到
		// settleAt 之后还有一整轮。清 retryPending:重连不绕过 settle 等待。
		static void WaitForSettle(Entry &entry)
		{
			entry.nextVerifyAt = entry.settleAt;
			entry.deadline = (std::max)(entry.deadline, entry.settleAt + kVerifyBudget);
			entry.verifyInFlight = false;
			entry.retryPending = false;
		}

		// 一条核实命令即将发出,返回它的代际(回调按值带着它)。唯一置 verifyInFlight、自增 verifyGen 的地方。
		static uint32_t MarkVerifySent(Entry &entry)
		{
			entry.verifyInFlight = true;
			entry.retryPending = false;
			return ++entry.verifyGen;
		}

		// 这次核实没读到(Redis 不可用 / 命令发不出 / 空应答 / ERROR / 形状不对):过 kVerifyRetryInterval 再试。
		static void MarkVerifyDeferred(Entry &entry, Clock::time_point now)
		{
			entry.verifyInFlight = false;
			entry.retryPending = true;
			entry.nextVerifyAt = now + kVerifyRetryInterval;
		}

		// 转入(或留在)落地等载入。
		//   reentered    true = 进场路由到了本节点(ReconcileRelocateOnReentry);false = 核实读到"落在本节点的另一个
		//                场景",路由还没到;
		//   loadPending  调用当刻待入场表里有没有该玩家 —— Reconcile 在登记新的待入场条目之前执行,所以它读到的是
		//                "上一次载入还在不在"。
		// 同一条路由可能到两次(gate 侧补发),所以要幂等:
		//   * 不在 kLanding → 转入:截止 = now + kLandingBudget;清 verifyInFlight / retryPending(在途核实作废,
		//     verifyGen 不动);
		//   * 已在 kLanding、入参 reentered 为假 → 什么都不改(按构造不可达,防御);
		//   * 已在 kLanding、条目已 reentered、上一次载入还在途 → 什么都不改(同一次载入的重复路由);
		//   * 其余(条目还没 reentered;或上一次载入已结束 = 真正的第二次进场)→ reentered 置真、截止重新起算。
		//     不重算的话,新一次载入可能在途中撞上第一次的截止时刻,被判超时而踢。
		static void BeginLanding(Entry &entry, Clock::time_point now, bool reentered, bool loadPending)
		{
			if (entry.phase != Phase::kLanding)
			{
				entry.phase = Phase::kLanding;
				entry.reentered = reentered;
				entry.deadline = now + kLandingBudget;
				entry.verifyInFlight = false;
				entry.retryPending = false;
				return;
			}
			if (!reentered)
			{
				return;
			}
			if (entry.reentered && loadPending)
			{
				return;
			}
			entry.reentered = true;
			entry.deadline = now + kLandingBudget;
		}

		// ── 到期归类 ──

		// 一次扫描的结果。三个列表里是 (playerId, seq),不是指针:调用方逐个处理时会结清条目,处理前必须重新 Find。
		struct Due
		{
			std::vector<std::pair<uint64_t, uint64_t>> toVerify;	   // 该发(或重发)核实的
			std::vector<std::pair<uint64_t, uint64_t>> verifyExpired;  // 一轮核实到期仍读不到的
			std::vector<std::pair<uint64_t, uint64_t>> landingToCheck; // kLanding:每拍都要看实体建出来没有
			std::size_t markWriteTimedOut{0};						   // 本次从 kMarkWriting 超时转核实的条数
			std::size_t replyTimedOut{0};							   // 本次从 kAwaitingReply 超时转核实的条数
		};

		// 每条条目至多落进一个列表(恰好等于截止时刻即到期):
		//   kMarkWriting 到期   → BeginVerify(kNotSent, kAtSource),进 toVerify;
		//   kAwaitingReply 到期 → BeginVerify(kNoReply, kAtSource),进 toVerify;
		//   kVerifying 到期     → 只进 verifyExpired,不管是否在途(黑洞连接不会回空应答);
		//   kVerifying 未到期、不在途,且 now >= nextVerifyAt 或(reconnected 且 retryPending)→ 进 toVerify。
		//     重连只对"上次没读到、等重试"的条目生效,不绕过 settle 等待;
		//   kLanding            → 进 landingToCheck。
		Due CollectDue(Clock::time_point now, bool reconnected)
		{
			Due due;
			for (auto &item : entries_)
			{
				Entry &entry = item.second;
				switch (entry.phase)
				{
				case Phase::kMarkWriting:
					if (now >= entry.deadline)
					{
						BeginVerify(entry, now, Evidence::kNotSent, Expectation::kAtSource);
						due.toVerify.emplace_back(entry.playerId, entry.seq);
						++due.markWriteTimedOut;
					}
					break;
				case Phase::kAwaitingReply:
					if (now >= entry.deadline)
					{
						BeginVerify(entry, now, Evidence::kNoReply, Expectation::kAtSource);
						due.toVerify.emplace_back(entry.playerId, entry.seq);
						++due.replyTimedOut;
					}
					break;
				case Phase::kVerifying:
					if (now >= entry.deadline)
					{
						due.verifyExpired.emplace_back(entry.playerId, entry.seq);
					}
					else if (!entry.verifyInFlight &&
							 (now >= entry.nextVerifyAt || (reconnected && entry.retryPending)))
					{
						due.toVerify.emplace_back(entry.playerId, entry.seq);
					}
					break;
				case Phase::kLanding:
					due.landingToCheck.emplace_back(entry.playerId, entry.seq);
					break;
				default:
					break;
				}
			}
			return due;
		}

	private:
		// 哈希表:疏散时每个在线玩家一条,认领 / Reconcile / 回调都按 playerId 查。
		std::unordered_map<uint64_t, Entry> entries_;
		std::size_t capacity_;
		uint64_t nextSeq_{1};
	};
} // namespace relocate_confirm
