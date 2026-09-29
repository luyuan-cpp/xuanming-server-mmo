#pragma once

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <iterator>

// 归属交接冻结的硬上限 + 晚发闸 + 看门狗单调复核 —— 纯常量与纯判定,不含 entt / redis / muduo,不读任何时钟,
// 可单测(cpp/tests/cross_zone_test 的 TravelFreezeCap*)。风格同 handoff_mark_withdraw.h / exit_release_mark.h。
// 粘合代码在 player_lifecycle.cpp(EnforceTravelFreezeCaps / ExpireTravelFreeze / ConcludeHandoffAfterMarkSent /
// RunAtMonotonic / BeginTravelHandoff 与 RequestTravelEnterScene 的晚发闸)。
// 设计:docs/design/cross-zone-scene-travel.md §12.3「冻结上限」行,以及「交接冻结硬上限 + 晚发闸 +『标记已发出』
// 统一收口」一节(按标题引用,章节号以文档为准)。
//
// ── 为什么需要 ──
// 交接冻结(PlayerFrozenComp)期间玩家什么都做不了,客户端挂着"传送中"遮罩。原先的冻结只靠两道 30s 看门狗
// 收敛,但有三段没有上限,zone Redis 不可用 / 半开连接时玩家会一直冻着:
//   (U1 / U3 按当时的写法描述;GO-2 起核实是一次原子取证 EVAL,失败形态与处置相同)
//   U1  ResolveTravelOutcome 核实归属时 Redis 不可用 / MGET 应答形状不对 / 命令发不出去,三处都重挂 30s 应答
//       看门狗,次数与总时长都不封顶;
//   U2  BeginTravelHandoff 先写 requestedAtMs 再发 SET,只有 SET 的成功回调才会走到 RequestTravelEnterScene;
//       SET 回调一直不来(半开连接)时存盘看门狗看到 requestedAtMs != 0 直接返回,应答看门狗又还没挂;
//   U3  ResolveTravelOutcome 的 DEL + MGET 发出后不挂任何定时器,应答不来就一直等。
// 本文件给整段冻结加一个按单调时钟计的硬上限 kFreezeCap,再加一道"晚发闸"保证窗口内发出的请求来得及在上限前核实。
//
// ── 时序推导(static_assert 守着,改任何一个常量都要重新过一遍)──
//   存盘看门狗 kSaveBudget 30s ≤ 晚发窗口 kDispatchWindow 35s:存盘阶段先由存盘看门狗收敛,
//       set_mark 阶段的晚发闸只是纵深防御;
//   kDispatchWindow 35 + kReplyBudget 30 + kVerifyMargin 5 ≤ kFreezeCap 70:窗口内发出的 EnterScene,它的应答
//       看门狗 + 一次原子取证核实一定在上限之前做完,只有"归属确实核实不了"时才轮到上限;
//   kFreezeCap 70 + 扫描周期 kSweepInterval 1 = 71 < 客户端预算 kClientAcceptedHandoffBudget 75:服务端最迟 71s
//       给出结论(解冻 + tip,或 tip + 踢线 34),客户端遮罩还没超时。
//
// ── 骨架的第一刀:handoff 标记的 SET 发没发出去(IsHandoffMarkSent)──
//   没发出 → 不可能已被放行(不变量 I0),解冻 + 失败 tip(AbortTravelHandoff);
//   已发出 → 归属可能已交出去,本文件的处置**永不解冻**:tip + 踢线 34 + 不存盘销毁(ConcludeHandoffAfterMarkSent)。
//   SET 发出后的解冻只有 ResolveTravelOutcome 的两类正向证据(GO-2 判定表 B4 / B5,cross-zone-scene-travel.md §12.8):
//   在同一段原子脚本里删掉本次交接这一族标记之后读到 owner_epoch 未变;或读到本次标记原文的回滚回执、且其余交叉校验
//   成立(采纳回滚后的 epoch 再解冻)。本文件不涉及。
//
// ── 单调时钟管到哪里 ──
//   判定(到没到上限、窗口关没关、看门狗是不是提前醒了)全部按 steady_clock;触发仍靠 muduo 定时器,而 muduo 按
//   墙钟(Timestamp::now,Windows 上走 system_clock)到期。所以墙钟**前跳**不会让任何判定提前(看门狗提前醒来会
//   按剩余时间重挂,见 ShouldRearmEarlyFire);墙钟**回拨** X 秒会把所有 muduo 定时器(含 1s 扫描)推迟最多 X 秒,
//   这是引擎级残余,本文件不修。
namespace travel_freeze_cap
{
	using Clock = std::chrono::steady_clock;

	// 存盘阶段看门狗(PlayerLifecycleSystem::ArmTravelSaveWatchdog):冻结后这么久存盘还没落地(交接还没发起)
	// 就解冻。这一段 handoff 标记还没写,解冻是安全的。
	inline constexpr std::chrono::seconds kSaveBudget{30};

	// EnterScene 应答看门狗(PlayerLifecycleSystem::ArmTravelReplyWatchdog)。scene_manager 不可达 / 连接被重置 /
	// zrpc 服务端超时(scene_manager_service.yaml 的 Timeout: 8000)/ C++ 侧 gRPC deadline 到期(base_deploy_config.yaml
	// GrpcClient.CallDeadlineMs,SceneManager 10000)时,生成的客户端会调失败处理器,但传输失败 = 结果未知:
	// 对交接只记日志、不当证据(PlayerLifecycleSystem::DispatchEnterSceneTransportFailure),源端仍只能等本看门狗
	// 按 owner_epoch 裁决 —— 没有这道看门狗,玩家会以冻结态挂到冻结上限。取值远大于 EnterScene 的正常耗时
	// (压测 P99 亚秒)与 gRPC deadline,只兜真正的丢应答。
	// 下限约束(别为了缩短"应答丢失时源实体冻结着留在场景里"的时间把它调小):必须远大于 scene_manager
	// "铸造 epoch → Kafka 路由 ACK / 失败回滚"这段窗口(KafkaWriteTimeoutSeconds,默认 5s)。看门狗按
	// owner_epoch 变没变裁决去留,落在窗口里会读到一个随后本该被回滚的新 epoch:取证已原子删掉本族标记,
	// 迟到的回滚只能回 marker_gone、保留落点 —— 源实体已销毁,location 却停在从未载入的目标上,玩家在线却没有实体。
	inline constexpr std::chrono::seconds kReplyBudget{30};

	// 一次核实往返(ResolveTravelOutcome 的原子取证 EVAL:删本族标记 + 读 owner_epoch / location)的余量。
	inline constexpr std::chrono::seconds kVerifyMargin{5};

	// 冻结硬上限(从 StartTravelHandoff 冻结那一刻起,按单调时钟)。到期不再等 Redis:标记没发出 → 解冻 + tip;
	// 已发出 → tip + 踢线 34 + 不存盘销毁。
	inline constexpr std::chrono::seconds kFreezeCap{70};

	// 晚发闸:冻结超过这么久就不再发出任何 handoff SET / EnterScene。保证窗口内发出的请求,其应答看门狗 +
	// 一次核实都能在上限之前做完。
	inline constexpr std::chrono::seconds kDispatchWindow = kFreezeCap - kReplyBudget - kVerifyMargin;

	// 上限扫描周期(PlayerLifecycleSystem::EnforceTravelFreezeCaps 的 1s 定时器)。
	inline constexpr std::chrono::seconds kSweepInterval{1};

	// 看门狗提前醒来的容差。两只时钟的频率差(对时调速最多约 500ppm,30s 内约 15ms)和 muduo 的微秒截断会造成
	// 毫秒级的伪提前;提前量不超过它就直接执行,超过才按剩余时间重挂并计 watchdog_early_fire ——
	// 这样该计数非 0 才能解读为"墙钟向前跳过 ≥1s"。提前至多 1s 执行是安全的:见下面 kEarlyFireTolerance ≤
	// kVerifyMargin 的 static_assert;应答看门狗 29s 仍远大于 scene_manager 铸造 → 路由 / 回滚的 5s 窗口。
	inline constexpr std::chrono::seconds kEarlyFireTolerance{1};

	// 镜像 mmorpg-client 的 CityTravelRequest.AcceptedHandoffBudgetSeconds(客户端"已受理的交接"等结论的预算)。
	// **两边改动必须同步**:客户端预算从发请求之前就开始计(GameClient.BeginZoneTravel),而同 zone 换图的服务端
	// 冻结要等 18 那次往返回来之后才开始,所以 kFreezeCap + kSweepInterval 与它之间的 4s 余量要同时盖住网络往返
	// 与 18 的往返。余量不够时客户端会先收起遮罩、之后才收到 tip / 34,只影响观感。
	inline constexpr std::chrono::seconds kClientAcceptedHandoffBudget{75};

	static_assert(kSaveBudget <= kDispatchWindow,
				  "存盘看门狗必须先于晚发窗口关闭到期:set_mark 阶段的晚发闸只是纵深防御");
	static_assert(kDispatchWindow + kReplyBudget + kVerifyMargin <= kFreezeCap,
				  "窗口内发出的 EnterScene,其应答看门狗 + 一次核实必须在冻结上限之前做完");
	static_assert(kEarlyFireTolerance <= kVerifyMargin, "看门狗提前执行的容差不得吃掉核实余量");
	static_assert(kFreezeCap + kSweepInterval < kClientAcceptedHandoffBudget,
				  "服务端最迟出结论的时刻必须早于客户端遮罩超时(同步改 CityTravelRequest.AcceptedHandoffBudgetSeconds)");

	// 到上限时怎么处置。底层类型 uint8_t 数的是处置种类数(现 3 种)。纯内存使用,不进协议、不落库。
	enum class FreezeCapAction : uint8_t
	{
		kNone,     // 还没到上限
		kUnfreeze, // 到上限且 handoff 标记从没发出:AbortTravelHandoff(解冻 + 失败 tip)
		kDestroy,  // 到上限且标记已发出:ConcludeHandoffAfterMarkSent(tip + 踢线 34 + 不存盘销毁)

		kCount
	};

	constexpr std::size_t kFreezeCapActionCount = static_cast<std::size_t>(FreezeCapAction::kCount);

	// 只给日志用的名字,与枚举一一对应。数组长度由初始化项推导,漏加 / 多加一行都会被 static_assert 拦下。
	inline constexpr const char *kFreezeCapActionNames[] = {
		"none",
		"unfreeze",
		"destroy",
	};
	static_assert(std::size(kFreezeCapActionNames) == kFreezeCapActionCount,
				  "kFreezeCapActionNames 必须与 FreezeCapAction 一一对应");

	constexpr const char *FreezeCapActionName(FreezeCapAction action)
	{
		const auto index = static_cast<std::size_t>(action);
		return index < kFreezeCapActionCount ? kFreezeCapActionNames[index] : "?";
	}

	// "handoff 标记已发出"之后的统一收口(ConcludeHandoffAfterMarkSent)是从哪个点进来的。
	// 底层类型 uint8_t 数的是收口点个数(现 5 个)。
	enum class MarkSentSite : uint8_t
	{
		kFreezeCap,       // 冻结满 kFreezeCap 仍无结论(EnforceTravelFreezeCaps)
		kDispatchWindow,  // SET 的 OK 应答晚于 kDispatchWindow 才到,不再发 EnterScene(RequestTravelEnterScene)
		kNoGateSession,   // SET 已 OK,却发现没有 gate 会话可带(RequestTravelEnterScene)
		kNoSceneManager,  // SET 已 OK,却发现没有可达的 scene_manager(RequestTravelEnterScene)
		kReceiptAnomaly,  // 取证读到本次标记原文的回滚回执,其余交叉校验却不成立(ResolveTravelOutcome,GO-2 判定表 B6)

		kCount
	};

	constexpr std::size_t kMarkSentSiteCount = static_cast<std::size_t>(MarkSentSite::kCount);

	// 这些名字同时用作 DestroyDeposedPlayer 的 reasonTag(它的收尾日志以 "[<reasonTag>]" 开头)和
	// [ZoneTravel][MarkSentDestroy] 日志里的 site=。runbook 按原文 grep,改名要同步 runbook。
	inline constexpr const char *kMarkSentSiteNames[] = {
		"travel_freeze_cap",
		"travel_dispatch_window",
		"travel_no_gate_session",
		"travel_no_scene_manager",
		"travel_receipt_anomaly",
	};
	static_assert(std::size(kMarkSentSiteNames) == kMarkSentSiteCount,
				  "kMarkSentSiteNames 必须与 MarkSentSite 一一对应");

	constexpr const char *MarkSentSiteName(MarkSentSite site)
	{
		const auto index = static_cast<std::size_t>(site);
		return index < kMarkSentSiteCount ? kMarkSentSiteNames[index] : "?";
	}

	// handoff 标记的 SET 是否"已发出"(骨架第一刀的判据)。只看 requestedAtMs:
	// BeginTravelHandoff 在 command() **之前**写 requestedAtMs;命令发不出去、或 SET 收到 ERROR 应答(确定没生效)
	// 时,都在同一个调用 / 同一个回调里同步 AbortTravelHandoff 摘掉组件。扫描与发送点都跑在 scene 逻辑线程上,
	// 不会与回调交错,所以只要观察到非 0,SET 一定已进了 hiredis 缓冲、已执行,或结果未知。
	// 不看 markEpoch:以后哪条路径漏写了 markEpoch,也只会落进"销毁"一侧,不会落进"解冻"一侧(fail-closed)。
	constexpr bool IsHandoffMarkSent(uint64_t requestedAtMs)
	{
		return requestedAtMs != 0;
	}

	// 冻结了 frozenFor 之后该怎么处置。恰好等于上限即到期。标记已发出后永不解冻。
	constexpr FreezeCapAction DecideFreezeCap(Clock::duration frozenFor, bool markSent)
	{
		if (frozenFor < kFreezeCap)
		{
			return FreezeCapAction::kNone;
		}
		return markSent ? FreezeCapAction::kDestroy : FreezeCapAction::kUnfreeze;
	}

	// 晚发窗口是否还开着:开着才允许发出 handoff SET / EnterScene。
	constexpr bool IsDispatchWindowOpen(Clock::duration frozenFor)
	{
		return frozenFor < kDispatchWindow;
	}

	// 离 dueAt 还剩多久;已过则为 0,不返回负值。
	constexpr Clock::duration RemainingUntil(Clock::time_point dueAt, Clock::time_point now)
	{
		return now < dueAt ? dueAt - now : Clock::duration::zero();
	}

	// muduo 定时器醒来时(now)离单调截止时刻 dueAt 还差多少才算"提前醒了、要按剩余时间重挂"。
	// 提前量不超过 kEarlyFireTolerance(含恰好等于)就直接执行。
	constexpr bool ShouldRearmEarlyFire(Clock::time_point dueAt, Clock::time_point now)
	{
		return RemainingUntil(dueAt, now) > kEarlyFireTolerance;
	}
} // namespace travel_freeze_cap
