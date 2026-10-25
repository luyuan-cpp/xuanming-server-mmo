// 玩家持久收件箱 scene 消费状态机(PlayerInboxSystem)的系统级用例
// (docs/design/player-inbox-service.md §7.3–§7.9、§8;评审 L3,覆盖 M1 / L1 / L2 / L10 / M4 / H1 的回归)。
// 离开路径另有一个用例经 PlayerLifecycleSystem::FinishExitOrAwaitDbTaskAck 走真正的退出收尾接缝(评审 F1)。
//
// 纯规则(分类、记账、durable 判定、判定顺序 PlanItem)已在 player_inbox_rules_test.cpp 逐条钉住;这里验的是
// 把它们串成状态机之后对外可观察的行为:发出了哪些 Fetch / Ack、请求里带了什么、账本与货币被改成什么样、
// 哪些回包被丢弃。运行时组件(PlayerInboxRuntimeComp)是公开头文件里的结构,其阶段只作辅助断言。
//
// 确定性(AGENTS §11.4):
//   * 时钟、存盘、传输、配置全部经 PlayerInboxSystem 的测试注入点替换:不读真实墙钟、不发 gRPC。
//     tlsRedis 在本进程里从不建连,所以存盘之后不会发 PING 快路径,"已落盘"只由用例显式调 CompleteFakeSave
//     模拟落盘回调更新 PlayerLastPersistedSnapshotComp。
//   * 退避带随机抖动(tlsRandom)。推进时钟一律推到抖动上界之外(PastBackoff / kBeyondAnyBackoffMs),
//     需要"还没到点"时推到抖动下界之内,不赌某个具体的抖动值。
//   * 关联号是节点级单调计数器(thread_local),跨用例持续递增:用例只从捕获的请求里取,不假设具体数值。
//   * 每个用例在 TearDown 里销毁自己建的实体、复原全部注入点、本节点信息与会话映射,不留 tls 残留,可 --gtest_shuffle。
//     抓日志的用例用 ScopedLogCapture,出作用域即还原 muduo 的输出与级别。

#include <gtest/gtest.h>

#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <memory>
#include <optional>
#include <string>
#include <vector>

#include <google/protobuf/unknown_field_set.h>

#include "engine/core/type_define/player_guid_comp.h"
#include "core/network/grpc_client_call.h" // kGrpcStatusUnavailable
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "muduo/base/Logging.h"             // Logger::setOutput:抓 metric= 对账行(评审 M4 的缺口)
#include "network/node_utils.h"             // InstallLocalNodeInfo / GetNodeInfo
#include "node/system/grpc_call_deadline.h" // 在途期限从 PlayerInboxNodeService 的 deadline 派生
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_inbox_runtime_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/player_exit_intent.h" // PlayerExitIntentComp:退出收尾接缝(评审 F1)
#include "player/system/player_inbox_registry.h" // SelfCheck:调 Initialize 之前先确认它不会停用本线程的消费
#include "player/system/player_inbox_rules.h"
#include "player/system/player_inbox_system.h"
#include "player/system/role_admission_system.h"
#include "battle/system/player_battle.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "player/system/player_lifecycle.h" // FinishExitOrAwaitDbTaskAck(评审 F1)
#include "proto/common/base/common.pb.h"    // NodeInfo
#include "proto/common/base/node.pb.h"      // PlayerInboxNodeService
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"         // UnregisterPlayer
#include "proto/common/component/player_inbox_ledger_comp.pb.h"
#include "proto/common/component/player_network_comp.pb.h" // PlayerSessionSnapshotComp
#include "proto/player_inbox/player_inbox.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/player_inbox_error_tip.pb.h"
#include "thread_context/ecs_context.h"
#include "type_alias/player_session_type_alias.h" // SessionMap

namespace
{
	namespace rules = player_inbox_rules;

	// 与其他用例文件的玩家号错开(asset_op 950100001、gm_sanction 950200001)。
	constexpr uint64_t kTestPlayerId = 950300001;
	constexpr uint64_t kEpoch = 7;
	constexpr uint32_t kSceneNodeId = 4242;
	constexpr uint64_t kStartMs = 10'000'000; // 非 0:运行时组件里 0 表示"未设置"

	constexpr uint32_t kGmDomain = player_inbox::PLAYER_INBOX_DOMAIN_SCENE_GM_SANCTION;
	constexpr uint32_t kBlockType = player_inbox::PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN;
	constexpr uint32_t kUnblockType = player_inbox::PLAYER_INBOX_MSG_GM_UNBLOCK_CURRENCY_GAIN;
	constexpr uint32_t kDebtType = player_inbox::PLAYER_INBOX_MSG_GM_ATTACH_CURRENCY_DEBT;
	// 本二进制不认识的类型(新类型先于 scene 升级被启用的版本错位)。proto3 枚举是开放的,线上可以出现。
	constexpr uint32_t kUnknownType = 99;

	constexpr uint32_t kGold = static_cast<uint32_t>(kCurrencyGold);
	constexpr uint32_t kCancelled = player_inbox::PLAYER_INBOX_VOID_CANCELLED;
	constexpr uint32_t kQuarantined = player_inbox::PLAYER_INBOX_VOID_QUARANTINED;

	// 任何 1s→60s 退避(含 ±20% 抖动,封顶 60s)推进这么久都已到期。
	constexpr uint64_t kBeyondAnyBackoffMs = rules::kBackoffMaxMs + 1;

	// 第 failStreak 次连续失败后的退避,取抖动上界再多 1ms:推进这么久一定到期。
	uint64_t PastBackoff(uint32_t failStreak)
	{
		return rules::BackoffMs(failStreak, rules::kJitterPermilleMax) + 1;
	}

	// 在途请求的兜底期限 = PlayerInboxNodeService 的 gRPC deadline + 实现里的余量(5s,匿名命名空间的常量)。
	// 这里多推 60s,只要求那份余量不超过 60s,不复制它的具体值。
	uint64_t PastInflightTimeout()
	{
		return static_cast<uint64_t>(grpc_call_deadline::Get(common::base::PlayerInboxNodeService).count()) +
			   rules::kBackoffMaxMs;
	}

	// ── 注入的假依赖 ─────────────────────────────────────────────────────────

	uint64_t g_nowMs = kStartMs;
	uint64_t FakeNowMs()
	{
		return g_nowMs;
	}
	void Advance(uint64_t ms)
	{
		g_nowMs += ms;
	}

	// 假存盘:只计数并回报"确实压了一次写",**不**更新快照 —— persistFn 返回 true 不等于已落盘,
	// "已落盘"只看 PlayerLastPersistedSnapshotComp,由用例显式 CompleteFakeSave。
	int g_persistCalls = 0;
	bool FakePersist(entt::entity /*player*/)
	{
		++g_persistCalls;
		return true;
	}

	// 假传输:attempts 计全部调用,sent 只收回报"发出去了"的那些(等同于到了线上的请求)。
	// 本夹具只驱动GM域；新增域仍由注册表自检与各自完整系统用例验证，不伪造它们的回包。
	uint32_t g_transportDomain = kGmDomain;
	bool g_fetchSendOk = true;
	bool g_ackSendOk = true;
	int g_fetchAttempts = 0;
	int g_ackAttempts = 0;
	std::vector<player_inbox::FetchPlayerInboxRequest> g_sentFetches;
	std::vector<player_inbox::AckPlayerInboxRequest> g_sentAcks;

	bool FakeSendFetch(const player_inbox::FetchPlayerInboxRequest &request)
	{
		if (static_cast<uint32_t>(request.domain()) != g_transportDomain) return false;
		++g_fetchAttempts;
		if (!g_fetchSendOk)
		{
			return false;
		}
		g_sentFetches.push_back(request);
		return true;
	}

	bool FakeSendAck(const player_inbox::AckPlayerInboxRequest &request)
	{
		if (static_cast<uint32_t>(request.domain()) != g_transportDomain) return false;
		++g_ackAttempts;
		if (!g_ackSendOk)
		{
			return false;
		}
		g_sentAcks.push_back(request);
		return true;
	}

	// 本节点信息:Ack / Fetch 的 scene_node_id 取自它(评审 L10)。用例期间换上自己的,结束时还原。
	NodeInfo &TestNodeInfo()
	{
		static NodeInfo info;
		return info;
	}
	// 用例开始前已安装的本节点信息;nullptr = 当时是只读默认实例。命名空间级变量,不是类成员。
	const NodeInfo *g_nodeInfoBeforeTest = nullptr;

	// ── 条目与回包 ───────────────────────────────────────────────────────────

	std::string GainPayload(uint32_t currencyType)
	{
		player_inbox::GmCurrencyGainPayload payload;
		payload.set_currency_type(currencyType);
		return payload.SerializeAsString();
	}

	// 字段 1 的 varint 标签后面没有值:任何消息都解不出来(认识的类型、载荷坏 → 确定性 REJECTED)。
	const std::string kTruncatedPayload("\x08", 1);

	player_inbox::PlayerInboxItem MakeItem(uint64_t seq, uint32_t msgType, uint64_t requestId, const std::string &payload,
										   player_inbox::PlayerInboxDirective directive = player_inbox::PLAYER_INBOX_DIRECTIVE_NONE,
										   uint32_t voidReason = 0)
	{
		player_inbox::PlayerInboxItem item;
		item.set_seq(seq);
		item.set_msg_type(static_cast<player_inbox::PlayerInboxMessageType>(msgType));
		item.set_request_id(requestId);
		item.set_payload(payload);
		item.set_directive(directive);
		item.set_void_reason(static_cast<player_inbox::PlayerInboxVoidReason>(voidReason));
		item.set_delivery_count(1);
		item.set_created_ms(1);
		return item;
	}

	player_inbox::PlayerInboxItem BlockGold(uint64_t seq, uint64_t requestId)
	{
		return MakeItem(seq, kBlockType, requestId, GainPayload(kGold));
	}

	// 服务端给已投递的行挂上作废指令后再投(设计 §7.9):协议上载荷为空。
	player_inbox::PlayerInboxItem Voided(uint64_t seq, uint32_t msgType, uint64_t requestId, uint32_t voidReason)
	{
		return MakeItem(seq, msgType, requestId, std::string{}, player_inbox::PLAYER_INBOX_DIRECTIVE_VOID, voidReason);
	}

	player_inbox::FetchPlayerInboxResponse FetchReplyFor(
		const player_inbox::FetchPlayerInboxRequest &request, const std::vector<player_inbox::PlayerInboxItem> &items,
		bool hasMore = false, player_inbox::PlayerInboxFenceVerdict fence = player_inbox::PLAYER_INBOX_FENCE_MATCH)
	{
		player_inbox::FetchPlayerInboxResponse response;
		response.set_player_id(request.player_id());
		response.set_domain(request.domain());
		response.set_correlation_id(request.correlation_id());
		response.set_fence(fence);
		for (const auto &item : items)
		{
			*response.add_items() = item;
		}
		response.set_has_more(hasMore);
		return response;
	}

	player_inbox::FetchPlayerInboxResponse EmptyFetchReplyFor(const player_inbox::FetchPlayerInboxRequest &request)
	{
		return FetchReplyFor(request, {}, false, player_inbox::PLAYER_INBOX_FENCE_NO_STREAM);
	}

	player_inbox::PlayerInboxItemState TerminalStateFor(player_inbox::PlayerInboxOutcome outcome)
	{
		switch (outcome)
		{
		case player_inbox::PLAYER_INBOX_OUTCOME_APPLIED:
			return player_inbox::PLAYER_INBOX_ITEM_APPLIED;
		case player_inbox::PLAYER_INBOX_OUTCOME_REJECTED:
			return player_inbox::PLAYER_INBOX_ITEM_REJECTED;
		case player_inbox::PLAYER_INBOX_OUTCOME_SKIPPED:
			return player_inbox::PLAYER_INBOX_ITEM_VOIDED;
		case player_inbox::PLAYER_INBOX_OUTCOME_UNKNOWN:
			return player_inbox::PLAYER_INBOX_ITEM_SETTLED_UNKNOWN;
		default:
			return player_inbox::PLAYER_INBOX_ITEM_UNSPECIFIED;
		}
	}

	// 围栏放行、逐条 FINALIZED 的成功回包(results 与 decisions 按序一一对应)。
	player_inbox::AckPlayerInboxResponse AckReplyFor(const player_inbox::AckPlayerInboxRequest &request)
	{
		player_inbox::AckPlayerInboxResponse response;
		response.set_player_id(request.player_id());
		response.set_domain(request.domain());
		response.set_correlation_id(request.correlation_id());
		response.set_fence(player_inbox::PLAYER_INBOX_FENCE_MATCH);
		for (const auto &decision : request.decisions())
		{
			auto *result = response.add_results();
			result->set_seq(decision.seq());
			result->set_result(player_inbox::PLAYER_INBOX_ACK_FINALIZED);
			result->set_state(TerminalStateFor(decision.outcome()));
		}
		return response;
	}

	player_inbox::AckPlayerInboxResponse StaleAckReplyFor(const player_inbox::AckPlayerInboxRequest &request)
	{
		player_inbox::AckPlayerInboxResponse response;
		response.set_player_id(request.player_id());
		response.set_domain(request.domain());
		response.set_correlation_id(request.correlation_id());
		response.set_fence(player_inbox::PLAYER_INBOX_FENCE_STALE); // 围栏拒绝时 results 为空
		return response;
	}

	player_inbox::AckPlayerInboxResponse TipAckReplyFor(const player_inbox::AckPlayerInboxRequest &request, uint32_t tip)
	{
		player_inbox::AckPlayerInboxResponse response;
		response.set_player_id(request.player_id());
		response.set_domain(request.domain());
		response.set_correlation_id(request.correlation_id());
		response.mutable_error_message()->set_id(tip);
		return response;
	}

	PlayerInboxDecision MakeDecision(uint64_t seq, player_inbox::PlayerInboxOutcome outcome, uint64_t requestId,
									 uint32_t msgType, uint32_t reason = 0)
	{
		PlayerInboxDecision decision;
		decision.set_seq(seq);
		decision.set_outcome(static_cast<uint32_t>(outcome));
		decision.set_reason(reason);
		decision.set_request_id(requestId);
		decision.set_msg_type(msgType);
		return decision;
	}

	void ExpectDecision(const player_inbox::PlayerInboxDecisionReport &report, uint64_t seq,
						player_inbox::PlayerInboxOutcome outcome, uint32_t reason, uint64_t requestId)
	{
		EXPECT_EQ(seq, report.seq());
		EXPECT_EQ(outcome, report.outcome()) << "seq=" << seq;
		EXPECT_EQ(reason, report.reason()) << "seq=" << seq;
		EXPECT_EQ(requestId, report.request_id()) << "seq=" << seq << ":Ack 必须带 request_id 供服务端核对(IB-9)";
	}

	// 全部到了线上的 Ack 里报告过的 seq,按发出顺序拼起来。
	std::vector<uint64_t> AllAckedSeqs()
	{
		std::vector<uint64_t> seqs;
		for (const auto &ack : g_sentAcks)
		{
			for (const auto &decision : ack.decisions())
			{
				seqs.push_back(decision.seq());
			}
		}
		return seqs;
	}

	// SQL回执由夹具独立提交；Redis快照更新本身不产生SQL证据。
	std::optional<RoleAdmissionSystem::SqlAck> g_sqlAck;
	std::vector<std::function<void(std::optional<RoleAdmissionSystem::SqlAck>)>> g_sqlReplies;
	bool g_deferSqlReply = false;
	bool g_sqlSendOk = true;
	bool g_sqlCompletionSendStatus = true;
	uint64_t g_snapshotSeq = 0;
	int g_sqlReads = 0;

	std::string SqlVersion(uint64_t epoch, uint64_t seq)
	{
		const auto e = std::to_string(epoch), q = std::to_string(seq);
		return std::string(20 - e.size(), '0') + e + ":" + std::string(20 - q.size(), '0') + q;
	}

	void CompleteFakeSqlSave(entt::entity player)
	{
		const auto &snap = tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player);
		const auto version = SqlVersion(snap.committed_owner_epoch, snap.committed_save_seq);
		g_sqlAck = RoleAdmissionSystem::SqlAck{std::to_string(snap.committed_owner_epoch), version, version, version};
	}

	// ── 实体侧 ───────────────────────────────────────────────────────────────

	const PlayerInboxDomainState *GmState(entt::entity player)
	{
		if (!tlsEcs.actorRegistry.valid(player))
		{
			return nullptr;
		}
		const auto *runtime = tlsEcs.actorRegistry.try_get<PlayerInboxRuntimeComp>(player);
		if (runtime == nullptr)
		{
			return nullptr;
		}
		for (const auto &state : runtime->domains)
		{
			if (state.domain == kGmDomain)
			{
				return &state;
			}
		}
		return nullptr;
	}

	std::size_t RuntimeCompCount()
	{
		std::size_t count = 0;
		for (const auto entity : tlsEcs.actorRegistry.view<PlayerInboxRuntimeComp>())
		{
			(void)entity;
			++count;
		}
		return count;
	}

	const PlayerInboxDomainCursor *GmCursor(entt::entity player)
	{
		return rules::FindCursor(tlsEcs.actorRegistry.get<PlayerInboxLedgerComp>(player), kGmDomain);
	}

	uint64_t DecidedThrough(entt::entity player)
	{
		const auto *cursor = GmCursor(player);
		return cursor != nullptr ? cursor->decided_through_seq() : 0;
	}

	bool GoldBlocked(entt::entity player)
	{
		return CurrencySystem::IsCurrencyBlocked(player, kCurrencyGold);
	}

	// 模拟一次落盘回调到达:把当前账本原样拷进"最后一次成功写入 Redis 的玩家数据"。
	// 测试在这里扮演 HandlePlayerAsyncSaved,所以由它建快照组件(生产里也只有存盘回调建它)。
	void CompleteFakeSave(entt::entity player, bool sqlCommitted = true)
	{
		PlayerAllData persisted;
		const auto epoch = tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch;
		const auto seq = ++g_snapshotSeq;
		auto *first = persisted.mutable_player_database_data();
		first->set_player_id(kTestPlayerId);
		if (const auto* currency = tlsEcs.actorRegistry.try_get<CurrencyComp>(player)) first->mutable_currency()->CopyFrom(*currency);
		if (const auto* attributes = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(player))
			first->mutable_derived_attributes_component()->CopyFrom(*attributes);
		auto *second = persisted.mutable_player_database_1_data();
		second->set_player_id(kTestPlayerId);
		persisted.set_last_save_seq(seq);
		persisted.mutable_player_database_data()->mutable_inbox_ledger()->CopyFrom(
			tlsEcs.actorRegistry.get<PlayerInboxLedgerComp>(player));
		const auto source = std::make_shared<const PlayerAllData>(persisted);
		const auto committed = RoleAdmissionSystem::CaptureSnapshotCommit(player, source, epoch, seq);
		EXPECT_TRUE(static_cast<bool>(committed));
		if (committed) committed(); // 与正式成功hook一致，先记原source，再发布saved回调。
		tlsEcs.actorRegistry.get_or_emplace<PlayerLastPersistedSnapshotComp>(player).Replace(*source);
		if (sqlCommitted) CompleteFakeSqlSave(player); // 普通用例显式完成两份证据；反例仅写Redis。
	}

	// ── 日志抓取 ─────────────────────────────────────────────────────────────
	// 已投递、还没定结局的行没有结局可确认,场景侧唯一的产出就是一行 metric= 对账日志(评审 M4 的缺口),只能从日志验。

	std::vector<std::string> g_capturedLogLines;

	void CaptureLogLine(const char *msg, int len)
	{
		g_capturedLogLines.emplace_back(msg, static_cast<std::size_t>(len));
		std::fwrite(msg, 1, static_cast<std::size_t>(len), stdout);
	}

	void WriteLogToStdout(const char *msg, int len)
	{
		std::fwrite(msg, 1, static_cast<std::size_t>(len), stdout);
	}

	// 作用域内把 muduo 的输出接到 g_capturedLogLines(照抄到 stdout),级别至少放到 INFO;析构时改回 stdout、还原级别。
	class ScopedLogCapture
	{
	public:
		ScopedLogCapture() : previousLevel_(muduo::Logger::logLevel())
		{
			g_capturedLogLines.clear();
			if (previousLevel_ > muduo::Logger::INFO)
			{
				muduo::Logger::setLogLevel(muduo::Logger::INFO);
			}
			muduo::Logger::setOutput(&CaptureLogLine);
		}
		~ScopedLogCapture()
		{
			muduo::Logger::setOutput(&WriteLogToStdout);
			muduo::Logger::setLogLevel(previousLevel_);
		}
		ScopedLogCapture(const ScopedLogCapture &) = delete;
		ScopedLogCapture &operator=(const ScopedLogCapture &) = delete;

	private:
		muduo::Logger::LogLevel previousLevel_;
	};

	// 抓到的行里同时含有全部片段的条数。
	std::size_t CapturedLinesWith(const std::vector<std::string> &fragments)
	{
		std::size_t count = 0;
		for (const auto &line : g_capturedLogLines)
		{
			bool matchesAll = true;
			for (const auto &fragment : fragments)
			{
				if (line.find(fragment) == std::string::npos)
				{
					matchesAll = false;
					break;
				}
			}
			if (matchesAll)
			{
				++count;
			}
		}
		return count;
	}

	// 消费开关关着时的节点级 WARN(评审 H1)。键名 metric=inbox_consumer_disabled 是告警固定查询的契约;
	// 级别字段是定宽的 "WARN  "(muduo Logging.cc 的 LogLevelName)。
	std::size_t ConsumerDisabledWarnLines()
	{
		return CapturedLinesWith({"WARN  ", "[PlayerInbox]", "metric=inbox_consumer_disabled"});
	}

	// ── 退出收尾接缝(评审 F1)──────────────────────────────────────────────

	// 退出用例的会话号(SessionId 是 uint32)。
	constexpr SessionId kExitSession = 950301;

	// 假 Ack 被调用那一刻实体的样子。OnPlayerLeaving 依赖实体有效、PlayerOwnerEpochComp 还在,所以
	// FinishExitAfterPersist 里那一行必须排在 DestroyPlayer 之前;这里在调用当场取证,不靠事后推断。
	struct AckMomentProbe
	{
		bool entityLive{false};
		bool exitMarkersPresent{false};
		uint64_t epochOnEntity{0};
	};
	AckMomentProbe g_ackMoment;

	bool ProbingSendAck(const player_inbox::AckPlayerInboxRequest &request)
	{
		const entt::entity player = tlsEcs.GetPlayer(request.player_id());
		g_ackMoment.entityLive = player != entt::null && tlsEcs.actorRegistry.valid(player);
		if (g_ackMoment.entityLive)
		{
			g_ackMoment.exitMarkersPresent = tlsEcs.actorRegistry.all_of<UnregisterPlayer, PlayerExitIntentComp>(player);
			const auto *ownerEpoch = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player);
			g_ackMoment.epochOnEntity = ownerEpoch != nullptr ? ownerEpoch->epoch : 0;
		}
		return FakeSendAck(request);
	}

	class PlayerInboxSystemTest : public ::testing::Test
	{
	protected:
		void SetUp() override
		{
			g_nowMs = kStartMs;
			g_persistCalls = 0;
			g_transportDomain = kGmDomain;
			g_fetchSendOk = true;
			g_ackSendOk = true;
			g_fetchAttempts = 0;
			g_ackAttempts = 0;
			g_sentFetches.clear();
			g_sentAcks.clear();
			g_ackMoment = AckMomentProbe{};
			g_snapshotSeq = 0; g_sqlReads = 0; g_sqlAck.reset(); g_sqlReplies.clear(); g_deferSqlReply = false; g_sqlSendOk = true; g_sqlCompletionSendStatus = true;

			PlayerInboxSystem::SetNowFnForTest(&FakeNowMs);
			PlayerInboxSystem::SetPersistFnForTest(&FakePersist);
			PlayerInboxSystem::SetTransportForTest(&FakeSendFetch, &FakeSendAck);
			// 消费打开、在线有界兜底关闭:兜底带随机抖动,开着会让检查时刻不确定。
			PlayerInboxSystem::SetConsumerConfigForTest(true, true, 0);

			const NodeInfo &installed = GetNodeInfo();
			g_nodeInfoBeforeTest = (&installed == &NodeInfo::default_instance()) ? nullptr : &installed;
			TestNodeInfo().set_node_id(kSceneNodeId);
			InstallLocalNodeInfo(&TestNodeInfo());

			player_ = CreateLoadedPlayer(kEpoch, PlayerInboxLedgerComp{});
		}

		void TearDown() override
		{
			for (const auto player : created_)
			{
				DestroyPlayer(player);
			}
			created_.clear();
			g_sqlReplies.clear();
			admission_.reset();
			tlsEcs.playerList.erase(kTestPlayerId);
			SessionMap().erase(kExitSession); // BindSession 的映射;退出收尾正常时已被 RemovePlayerSession 擦掉

			InstallLocalNodeInfo(g_nodeInfoBeforeTest);
			g_nodeInfoBeforeTest = nullptr;
			PlayerInboxSystem::SetConsumerConfigForTest(false, false, 0);
			PlayerInboxSystem::SetTransportForTest(nullptr, nullptr);
			PlayerInboxSystem::SetPersistFnForTest(nullptr);
			PlayerInboxSystem::SetNowFnForTest(nullptr);
			g_sentFetches.clear();
			g_sentAcks.clear();
		}

		// 手搭一个"加载已完成"的玩家:持久组件(货币两份、收件箱账本)只由加载创建,这里就是测试的创建点;
		// 运行时组件一律不预挂 —— "没有运行时组件"本身就是新落地判据的一半(设计 §7.3)。
		entt::entity CreateLoadedPlayer(uint64_t epoch, const PlayerInboxLedgerComp &ledger)
		{
			// 重登必须由新的已确认入场原件建立，旧作用域回包不能借用新实体。
			admission_.reset();
			RoleAdmissionSystem::Dependencies dependencies;
			dependencies.now = [] { return RoleAdmissionSystem::Clock::time_point(std::chrono::milliseconds(g_nowMs)); };
			dependencies.localNode = [] { return RoleAdmissionSystem::LocalNode{"inbox-sql-test-scene", "4242", 1}; };
			dependencies.freeze = [](Guid) {}; // 本fixture只验撤资格/只读确认，不驱动真实存盘。
			dependencies.hasPendingLoad = [](Guid) { return false; };
			common::base::RoleAdmissionIdentity identity;
			identity.set_player_id(kTestPlayerId); identity.set_generation(++admissionGeneration_); identity.set_owner_revision(1);
			identity.set_owner_epoch(epoch); identity.set_scene_instance_id("inbox-sql-test-scene");
			dependencies.confirmedAdmissions.push_back(identity);
			dependencies.readSqlAck = [](Guid id, auto completed) {
				EXPECT_EQ(kTestPlayerId, id);
				++g_sqlReads;
				if (!g_sqlSendOk) return false;
				if (g_deferSqlReply) g_sqlReplies.push_back(std::move(completed));
				else completed(g_sqlAck);
				return g_deferSqlReply || g_sqlCompletionSendStatus;
			};
			admission_ = std::make_unique<RoleAdmissionSystem::ScopedDependencies>(std::move(dependencies));
			const entt::entity player = tlsEcs.actorRegistry.create();
			tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, kTestPlayerId);
			tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
			tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
			tlsEcs.actorRegistry.emplace<PlayerInboxLedgerComp>(player, ledger);
			// 收件箱只在存盘带归属围栏的实体上消费(player_mutability::HasFencedOwnership)。
			tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = epoch;
			tlsEcs.actorRegistry.emplace<PlayerRoleAdmissionComp>(player, admissionGeneration_, std::uint64_t{1});
			tlsEcs.playerList[kTestPlayerId] = player;
			created_.push_back(player);
			return player;
		}

		// 照 DestroyPlayer 的收尾顺序:先摘索引再销毁实体,之后按 player_id 找不到它。
		static void DestroyPlayer(entt::entity player)
		{
			if (const auto it = tlsEcs.playerList.find(kTestPlayerId); it != tlsEcs.playerList.end() && it->second == player)
			{
				tlsEcs.playerList.erase(it);
			}
			if (tlsEcs.actorRegistry.valid(player))
			{
				tlsEcs.actorRegistry.destroy(player);
			}
		}

		// 登录时建好的会话(会话快照 + SessionMap 映射):退出收尾的 RemovePlayerSession 照生产路径擦掉它。
		static void BindSession(entt::entity player)
		{
			auto &session = tlsEcs.actorRegistry.emplace<PlayerSessionSnapshotComp>(player);
			session.set_gate_session_id(kExitSession);
			session.set_player_id(kTestPlayerId);
			SessionMap().insert_or_assign(kExitSession, kTestPlayerId);
		}

		// 退出存盘在途的形状:HandleExitGameNode 紧跟 UnregisterPlayer 挂上退出意图(成对约定见 player_exit_intent.h)。
		// 挂上之后 ClassifyPlayerMutability 判 kExiting,前置闸不再放行;确认不受前置闸限制。
		static void MarkExiting(entt::entity player)
		{
			tlsEcs.actorRegistry.emplace<UnregisterPlayer>(player);
			PlayerExitIntentComp intent;
			intent.sessionAtExit = kExitSession;
			intent.cause = ExitCause::kClientDisconnect;
			tlsEcs.actorRegistry.emplace<PlayerExitIntentComp>(player, intent);
		}

		// 落地前预置账本(模拟从存档加载出来的状态),并校验它本身合法。
		void SeedLedger(uint64_t decidedThrough, const std::vector<PlayerInboxDecision> &recent)
		{
			auto &ledger = tlsEcs.actorRegistry.get<PlayerInboxLedgerComp>(player_);
			auto *cursor = ledger.add_cursors();
			cursor->set_domain(kGmDomain);
			cursor->set_decided_through_seq(decidedThrough);
			for (const auto &decision : recent)
			{
				*cursor->add_recent() = decision;
			}
			ASSERT_EQ(std::string{}, rules::ValidateLedger(ledger));
		}

		// 落地 → tick → 恰好发出一次 Fetch。
		static void LandAndFetch(entt::entity player, player_inbox::FetchPlayerInboxRequest &fetch)
		{
			const std::size_t before = g_sentFetches.size();
			PlayerInboxSystem::OnPlayerLanded(player);
			PlayerInboxSystem::Tick();
			ASSERT_EQ(before + 1, g_sentFetches.size()) << "新落地要无条件拉一次(设计 §7.3)";
			fetch = g_sentFetches.back();
		}

		// 模拟落盘 → tick → 恰好发出一次 Ack。
		static void SaveAndAck(entt::entity player, player_inbox::AckPlayerInboxRequest &ack)
		{
			const std::size_t before = g_sentAcks.size();
			CompleteFakeSave(player);
			PlayerInboxSystem::Tick();
			ASSERT_EQ(before + 1, g_sentAcks.size()) << "结局出现在落盘快照里之后,下一个 tick 应确认";
			ack = g_sentAcks.back();
		}

		std::vector<entt::entity> created_;
		std::unique_ptr<RoleAdmissionSystem::ScopedDependencies> admission_;
		uint64_t admissionGeneration_{0};
		entt::entity player_{entt::null};
	};
} // namespace

// ── 同一快照双SQL确认(IB-3)──────────────────────────────────────────────

TEST_F(PlayerInboxSystemTest, RedisSuccessDoesNotAcknowledgeWithoutBothSqlRows)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(fetch.player_id(), fetch.domain(), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	CompleteFakeSave(player_, false);
	PlayerInboxSystem::Tick();
	EXPECT_TRUE(g_sentAcks.empty());
	CompleteFakeSqlSave(player_);
	const auto correct = *g_sqlAck;
	for (int damage = 0; damage != 4; ++damage)
	{
		g_sqlAck = correct;
		if (damage == 0) g_sqlAck->playerDatabase1 = SqlVersion(kEpoch, 0);
		if (damage == 1) g_sqlAck->playerDatabase = SqlVersion(kEpoch, 0);
		if (damage == 2) g_sqlAck->ownerEpoch = std::to_string(kEpoch + 1);
		if (damage == 3) g_sqlAck->pending = SqlVersion(kEpoch, g_snapshotSeq + 1);
		PlayerInboxSystem::Tick();
		EXPECT_TRUE(g_sentAcks.empty()) << damage;
	}
	g_sqlAck = correct;
	PlayerInboxSystem::Tick();
	ASSERT_EQ(1u, g_sentAcks.size());
	EXPECT_EQ(1, g_sentAcks.back().decisions_size());
}

TEST_F(PlayerInboxSystemTest, SqlReplyForReplacedSnapshotCannotAcknowledgeNewRound)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(fetch.player_id(), fetch.domain(), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	CompleteFakeSave(player_);
	g_deferSqlReply = true;
	PlayerInboxSystem::Tick();
	ASSERT_EQ(1u, g_sqlReplies.size());
	const auto old = g_sqlAck;
	CompleteFakeSave(player_); // 同账本，不同q，旧SQL回包不能给新快照确认。
	g_sqlReplies.front()(old);
	EXPECT_TRUE(g_sentAcks.empty());
	g_deferSqlReply = false;
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, g_sentAcks.size());
}

TEST_F(PlayerInboxSystemTest, SqlReplyCannotCrossAdmissionOrEntityLifetime)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(fetch.player_id(), fetch.domain(), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	CompleteFakeSave(player_);
	g_deferSqlReply = true;
	PlayerInboxSystem::Tick();
	ASSERT_EQ(1u, g_sqlReplies.size());
	tlsEcs.actorRegistry.get<PlayerRoleAdmissionComp>(player_).generation = 2;
	g_sqlReplies.front()(g_sqlAck);
	EXPECT_TRUE(g_sentAcks.empty());
	// 另一次读在原N下发出，然后同playerId重建实体。旧回调不能给新实体挂状态或发Ack。
	tlsEcs.actorRegistry.get<PlayerRoleAdmissionComp>(player_).generation = 1;
	PlayerInboxSystem::Tick();
	ASSERT_EQ(2u, g_sqlReplies.size());
	DestroyPlayer(player_);
	player_ = CreateLoadedPlayer(kEpoch, PlayerInboxLedgerComp{});
	g_sqlReplies.back()(g_sqlAck);
	EXPECT_TRUE(g_sentAcks.empty());
	EXPECT_EQ(nullptr, GmState(player_));
}

TEST_F(PlayerInboxSystemTest, LeavingWhileSqlPendingRetainsUnacknowledgedDecision)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(fetch.player_id(), fetch.domain(), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	CompleteFakeSave(player_, false);
	PlayerInboxSystem::OnPlayerLeaving(player_);
	EXPECT_TRUE(g_sentAcks.empty());
	EXPECT_EQ(10u, DecidedThrough(player_));
	EXPECT_EQ(1, g_persistCalls);
}

TEST_F(PlayerInboxSystemTest, SealBeforeSqlReplyRejectsAckWithoutBlockingWriterDrain)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(fetch.player_id(), fetch.domain(), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	CompleteFakeSave(player_);
	g_deferSqlReply = true;
	PlayerInboxSystem::Tick();
	ASSERT_EQ(1u, g_sqlReplies.size());
	common::base::RoleTradeSeal seal;
	seal.set_player_id(kTestPlayerId); seal.set_owner_revision(1); seal.set_through_generation(1);
	seal.set_listing_id(42); seal.set_scene_instance_id("inbox-sql-test-scene");
	seal.set_scene_node_id("4242"); seal.set_zone_id(1);
	ASSERT_TRUE(RoleAdmissionSystem::Seal(seal));
	EXPECT_FALSE(RoleAdmissionSystem::ConfirmedIdentity(kTestPlayerId).has_value());
	// 第一条只读回包保持悬挂，也不计入写者callbacks；drain能读取自己的SQL floor。
	RoleAdmissionSystem::ObserveTradeSaveQueued(kTestPlayerId, kEpoch, g_snapshotSeq);
	DestroyPlayer(player_);
	Advance(1001);
	RoleAdmissionSystem::Tick();
	EXPECT_EQ(2u, g_sqlReplies.size()) << "排空应能读取其原SQL floor，不被Inbox只读probe阻塞";
	g_sqlReplies.front()(g_sqlAck);
	EXPECT_TRUE(g_sentAcks.empty());
}

TEST_F(PlayerInboxSystemTest, SnapshotSqlConfirmationRejectsMismatchedRowsBeforeReading)
{
	CompleteFakeSave(player_);
	PlayerAllData wrong = *tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player_).snapshot;
	wrong.mutable_player_database_1_data()->set_fence_save_seq(g_snapshotSeq + 1);
	tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player_).Replace(wrong);
	int callbacks = 0;
	EXPECT_FALSE(RoleAdmissionSystem::ConfirmSnapshotSqlCommit(player_,
		*tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player_).snapshot,
		[&callbacks](bool) { ++callbacks; }));
	EXPECT_EQ(0, g_sqlReads);
	EXPECT_EQ(0, callbacks);
}

TEST_F(PlayerInboxSystemTest, SnapshotSqlSendFailureAndDuplicateRepliesPreserveSingleCompletion)
{
	CompleteFakeSave(player_);
	const auto &snap = *tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player_).snapshot;
	int callbacks = 0;
	g_sqlSendOk = false;
	EXPECT_FALSE(RoleAdmissionSystem::ConfirmSnapshotSqlCommit(player_, snap, [&callbacks](bool) { ++callbacks; }));
	EXPECT_EQ(0, callbacks);
	g_sqlSendOk = true;
	EXPECT_TRUE(RoleAdmissionSystem::ConfirmSnapshotSqlCommit(player_, snap, [&callbacks](bool committed) {
		EXPECT_TRUE(committed); ++callbacks;
	}));
	EXPECT_EQ(1, callbacks); // 同步回调也只完成一次。
	g_sqlCompletionSendStatus = false; // 错误适配器已同步完成却报未发送，公开接口仍须返回已接管。
	EXPECT_TRUE(RoleAdmissionSystem::ConfirmSnapshotSqlCommit(player_, snap, [&callbacks](bool committed) {
		EXPECT_TRUE(committed); ++callbacks;
	}));
	EXPECT_EQ(2, callbacks);
	g_sqlCompletionSendStatus = true;
	g_deferSqlReply = true;
	EXPECT_TRUE(RoleAdmissionSystem::ConfirmSnapshotSqlCommit(player_, snap, [&callbacks](bool committed) {
		EXPECT_TRUE(committed); ++callbacks;
	}));
	ASSERT_EQ(1u, g_sqlReplies.size());
	g_sqlReplies.front()(g_sqlAck);
	g_sqlReplies.front()(g_sqlAck);
	EXPECT_EQ(3, callbacks);
}

TEST_F(PlayerInboxSystemTest, CommitProofRequiresOriginalHookBeforeSamePayloadSavedCallback)
{
	CompleteFakeSave(player_);
	PlayerAllData fresh = *tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player_).snapshot;
	fresh.set_last_save_seq(++g_snapshotSeq);
	const auto source = std::make_shared<const PlayerAllData>(fresh);
	const auto hook = RoleAdmissionSystem::CaptureSnapshotCommit(player_, source, kEpoch, g_snapshotSeq);
	ASSERT_TRUE(static_cast<bool>(hook));
	auto &snap = tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player_);
	snap.Replace(*source); // 未成功的提交不调用hook：有字节也没有提交资格。
	EXPECT_EQ(0u, snap.committed_owner_epoch);
	hook();
	const auto sameBytesDifferentSource = std::make_shared<const PlayerAllData>(fresh);
	snap.Replace(*sameBytesDifferentSource);
	EXPECT_EQ(0u, snap.committed_save_seq) << "原hook不能被另一份相同字节载荷借用";
	hook();
	snap.Replace(*source);
	EXPECT_EQ(kEpoch, snap.committed_owner_epoch);
	EXPECT_EQ(g_snapshotSeq, snap.committed_save_seq);
	EXPECT_EQ(0u, snap.snapshot->last_save_seq());
	EXPECT_EQ(0u, snap.snapshot->player_database_data().fence_owner_epoch());
	EXPECT_EQ(0u, snap.snapshot->player_database_1_data().fence_save_seq());
}

TEST_F(PlayerInboxSystemTest, OldCommitHookCannotAuthorizeNewAdmissionOrRecreatedEntity)
{
	CompleteFakeSave(player_);
	PlayerAllData fresh = *tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player_).snapshot;
	fresh.set_last_save_seq(++g_snapshotSeq);
	const auto source = std::make_shared<const PlayerAllData>(fresh);
	const auto hook = RoleAdmissionSystem::CaptureSnapshotCommit(player_, source, kEpoch, g_snapshotSeq);
	ASSERT_TRUE(static_cast<bool>(hook));
	tlsEcs.actorRegistry.get<PlayerRoleAdmissionComp>(player_).generation = 2;
	hook();
	auto &old = tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player_);
	old.Replace(*source);
	EXPECT_EQ(0u, old.committed_save_seq);
	DestroyPlayer(player_);
	player_ = CreateLoadedPlayer(kEpoch, PlayerInboxLedgerComp{});
	hook();
	auto &current = tlsEcs.actorRegistry.get_or_emplace<PlayerLastPersistedSnapshotComp>(player_);
	current.Replace(*source);
	EXPECT_EQ(0u, current.committed_save_seq);
	EXPECT_EQ(0, g_sqlReads);
}

TEST_F(PlayerInboxSystemTest, SameBusinessBytesAfterOwnerAdvanceRequireFreshCommitAndBothSqlAcks)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(fetch.player_id(), fetch.domain(), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	CompleteFakeSave(player_);
	const auto oldBytes = tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player_).snapshot->SerializeAsString();
	tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player_).epoch = kEpoch + 2; // B5保留原N与identity。
	Advance(rules::kResaveIntervalMs);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(2, g_persistCalls) << "不能仅凭旧E的同业务字节跳过新提交";
	EXPECT_EQ(0, g_sqlReads);
	EXPECT_TRUE(g_sentAcks.empty());
	CompleteFakeSave(player_, false); // 真正的新E/q提交，SQL仍是旧E。
	EXPECT_EQ(oldBytes, tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player_).snapshot->SerializeAsString());
	PlayerInboxSystem::Tick();
	EXPECT_TRUE(g_sentAcks.empty());
	CompleteFakeSqlSave(player_);
	PlayerInboxSystem::Tick();
	ASSERT_EQ(1u, g_sentAcks.size());
	EXPECT_EQ(kEpoch + 2, g_sentAcks.back().owner_epoch());
}

TEST_F(PlayerInboxSystemTest, SnapshotSqlTimeoutDoesNotQueueCommandsOrAcceptLateReply)
{
	CompleteFakeSave(player_);
	const auto &snapshot = *tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player_).snapshot;
	g_deferSqlReply = true;
	int callbacks = 0;
	ASSERT_TRUE(RoleAdmissionSystem::ConfirmSnapshotSqlCommit(player_, snapshot, [&callbacks](bool committed) {
		EXPECT_FALSE(committed); ++callbacks;
	}));
	ASSERT_EQ(1u, g_sqlReplies.size());
	Advance(9999);
	RoleAdmissionSystem::Tick();
	EXPECT_EQ(0, callbacks);
	Advance(1);
	RoleAdmissionSystem::Tick();
	EXPECT_EQ(1, callbacks);
	for (int tick = 0; tick != 3; ++tick) {
		EXPECT_FALSE(RoleAdmissionSystem::ConfirmSnapshotSqlCommit(player_, snapshot, [&callbacks](bool) { ++callbacks; }));
		Advance(11000); RoleAdmissionSystem::Tick();
	}
	EXPECT_EQ(1, g_sqlReads) << "physical回包未到，不能每次超时又排一条Redis命令";
	g_sqlReplies.front()(g_sqlAck);
	EXPECT_EQ(1, callbacks) << "迟到不能重复交付原结果";
	g_deferSqlReply = false;
	EXPECT_TRUE(RoleAdmissionSystem::ConfirmSnapshotSqlCommit(player_, snapshot, [&callbacks](bool committed) {
		EXPECT_TRUE(committed); ++callbacks;
	}));
	EXPECT_EQ(2, callbacks);
	EXPECT_EQ(2, g_sqlReads);
}

TEST_F(PlayerInboxSystemTest, SnapshotSqlReplyAfterRoleStateDestructionHasNoCallback)
{
	CompleteFakeSave(player_);
	g_deferSqlReply = true;
	int callbacks = 0;
	ASSERT_TRUE(RoleAdmissionSystem::ConfirmSnapshotSqlCommit(player_,
		*tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player_).snapshot,
		[&callbacks](bool) { ++callbacks; }));
	ASSERT_EQ(1u, g_sqlReplies.size());
	admission_.reset();
	g_sqlReplies.front()(g_sqlAck);
	EXPECT_EQ(0, callbacks);
}

// ── 落盘之后才确认(IB-3,设计 §7.7)─────────────────────────────────────────

TEST_F(PlayerInboxSystemTest, AckWaitsUntilTheLastPersistedSnapshotHoldsTheOutcome)
{
	// 落地前已有一份旧快照(账本还是空的):"有快照"不等于"本批结局已落盘"。
	CompleteFakeSave(player_);
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));

	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	EXPECT_TRUE(GoldBlocked(player_)) << "效果与记账同一帧写入";
	EXPECT_EQ(10u, DecidedThrough(player_));
	EXPECT_EQ(1, g_persistCalls) << "本批有新记账:请求一次存盘";
	EXPECT_TRUE(g_sentAcks.empty()) << "persistFn 回报压了写,但快照没更新:不能确认";

	for (int tick = 1; tick <= 5; ++tick)
	{
		Advance(1000);
		PlayerInboxSystem::Tick();
	}
	EXPECT_TRUE(g_sentAcks.empty()) << "快照一直没追上:一直不确认";
	EXPECT_EQ(0, g_ackAttempts) << "连发送都不该尝试";
	EXPECT_EQ(2, g_persistCalls) << "等落盘期间每 5 秒补请求一次存盘(设计 §7.7)";

	player_inbox::AckPlayerInboxRequest ack;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, ack));
	ASSERT_EQ(1, ack.decisions_size());
	ExpectDecision(ack.decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 0, 1001);

	PlayerInboxSystem::OnAckReply(
		ack.player_id(), static_cast<uint32_t>(ack.domain()), ack.correlation_id(),
		AckReplyFor(ack));
	const auto *state = GmState(player_);
	ASSERT_NE(nullptr, state);
	EXPECT_EQ(PlayerInboxPhase::kIdle, state->phase);
	EXPECT_TRUE(state->ackQueue.empty());

	// 一轮结束、没有 has_more 也没有新触发:不再拉,也不再确认。
	Advance(kBeyondAnyBackoffMs);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, g_sentFetches.size());
	EXPECT_EQ(1u, g_sentAcks.size());
}

// 评审 L10:Ack 也带 scene_node_id(只用于服务端日志定位发送方),与 Fetch 填的是同一个本节点 id。
TEST_F(PlayerInboxSystemTest, FetchAndAckRequestsCarryOwnerEpochAndSceneNodeId)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	EXPECT_EQ(kTestPlayerId, fetch.player_id());
	EXPECT_EQ(player_inbox::PLAYER_INBOX_DOMAIN_SCENE_GM_SANCTION, fetch.domain());
	EXPECT_EQ(kEpoch, fetch.owner_epoch());
	EXPECT_NE(0u, fetch.correlation_id()) << "0 保留给\"没有在途\"";
	EXPECT_EQ(rules::kFetchBatchMax, fetch.max_items());
	EXPECT_EQ(0u, fetch.consumer_decided_through());
	EXPECT_EQ(kSceneNodeId, fetch.scene_node_id());

	// 认识的类型、载荷解不出来:确定性 REJECTED,reason 是 tip id。
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {MakeItem(10, kDebtType, 1001, kTruncatedPayload)}));

	player_inbox::AckPlayerInboxRequest ack;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, ack));
	EXPECT_EQ(kTestPlayerId, ack.player_id());
	EXPECT_EQ(player_inbox::PLAYER_INBOX_DOMAIN_SCENE_GM_SANCTION, ack.domain());
	EXPECT_EQ(kEpoch, ack.owner_epoch());
	EXPECT_NE(0u, ack.correlation_id());
	EXPECT_NE(fetch.correlation_id(), ack.correlation_id()) << "每个请求取一个新的关联号";
	EXPECT_EQ(kSceneNodeId, ack.scene_node_id()) << "评审 L10";
	ASSERT_EQ(1, ack.decisions_size());
	ExpectDecision(ack.decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_REJECTED,
				   static_cast<uint32_t>(kPlayerInboxPayloadInvalid), 1001);
}

// ── 单飞与关联号(设计 §7.4、§7.11)─────────────────────────────────────────

TEST_F(PlayerInboxSystemTest, TriggersDuringAnInflightRoundOnlyRequestARecheck)
{
	player_inbox::FetchPlayerInboxRequest first;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, first));

	// Fetch 在途:提醒、归属变化后的落地都只置 recheck。
	PlayerInboxSystem::OnNotifyEvent(kTestPlayerId, kGmDomain);
	PlayerInboxSystem::OnNotifyEvent(kTestPlayerId, kGmDomain);
	tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player_).epoch = kEpoch + 1;
	PlayerInboxSystem::OnPlayerLanded(player_);
	for (int tick = 1; tick <= 5; ++tick) // 5 秒远小于在途期限
	{
		Advance(1000);
		PlayerInboxSystem::Tick();
	}
	EXPECT_EQ(1u, g_sentFetches.size()) << "同一 (玩家, 域) 同一时刻至多一个在途请求";
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_TRUE(GmState(player_)->recheckRequested);

	// 这一轮结束后补拉恰好一次(触发不累加)。
	PlayerInboxSystem::OnFetchReply(
		first.player_id(), static_cast<uint32_t>(first.domain()), first.correlation_id(),
		EmptyFetchReplyFor(first));
	PlayerInboxSystem::Tick();
	ASSERT_EQ(2u, g_sentFetches.size());
	const auto second = g_sentFetches.back();
	EXPECT_EQ(kEpoch + 1, second.owner_epoch());

	// Ack 在途同理:提醒只置 recheck,确认完才拉下一轮(上一批全部确认之前不拉下一批)。
	PlayerInboxSystem::OnFetchReply(
		second.player_id(), static_cast<uint32_t>(second.domain()), second.correlation_id(),
		FetchReplyFor(second, {BlockGold(10, 1001)}));
	player_inbox::AckPlayerInboxRequest ack;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, ack));
	PlayerInboxSystem::OnNotifyEvent(kTestPlayerId, kGmDomain);
	Advance(1000);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(2u, g_sentFetches.size()) << "Ack 在途期间不拉";

	PlayerInboxSystem::OnAckReply(
		ack.player_id(), static_cast<uint32_t>(ack.domain()), ack.correlation_id(),
		AckReplyFor(ack));
	PlayerInboxSystem::Tick();
	ASSERT_EQ(3u, g_sentFetches.size()) << "确认完成后按 recheck 再拉一次";

	PlayerInboxSystem::OnFetchReply(
		g_sentFetches.back().player_id(), static_cast<uint32_t>(g_sentFetches.back().domain()), g_sentFetches.back().correlation_id(),
		EmptyFetchReplyFor(g_sentFetches.back()));
	Advance(kBeyondAnyBackoffMs);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(3u, g_sentFetches.size()) << "没有新触发就不再拉";
}

// 没有 READY 的收件箱节点:发不出去就按 kNoNode 退避,不进在途;到点再试(设计 §7.5 最后一条)。
TEST_F(PlayerInboxSystemTest, FetchWithNoReachableNodeBacksOffAndRetries)
{
	g_fetchSendOk = false;
	PlayerInboxSystem::OnPlayerLanded(player_);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1, g_fetchAttempts);
	EXPECT_TRUE(g_sentFetches.empty());
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_EQ(PlayerInboxPhase::kIdle, GmState(player_)->phase) << "没发出去就不算在途";
	EXPECT_EQ(PlayerInboxDeferReason::kNoNode, GmState(player_)->deferReason);

	PlayerInboxSystem::Tick();
	EXPECT_EQ(1, g_fetchAttempts) << "退避未到不重试";

	g_fetchSendOk = true;
	Advance(PastBackoff(1));
	PlayerInboxSystem::Tick();
	EXPECT_EQ(2, g_fetchAttempts);
	ASSERT_EQ(1u, g_sentFetches.size());
	EXPECT_EQ(PlayerInboxPhase::kFetching, GmState(player_)->phase);
}

TEST_F(PlayerInboxSystemTest, LateFetchReplyAfterTimeoutIsDiscardedByCorrelation)
{
	player_inbox::FetchPlayerInboxRequest timedOut;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, timedOut));

	// 完成通知迟迟不来:按传输失败收场、退避。
	Advance(PastInflightTimeout());
	PlayerInboxSystem::Tick();
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_EQ(PlayerInboxPhase::kIdle, GmState(player_)->phase);
	EXPECT_EQ(0u, GmState(player_)->correlationId);

	// 迟到的回包:已经不在途 → 丢弃,不执行、不记账。
	PlayerInboxSystem::OnFetchReply(
		timedOut.player_id(), static_cast<uint32_t>(timedOut.domain()), timedOut.correlation_id(),
		FetchReplyFor(timedOut, {BlockGold(10, 1001)}));
	EXPECT_FALSE(GoldBlocked(player_));
	EXPECT_EQ(nullptr, GmCursor(player_));
	EXPECT_TRUE(GmState(player_)->pendingItems.empty());

	Advance(PastBackoff(1));
	PlayerInboxSystem::Tick();
	ASSERT_EQ(2u, g_sentFetches.size()) << "退避到点后重拉";
	const auto retry = g_sentFetches.back();
	ASSERT_NE(timedOut.correlation_id(), retry.correlation_id());

	// 又在途了,但关联号对不上:迟到的旧回包与旧失败通知照样丢弃,不顶掉新的在途请求。
	PlayerInboxSystem::OnFetchReply(
		timedOut.player_id(), static_cast<uint32_t>(timedOut.domain()), timedOut.correlation_id(),
		FetchReplyFor(timedOut, {BlockGold(10, 1001)}));
	PlayerInboxSystem::OnFetchFailed(timedOut.player_id(), static_cast<uint32_t>(timedOut.domain()), timedOut.correlation_id(), kGrpcStatusUnavailable, "late failure");
	EXPECT_FALSE(GoldBlocked(player_));
	EXPECT_EQ(PlayerInboxPhase::kFetching, GmState(player_)->phase);
	EXPECT_EQ(retry.correlation_id(), GmState(player_)->correlationId);

	PlayerInboxSystem::OnFetchReply(
		retry.player_id(), static_cast<uint32_t>(retry.domain()), retry.correlation_id(),
		FetchReplyFor(retry, {BlockGold(10, 1001)}));
	EXPECT_TRUE(GoldBlocked(player_));
	EXPECT_EQ(10u, DecidedThrough(player_));
}

TEST_F(PlayerInboxSystemTest, LateAckReplyAfterTimeoutIsDiscardedAndTheSameDecisionsAreResent)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	player_inbox::AckPlayerInboxRequest timedOut;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, timedOut));

	Advance(PastInflightTimeout());
	PlayerInboxSystem::Tick();
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_EQ(PlayerInboxPhase::kAwaitDurable, GmState(player_)->phase) << "Ack 超时:保留 ackQueue,退避重发";
	EXPECT_EQ(1u, GmState(player_)->ackQueue.size());

	PlayerInboxSystem::OnAckReply(
		timedOut.player_id(), static_cast<uint32_t>(timedOut.domain()), timedOut.correlation_id(),
		AckReplyFor(timedOut));
	EXPECT_EQ(1u, GmState(player_)->ackQueue.size()) << "不在途时迟到的回包不能清队列";

	Advance(PastBackoff(1));
	PlayerInboxSystem::Tick();
	ASSERT_EQ(2u, g_sentAcks.size());
	const auto resent = g_sentAcks.back();
	EXPECT_NE(timedOut.correlation_id(), resent.correlation_id());
	ASSERT_EQ(1, resent.decisions_size());
	ExpectDecision(resent.decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 0, 1001);

	// 关联号对不上的旧回包 / 旧失败通知:丢弃,新的 Ack 仍在途。
	PlayerInboxSystem::OnAckReply(
		timedOut.player_id(), static_cast<uint32_t>(timedOut.domain()), timedOut.correlation_id(),
		AckReplyFor(timedOut));
	PlayerInboxSystem::OnAckFailed(timedOut.player_id(), static_cast<uint32_t>(timedOut.domain()), timedOut.correlation_id(), kGrpcStatusUnavailable, "late failure");
	EXPECT_EQ(PlayerInboxPhase::kAcking, GmState(player_)->phase);
	EXPECT_EQ(resent.correlation_id(), GmState(player_)->correlationId);

	PlayerInboxSystem::OnAckReply(
		resent.player_id(), static_cast<uint32_t>(resent.domain()), resent.correlation_id(),
		AckReplyFor(resent));
	EXPECT_EQ(PlayerInboxPhase::kIdle, GmState(player_)->phase);
	EXPECT_TRUE(GmState(player_)->ackQueue.empty());
}

// 围栏判 STALE:新 owner 已推进围栏,它加载的存档里已有这些结局,会自己补确认(设计 §7.7、§7.10)。
TEST_F(PlayerInboxSystemTest, StaleAckClearsTheQueueAndBacksOffBeforeRefetching)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	player_inbox::AckPlayerInboxRequest ack;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, ack));

	PlayerInboxSystem::OnAckReply(
		ack.player_id(), static_cast<uint32_t>(ack.domain()), ack.correlation_id(),
		StaleAckReplyFor(ack));
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_TRUE(GmState(player_)->ackQueue.empty()) << "STALE:清队列,不重发";
	EXPECT_EQ(PlayerInboxPhase::kIdle, GmState(player_)->phase);
	EXPECT_EQ(10u, DecidedThrough(player_)) << "账本不动:结局已落盘,由新 owner 补确认";

	// 固定退避 60 秒(kStaleOwnerBackoffMs,不加抖动):到点之前什么都不发。
	Advance(rules::kStaleOwnerBackoffMs - 1);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, g_sentAcks.size());
	EXPECT_EQ(1u, g_sentFetches.size());

	Advance(1);
	PlayerInboxSystem::Tick();
	ASSERT_EQ(2u, g_sentFetches.size()) << "退避到点后重拉一次,看自己是否仍是 owner";
	EXPECT_EQ(10u, g_sentFetches.back().consumer_decided_through());
	EXPECT_EQ(1u, g_sentAcks.size());
}

// ── 判定顺序(评审 M1)───────────────────────────────────────────────────────

TEST_F(PlayerInboxSystemTest, VoidedNewEntryIsSkippedWithoutRunningTheExecutor)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));

	// 故意带上合法载荷:若误调执行器,金币会被封锁。
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {MakeItem(10, kBlockType, 1001, GainPayload(kGold), player_inbox::PLAYER_INBOX_DIRECTIVE_VOID, kCancelled)}));
	EXPECT_FALSE(GoldBlocked(player_)) << "作废的新条目不查执行器、不解码载荷(设计 §7.9)";

	const auto *cursor = GmCursor(player_);
	ASSERT_NE(nullptr, cursor);
	EXPECT_EQ(10u, cursor->decided_through_seq());
	ASSERT_EQ(1, cursor->recent_size());
	EXPECT_EQ(static_cast<uint32_t>(player_inbox::PLAYER_INBOX_OUTCOME_SKIPPED), cursor->recent(0).outcome());
	EXPECT_EQ(kCancelled, cursor->recent(0).reason()) << "SKIPPED 的 reason 取 void_reason";
	EXPECT_EQ(1, g_persistCalls) << "SKIPPED 照常记账、存盘";

	player_inbox::AckPlayerInboxRequest ack;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, ack));
	ASSERT_EQ(1, ack.decisions_size());
	ExpectDecision(ack.decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_SKIPPED, kCancelled, 1001);
}

// M1 的直接回归:旧顺序先查执行器,本二进制不认识的类型即使被运维作废,每次登录仍会先撞上"未知类型"停用该域。
TEST_F(PlayerInboxSystemTest, VoidedEntryOfAnUnknownTypeIsSkippedAndTheBatchContinues)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));

	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {Voided(10, kUnknownType, 1001, kQuarantined), BlockGold(11, 1101)}));
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_FALSE(GmState(player_)->halted) << "作废指令能解开未知类型的停用";
	EXPECT_TRUE(GoldBlocked(player_)) << "后面的条目照常执行";
	EXPECT_EQ(11u, DecidedThrough(player_));

	player_inbox::AckPlayerInboxRequest ack;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, ack));
	ASSERT_EQ(2, ack.decisions_size());
	ExpectDecision(ack.decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_SKIPPED, kQuarantined, 1001);
	ExpectDecision(ack.decisions(1), 11, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 0, 1101);
}

TEST_F(PlayerInboxSystemTest, UnknownTypeWithoutVoidHaltsAfterConfirmingTheDecidedPrefix)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));

	// 第 12 条是解封:若越过停用点继续执行,金币会被解封。
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001), MakeItem(11, kUnknownType, 1101, "opaque"),
				MakeItem(12, kUnblockType, 1201, GainPayload(kGold))}));
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_TRUE(GmState(player_)->halted);
	EXPECT_EQ(PlayerInboxHaltReason::kUnknownType, GmState(player_)->haltReason);
	EXPECT_EQ(10u, DecidedThrough(player_)) << "停在未知类型这一条:不记账,绝不当成 REJECTED";
	EXPECT_TRUE(GoldBlocked(player_)) << "停用点之后的条目不执行";
	EXPECT_TRUE(GmState(player_)->pendingItems.empty());

	// 已定的前缀照常落盘后确认(设计 §7.6);停用只停拉取与应用,不停确认。
	player_inbox::AckPlayerInboxRequest ack;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, ack));
	ASSERT_EQ(1, ack.decisions_size());
	ExpectDecision(ack.decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 0, 1001);
	PlayerInboxSystem::OnAckReply(
		ack.player_id(), static_cast<uint32_t>(ack.domain()), ack.correlation_id(),
		AckReplyFor(ack));

	// 本实体不再为这个域拉取:提醒、到期、甚至归属变化后的落地都不行,直到实体下次在本节点新建。
	PlayerInboxSystem::OnNotifyEvent(kTestPlayerId, kGmDomain);
	tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player_).epoch = kEpoch + 1;
	PlayerInboxSystem::OnPlayerLanded(player_);
	Advance(kBeyondAnyBackoffMs);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, g_sentFetches.size());
	EXPECT_EQ(1u, g_sentAcks.size());
}

// 评审 SM-1:已停用的域确认已定前缀时 Ack 回 STALE,只清队列、回 Idle;不再安排「60 秒后重试」,
// 也不打 reason=stale_owner 的 inbox_deferred,免得把永久停用伪装成暂时延后。
TEST_F(PlayerInboxSystemTest, StaleAckOnAHaltedDomainSchedulesNoRetry)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001), MakeItem(11, kUnknownType, 1101, "opaque")}));
	ASSERT_NE(nullptr, GmState(player_));
	ASSERT_TRUE(GmState(player_)->halted);
	const auto deferBefore = GmState(player_)->deferReason;
	player_inbox::AckPlayerInboxRequest ack;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, ack));

	ScopedLogCapture capture;
	PlayerInboxSystem::OnAckReply(
		ack.player_id(), static_cast<uint32_t>(ack.domain()), ack.correlation_id(),
		StaleAckReplyFor(ack));
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_TRUE(GmState(player_)->ackQueue.empty());
	EXPECT_EQ(PlayerInboxPhase::kIdle, GmState(player_)->phase);
	EXPECT_EQ(0u, GmState(player_)->nextCheckMs) << "停用的域没有下一次检查";
	EXPECT_EQ(deferBefore, GmState(player_)->deferReason) << "延后原因不改写成 stale_owner";
	EXPECT_EQ(0u, CapturedLinesWith({"metric=inbox_deferred", "reason=stale_owner"}));

	Advance(kBeyondAnyBackoffMs);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, g_sentFetches.size()) << "停用后不再拉取";
	EXPECT_EQ(1u, g_sentAcks.size());
}

// 回放 / UNKNOWN 只报告账本里已有的事实:不需要执行器,与指令、与本二进制认不认识这个类型都无关(IB-4、IB-9)。
TEST_F(PlayerInboxSystemTest, ReplayAndUnknownNeedNoExecutorEvenForUnknownTypesOrVoid)
{
	// 存档里的账本:水位 11,环里只剩 10、11(更早的已被挤出)。11 是旧版本二进制认识、本二进制不认识的类型。
	ASSERT_NO_FATAL_FAILURE(SeedLedger(
		11, {MakeDecision(10, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 1001, kBlockType),
			 MakeDecision(11, player_inbox::PLAYER_INBOX_OUTCOME_REJECTED, 1101, kUnknownType,
						  static_cast<uint32_t>(kPlayerInboxPayloadInvalid))}));
	CompleteFakeSave(player_); // 这份账本就是从存档加载的:已落盘
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	EXPECT_EQ(11u, fetch.consumer_decided_through());

	// 重复投递:5 已出环(UNKNOWN),10 被补挂了 VOID(必须回放 APPLIED,不能报 SKIPPED),11 是未知类型(回放)。
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {MakeItem(5, kUnknownType, 501, "opaque"), Voided(10, kBlockType, 1001, kCancelled),
				MakeItem(11, kUnknownType, 1101, "opaque")}));
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_FALSE(GmState(player_)->halted) << "未知类型的重复投递只回放,不停用";
	EXPECT_FALSE(GoldBlocked(player_)) << "回放不重做效果";
	EXPECT_EQ(0, g_persistCalls) << "没有新记账就不请求存盘";
	EXPECT_EQ(11u, DecidedThrough(player_));
	EXPECT_EQ(2, GmCursor(player_)->recent_size()) << "UNKNOWN 不入环";

	// 全部已在存档里:当场确认,不等 tick。
	ASSERT_EQ(1u, g_sentAcks.size());
	const auto &ack = g_sentAcks.back();
	ASSERT_EQ(3, ack.decisions_size());
	ExpectDecision(ack.decisions(0), 5, player_inbox::PLAYER_INBOX_OUTCOME_UNKNOWN, 0, 501);
	ExpectDecision(ack.decisions(1), 10, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 0, 1001);
	ExpectDecision(ack.decisions(2), 11, player_inbox::PLAYER_INBOX_OUTCOME_REJECTED,
				   static_cast<uint32_t>(kPlayerInboxPayloadInvalid), 1101);
}

// MISMATCH 时什么都不确认,前面已回放的也不确认;场景侧的 VOID 解不开它(解卡靠服务端 VoidItem 的 FORCE_MISMATCH)。
TEST_F(PlayerInboxSystemTest, MismatchConfirmsNothingEvenWhenTheEntryIsVoided)
{
	ASSERT_NO_FATAL_FAILURE(SeedLedger(10, {MakeDecision(9, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 901, kBlockType),
											MakeDecision(10, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 1001, kBlockType)}));
	CompleteFakeSave(player_);
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));

	// 9 与账本一致(可回放),10 的 request_id 与账本里的同一 seq 不同(IB-9)。
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(9, 901), Voided(10, kBlockType, 2002, kCancelled)}));
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_TRUE(GmState(player_)->halted);
	EXPECT_EQ(PlayerInboxHaltReason::kReplayMismatch, GmState(player_)->haltReason);
	EXPECT_TRUE(GmState(player_)->ackQueue.empty());
	EXPECT_TRUE(g_sentAcks.empty()) << "MISMATCH:一条都不确认,连已回放的 9 也不确认";

	const auto *cursor = GmCursor(player_);
	ASSERT_NE(nullptr, cursor);
	EXPECT_EQ(10u, cursor->decided_through_seq()) << "账本不改写";
	ASSERT_EQ(2, cursor->recent_size());
	EXPECT_EQ(901u, cursor->recent(0).request_id());
	EXPECT_EQ(1001u, cursor->recent(1).request_id());

	PlayerInboxSystem::OnNotifyEvent(kTestPlayerId, kGmDomain);
	Advance(kBeyondAnyBackoffMs);
	PlayerInboxSystem::Tick();
	Advance(kBeyondAnyBackoffMs);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(0, g_ackAttempts);
	EXPECT_EQ(1u, g_sentFetches.size());
}

// ── 应用前的前置闸与执行器 DEFER(评审 L1)─────────────────────────────────

// L1 的直接回归:旧实现闸不过就把条目留在本地续做,等闸期间服务端挂上的 VOID 看不见,解冻后照旧副本执行。
TEST_F(PlayerInboxSystemTest, ApplyGateClosedOnReplyDropsTheBatchAndTheRefetchSeesTheVoid)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));

	// Fetch 在途时玩家进入归属交接冻结;回包到达时应用前的那一次闸判不过。
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player_);
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_EQ(PlayerInboxPhase::kIdle, GmState(player_)->phase) << "丢掉本地副本、回 Idle";
	EXPECT_TRUE(GmState(player_)->pendingItems.empty());
	EXPECT_FALSE(GoldBlocked(player_));
	EXPECT_EQ(nullptr, GmCursor(player_)) << "不记账";
	EXPECT_EQ(0, g_persistCalls);

	// 冻结期间拉取前的闸也不过:不拉。
	Advance(kBeyondAnyBackoffMs);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, g_sentFetches.size());

	// 解冻后重拉;这期间服务端给这一行挂上了 VOID。
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player_);
	Advance(kBeyondAnyBackoffMs);
	PlayerInboxSystem::Tick();
	ASSERT_EQ(2u, g_sentFetches.size()) << "下次检查重新拉取(只多 +1 次 delivery_count)";
	const auto refetch = g_sentFetches.back();
	EXPECT_EQ(0u, refetch.consumer_decided_through());

	PlayerInboxSystem::OnFetchReply(
		refetch.player_id(), static_cast<uint32_t>(refetch.domain()), refetch.correlation_id(),
		FetchReplyFor(refetch, {Voided(10, kBlockType, 1001, kCancelled)}));
	EXPECT_FALSE(GoldBlocked(player_)) << "已作废的处置不能按旧副本执行";

	player_inbox::AckPlayerInboxRequest ack;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, ack));
	ASSERT_EQ(1, ack.decisions_size());
	ExpectDecision(ack.decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_SKIPPED, kCancelled, 1001);
}

// 执行器 DEFER(闸已过、条目已开始处理)仍在本地续做,不重新 Fetch;已定前缀等整批做完一起确认。
TEST_F(PlayerInboxSystemTest, ExecutorDeferResumesLocallyWithoutRefetching)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));

	// 持久货币组件缺失(加载不变量被破坏)是 v1 执行器唯一的 DEFER 来源;作废条目不经执行器,照常记 SKIPPED。
	tlsEcs.actorRegistry.remove<PlayerCurrencyComp>(player_);
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {Voided(10, kBlockType, 1001, kCancelled), BlockGold(11, 1101)}));
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_EQ(PlayerInboxPhase::kApplying, GmState(player_)->phase);
	ASSERT_EQ(1u, GmState(player_)->pendingItems.size()) << "已处理的前缀从本地缓存摘掉";
	EXPECT_EQ(11u, GmState(player_)->pendingItems.front().seq());
	EXPECT_EQ(10u, DecidedThrough(player_));
	EXPECT_EQ(1, g_persistCalls) << "前缀已记账:请求存盘";
	EXPECT_TRUE(g_sentAcks.empty()) << "整批做完才确认";

	// 测试专用:把组件按加载路径补回来,模拟不变量恢复(生产里组件只由加载创建)。
	tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player_);
	Advance(PastBackoff(1));
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, g_sentFetches.size()) << "DEFER 本地续做,不重新 Fetch";
	EXPECT_TRUE(GoldBlocked(player_));
	EXPECT_EQ(11u, DecidedThrough(player_));

	player_inbox::AckPlayerInboxRequest ack;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, ack));
	ASSERT_EQ(2, ack.decisions_size());
	ExpectDecision(ack.decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_SKIPPED, kCancelled, 1001);
	ExpectDecision(ack.decisions(1), 11, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 0, 1101);
}

// 执行器报版本错位(混版本保护:同币种欠款带本版本不认识的字段)与未知类型同口径:停用本域,已定前缀照常确认,
// 这一条不记账、不确认。**不按本地副本续做**:错位在会话内消失(欠款被还清删除)之后,本地旧副本也不得被执行 ——
// 服务端可能已给这一行挂了 VOID,只有重拉才看得到(设计 §7.5 评审 L1;mixed-version-persist-field-protection.md §4.4)。
TEST_F(PlayerInboxSystemTest, ExecutorVersionSkewHaltsAfterConfirmingThePrefixAndNeverResumesLocally)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));

	// 新版本 scene 写下的金币欠款:带本版本不认识的字段。
	CurrencyComp persisted;
	auto *entry = persisted.add_debts();
	entry->set_currency_type(kGold);
	entry->set_owed(500);
	entry->mutable_unknown_fields()->AddVarint(9999, 1);
	auto &currency = tlsEcs.actorRegistry.get<PlayerCurrencyComp>(player_);
	currency.LoadFromProto(persisted);

	player_inbox::GmCurrencyDebtPayload debt;
	debt.set_currency_type(kGold);
	debt.set_owe_amount(100);
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001), MakeItem(11, kDebtType, 1101, debt.SerializeAsString()),
				MakeItem(12, kUnblockType, 1201, GainPayload(kGold))}));
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_TRUE(GmState(player_)->halted);
	EXPECT_EQ(PlayerInboxHaltReason::kVersionSkew, GmState(player_)->haltReason);
	EXPECT_EQ(10u, DecidedThrough(player_)) << "停在版本错位这一条:不记账,绝不当成 REJECTED";
	EXPECT_TRUE(GoldBlocked(player_)) << "停用点之后的条目不执行";
	EXPECT_TRUE(GmState(player_)->pendingItems.empty()) << "不留本地副本";
	EXPECT_EQ(500u, currency.debts.at(kGold).owed) << "没有并进带新字段的欠款";

	// 已定的前缀照常落盘后确认;版本错位这一条不在确认里。
	player_inbox::AckPlayerInboxRequest ack;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, ack));
	ASSERT_EQ(1, ack.decisions_size());
	ExpectDecision(ack.decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 0, 1001);
	PlayerInboxSystem::OnAckReply(
		ack.player_id(), static_cast<uint32_t>(ack.domain()), ack.correlation_id(), AckReplyFor(ack));

	// 会话内错位消失:那笔欠款被还清删除。本地旧副本已丢弃,之后的 tick 不得把这笔欠款挂上,也不再拉取。
	currency.debts.erase(kGold);
	PlayerInboxSystem::OnNotifyEvent(kTestPlayerId, kGmDomain);
	Advance(kBeyondAnyBackoffMs);
	PlayerInboxSystem::Tick();
	EXPECT_TRUE(currency.debts.find(kGold) == currency.debts.end()) << "不得按本地旧副本把(可能已作废的)欠款挂上";
	EXPECT_EQ(10u, DecidedThrough(player_));
	EXPECT_EQ(1u, g_sentFetches.size()) << "停用对当前实体一直有效,实体重建后才重拉";
	EXPECT_EQ(1u, g_sentAcks.size());
}

// DEFER 之后续做时闸不过:同样丢掉剩下的本地副本,但已定前缀既不丢、也不留到下一批 —— 先确认,再重拉。
TEST_F(PlayerInboxSystemTest, GateClosingDuringDeferResumeAcksThePrefixThenRefetches)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	tlsEcs.actorRegistry.remove<PlayerCurrencyComp>(player_);
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {Voided(10, kBlockType, 1001, kCancelled), BlockGold(11, 1101)}));
	ASSERT_NE(nullptr, GmState(player_));
	ASSERT_EQ(PlayerInboxPhase::kApplying, GmState(player_)->phase);

	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player_);
	Advance(PastBackoff(1));
	PlayerInboxSystem::Tick();
	EXPECT_EQ(PlayerInboxPhase::kAwaitDurable, GmState(player_)->phase);
	EXPECT_TRUE(GmState(player_)->pendingItems.empty()) << "未处理的 11 不在本地等闸";
	EXPECT_EQ(1u, g_sentFetches.size());

	// 确认不受前置闸限制:冻结中照样确认已落盘的前缀。
	player_inbox::AckPlayerInboxRequest prefixAck;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, prefixAck));
	ASSERT_EQ(1, prefixAck.decisions_size());
	ExpectDecision(prefixAck.decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_SKIPPED, kCancelled, 1001);
	PlayerInboxSystem::OnAckReply(
		prefixAck.player_id(), static_cast<uint32_t>(prefixAck.domain()), prefixAck.correlation_id(),
		AckReplyFor(prefixAck));

	// 确认完才重拉,而重拉仍要过闸:冻结中不拉。
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, g_sentFetches.size());

	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player_);
	tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player_); // 测试专用:模拟加载不变量恢复
	Advance(kBeyondAnyBackoffMs);
	PlayerInboxSystem::Tick();
	ASSERT_EQ(2u, g_sentFetches.size());
	EXPECT_EQ(10u, g_sentFetches.back().consumer_decided_through());

	PlayerInboxSystem::OnFetchReply(
		g_sentFetches.back().player_id(), static_cast<uint32_t>(g_sentFetches.back().domain()), g_sentFetches.back().correlation_id(),
		FetchReplyFor(g_sentFetches.back(), {BlockGold(11, 1101)}));
	EXPECT_TRUE(GoldBlocked(player_));
	player_inbox::AckPlayerInboxRequest restAck;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, restAck));
	EXPECT_EQ((std::vector<uint64_t>{10, 11}), AllAckedSeqs()) << "每个 seq 恰好确认一次,严格递增";
}

// ── 新落地重置(评审 L2)──────────────────────────────────────────────────────

// L2 的直接回归:旧实现落地时把 kApplying 直接打回 Idle 却不清 ackQueue,随即重拉;服务端把仍是 PENDING 的前缀
// 再投一遍,回放后同一 seq 在 Ack 里出现两次,服务端回 InvalidParameter。
TEST_F(PlayerInboxSystemTest, NewLandingDuringDeferAcksThePrefixBeforeRefetching)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	tlsEcs.actorRegistry.remove<PlayerCurrencyComp>(player_);
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {Voided(10, kBlockType, 1001, kCancelled), BlockGold(11, 1101)}));
	ASSERT_NE(nullptr, GmState(player_));
	ASSERT_EQ(PlayerInboxPhase::kApplying, GmState(player_)->phase);
	tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player_); // 测试专用:模拟加载不变量恢复

	// 归属变了 = 新落地。落地钩子只切状态:不当场发 Ack、不压存盘、不拉。
	tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player_).epoch = kEpoch + 1;
	const int persistBeforeLanding = g_persistCalls;
	PlayerInboxSystem::OnPlayerLanded(player_);
	EXPECT_EQ(PlayerInboxPhase::kAwaitDurable, GmState(player_)->phase);
	EXPECT_TRUE(GmState(player_)->pendingItems.empty()) << "旧归属下拉到的本地条目丢掉重拉";
	EXPECT_EQ(1u, GmState(player_)->ackQueue.size());
	EXPECT_EQ(persistBeforeLanding, g_persistCalls);
	EXPECT_EQ(0, g_ackAttempts);

	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, g_sentFetches.size()) << "上一批全部确认之前不拉下一批";

	player_inbox::AckPlayerInboxRequest prefixAck;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, prefixAck));
	EXPECT_EQ(kEpoch + 1, prefixAck.owner_epoch());
	ASSERT_EQ(1, prefixAck.decisions_size());
	ExpectDecision(prefixAck.decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_SKIPPED, kCancelled, 1001);
	Advance(1000);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, g_sentFetches.size()) << "Ack 在途期间也不拉";

	PlayerInboxSystem::OnAckReply(
		prefixAck.player_id(), static_cast<uint32_t>(prefixAck.domain()), prefixAck.correlation_id(),
		AckReplyFor(prefixAck));
	PlayerInboxSystem::Tick();
	ASSERT_EQ(2u, g_sentFetches.size()) << "前缀确认完,按 recheck 重拉";
	EXPECT_EQ(kEpoch + 1, g_sentFetches.back().owner_epoch());
	EXPECT_EQ(10u, g_sentFetches.back().consumer_decided_through());

	PlayerInboxSystem::OnFetchReply(
		g_sentFetches.back().player_id(), static_cast<uint32_t>(g_sentFetches.back().domain()), g_sentFetches.back().correlation_id(),
		FetchReplyFor(g_sentFetches.back(), {BlockGold(11, 1101)}));
	EXPECT_TRUE(GoldBlocked(player_));
	player_inbox::AckPlayerInboxRequest restAck;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, restAck));
	EXPECT_EQ((std::vector<uint64_t>{10, 11}), AllAckedSeqs()) << "seq 严格递增、无重复";
}

// Ack 回 kInvalidParameter 是确定性协议错误:重发只会得到同一个答复,按协议异常停用;服务端故障才退避重发。
TEST_F(PlayerInboxSystemTest, AckInvalidParameterHaltsWhileServerFaultsAreResent)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	player_inbox::AckPlayerInboxRequest first;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, first));

	// 服务端故障:保留 ackQueue,退避后原样重发。
	PlayerInboxSystem::OnAckReply(
		first.player_id(), static_cast<uint32_t>(first.domain()), first.correlation_id(),
		TipAckReplyFor(first, static_cast<uint32_t>(kPlayerInboxStoreUnavailable)));
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_FALSE(GmState(player_)->halted);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, g_sentAcks.size()) << "退避未到不重发";
	Advance(PastBackoff(1));
	PlayerInboxSystem::Tick();
	ASSERT_EQ(2u, g_sentAcks.size());
	ASSERT_EQ(1, g_sentAcks.back().decisions_size());
	ExpectDecision(g_sentAcks.back().decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 0, 1001);

	PlayerInboxSystem::OnAckReply(
		g_sentAcks.back().player_id(), static_cast<uint32_t>(g_sentAcks.back().domain()), g_sentAcks.back().correlation_id(),
		TipAckReplyFor(g_sentAcks.back(), static_cast<uint32_t>(kInvalidParameter)));
	EXPECT_TRUE(GmState(player_)->halted);
	EXPECT_EQ(PlayerInboxHaltReason::kAckAnomaly, GmState(player_)->haltReason);
	EXPECT_TRUE(GmState(player_)->ackQueue.empty());
	EXPECT_EQ(10u, DecidedThrough(player_)) << "结局已落盘;实体下次新建时按账本回放、重新确认";

	for (int round = 0; round < 3; ++round)
	{
		Advance(kBeyondAnyBackoffMs);
		PlayerInboxSystem::Tick();
	}
	PlayerInboxSystem::OnNotifyEvent(kTestPlayerId, kGmDomain);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(2u, g_sentAcks.size()) << "不当故障无限重发";
	EXPECT_EQ(1u, g_sentFetches.size()) << "停用后也不再拉";
}

// ── 不会自愈的延后(评审 L13、F3)────────────────────────────────────────────

// 账本损坏:WARN 在延后原因刚变成 ledger_invalid 时打一次,之后每 kStuckWarnEveryFailures 次连续延后再打一次
// (封顶后约 8 分钟一条,落在 [10m] 日志告警窗口之内,节拍的推导由 player_inbox_rules_test 钉住)。只影响日志。
TEST_F(PlayerInboxSystemTest, CorruptLedgerWarnsAgainEveryFixedNumberOfDeferrals)
{
	tlsEcs.actorRegistry.emplace<PlayerInboxLedgerInvalidComp>(player_); // 加载校验判损坏时挂的标签
	ScopedLogCapture capture;
	PlayerInboxSystem::OnPlayerLanded(player_);
	PlayerInboxSystem::Tick();
	ASSERT_NE(nullptr, GmState(player_));
	ASSERT_EQ(PlayerInboxDeferReason::kLedgerInvalid, GmState(player_)->deferReason);
	const std::vector<std::string> stuckWarn{"metric=inbox_ledger_invalid", "source=apply_gate"};
	const auto& domains = player_inbox_registry::ConsumerDomains();
	ASSERT_FALSE(domains.empty());
	EXPECT_EQ(domains.size(), CapturedLinesWith(stuckWarn)) << "每个注册域首次延后各打一次";
	for (const auto domain : domains)
	{
		EXPECT_EQ(1u, CapturedLinesWith({"metric=inbox_ledger_invalid", "source=apply_gate", " domain=" + std::to_string(domain) + " "}))
			<< "延后原因刚变成账本损坏:该域打一次";
	}

	for (uint32_t streak = 1; streak < rules::kStuckWarnEveryFailures; ++streak)
	{
		Advance(PastBackoff(streak));
		PlayerInboxSystem::Tick();
	}
	EXPECT_EQ(rules::kStuckWarnEveryFailures, GmState(player_)->failStreak);
	EXPECT_EQ(domains.size() * 2, CapturedLinesWith(stuckWarn)) << "各域到固定连续延后次数再打一次";
	for (const auto domain : domains)
	{
		EXPECT_EQ(2u, CapturedLinesWith({"metric=inbox_ledger_invalid", "source=apply_gate", " domain=" + std::to_string(domain) + " "}))
			<< "第 kStuckWarnEveryFailures 次连续延后:该域再打一次";
	}
	EXPECT_EQ(0, g_fetchAttempts) << "账本损坏:一次都不拉";
}

// ── 消费开关关着(评审 H1)──────────────────────────────────────────────────
// 开关关着时收件箱服务照常收单、本节点一条都不执行,服务端看不到任何异常;例行发布曾把它静默关掉。
// scene 侧唯一的信号是节点级 WARN:启动那行就是它,之后每 kNodeStuckRelogIntervalMs 补打一次。
// 节拍落在 [10m] 日志告警窗口之内,由 player_inbox_rules.h 的 static_assert 与 rules 用例钉住;这里钉接线。
// 补打排期是 thread_local:关过开关的用例结尾都重新打开并 tick 一次,清掉排期,不给后面的用例留残留。

TEST_F(PlayerInboxSystemTest, ConsumerDisabledWarnsAtStartupThenOnTheNodeRelogInterval)
{
	// Initialize 按注册表自检结果停用本线程的消费;自检不过时先停在这里,不污染其余用例。
	ASSERT_EQ(std::string{}, player_inbox_registry::SelfCheck());
	PlayerInboxSystem::SetConsumerConfigForTest(true, false, 0);
	ScopedLogCapture capture;
	PlayerInboxSystem::Initialize();
	EXPECT_EQ(1u, ConsumerDisabledWarnLines()) << "启动那一行就是 WARN,带 metric=";

	// 关着时落地不建运行时组件、不拉(行为不变)。
	PlayerInboxSystem::OnPlayerLanded(player_);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(0u, RuntimeCompCount());
	EXPECT_EQ(0, g_fetchAttempts);
	EXPECT_EQ(1u, ConsumerDisabledWarnLines()) << "启动刚打过:同一时刻的 tick 不重复打";

	Advance(rules::kNodeStuckRelogIntervalMs - 1);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, ConsumerDisabledWarnLines()) << "差 1ms 不到点";
	Advance(1);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(2u, ConsumerDisabledWarnLines()) << "到点补打一次";

	// 按生产节奏每秒 tick 一次走满下一个间隔:只在最后一个 tick 打一条,中间不多打。
	uint64_t elapsedMs = 0;
	do
	{
		Advance(1000);
		elapsedMs += 1000;
		PlayerInboxSystem::Tick();
	} while (elapsedMs < rules::kNodeStuckRelogIntervalMs);
	EXPECT_EQ(3u, ConsumerDisabledWarnLines());
	EXPECT_EQ(0u, RuntimeCompCount());
	EXPECT_EQ(0, g_fetchAttempts);

	PlayerInboxSystem::SetConsumerConfigForTest(true, true, 0);
	Advance(rules::kNodeStuckRelogIntervalMs);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(3u, ConsumerDisabledWarnLines()) << "开关打开之后不再打";
}

// 节拍不依赖 Initialize:开关在运行中被关上(经测试注入点模拟)时,下一个 tick 立即打首条;打开时清掉排期,
// 再关上又立即打首条。
TEST_F(PlayerInboxSystemTest, ConsumerTurnedOffWarnsOnTheNextTickAndStopsOnceTurnedBackOn)
{
	ScopedLogCapture capture;
	PlayerInboxSystem::Tick(); // 开着:清掉排期
	EXPECT_EQ(0u, ConsumerDisabledWarnLines());

	PlayerInboxSystem::SetConsumerConfigForTest(true, false, 0);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, ConsumerDisabledWarnLines()) << "关上之后的第一个 tick 立即打首条,不等满一个间隔";
	Advance(rules::kNodeStuckRelogIntervalMs - 1);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, ConsumerDisabledWarnLines());
	Advance(1);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(2u, ConsumerDisabledWarnLines());

	PlayerInboxSystem::SetConsumerConfigForTest(true, true, 0);
	Advance(rules::kNodeStuckRelogIntervalMs);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(2u, ConsumerDisabledWarnLines()) << "开着不打";

	PlayerInboxSystem::SetConsumerConfigForTest(true, false, 0);
	Advance(1000);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(3u, ConsumerDisabledWarnLines()) << "打开时已清掉排期:再关上又立即打首条";

	PlayerInboxSystem::SetConsumerConfigForTest(true, true, 0);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(3u, ConsumerDisabledWarnLines());
}

TEST_F(PlayerInboxSystemTest, ConsumerEnabledNeverLogsTheDisabledWarn)
{
	ASSERT_EQ(std::string{}, player_inbox_registry::SelfCheck());
	ScopedLogCapture capture;
	PlayerInboxSystem::Initialize();
	EXPECT_EQ(1u, CapturedLinesWith({"INFO  ", "[PlayerInbox]", "consumer_enabled=1"}))
		<< "开着:启动那行仍是 INFO 的生效配置";

	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		EmptyFetchReplyFor(fetch));
	for (int interval = 0; interval < 3; ++interval)
	{
		Advance(rules::kNodeStuckRelogIntervalMs);
		PlayerInboxSystem::Tick();
	}
	EXPECT_EQ(0u, ConsumerDisabledWarnLines());
}

// ── 离开时尽力补确认(评审 M4)───────────────────────────────────────────────

TEST_F(PlayerInboxSystemTest, LeavingSendsOneAckIgnoringBackoffAndLateRepliesAreHarmless)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	CompleteFakeSave(player_);

	// 先让常规的那次 Ack 发不出去,Ack 进入退避。
	g_ackSendOk = false;
	PlayerInboxSystem::Tick();
	ASSERT_EQ(1, g_ackAttempts);
	ASSERT_TRUE(g_sentAcks.empty());
	g_ackSendOk = true;

	// 离开是最后一次机会:不看退避,补发一次。
	PlayerInboxSystem::OnPlayerLeaving(player_);
	ASSERT_EQ(1u, g_sentAcks.size());
	const auto leaveAck = g_sentAcks.back();
	EXPECT_EQ(kEpoch, leaveAck.owner_epoch()) << "此刻仍是本节点的当前 epoch";
	EXPECT_EQ(kSceneNodeId, leaveAck.scene_node_id());
	ASSERT_EQ(1, leaveAck.decisions_size());
	ExpectDecision(leaveAck.decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 0, 1001);
	EXPECT_EQ(1, g_persistCalls) << "离开路径不压存盘(退出存盘就是最后一次)";

	// 实体销毁之后回包 / 失败通知才到:按 player_id 找不到实体就丢弃,不崩、不建组件。
	const entt::entity leaving = player_;
	DestroyPlayer(leaving);
	ASSERT_FALSE(tlsEcs.actorRegistry.valid(leaving));
	PlayerInboxSystem::OnAckReply(
		leaveAck.player_id(), static_cast<uint32_t>(leaveAck.domain()), leaveAck.correlation_id(),
		AckReplyFor(leaveAck));
	PlayerInboxSystem::OnAckFailed(leaveAck.player_id(), static_cast<uint32_t>(leaveAck.domain()), leaveAck.correlation_id(), kGrpcStatusUnavailable, "late failure");
	PlayerInboxSystem::Tick();
	EXPECT_EQ(0u, RuntimeCompCount());
	EXPECT_EQ(1u, g_sentAcks.size());
	EXPECT_EQ(1u, g_sentFetches.size());
}

// 评审 F2:走到退出收尾时快照组件多半已经在(退出存盘落地、或快路径判已落盘)。真正要拦的是"有快照、但不含本批结局"
// (例如 kFinishUnsynced 时快照还是上一份);只看快照在不在就确认,会在结局落盘之前让服务端终结这一行(IB-3)。
TEST_F(PlayerInboxSystemTest, LeavingDoesNotAckBeforeTheOutcomeIsPersisted)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	CompleteFakeSave(player_); // 上一份快照:账本里还没有 seq 10
	ASSERT_TRUE(tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player_).HasSnapshot());
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	const int persistBeforeLeaving = g_persistCalls;

	ScopedLogCapture capture;
	PlayerInboxSystem::OnPlayerLeaving(player_);
	EXPECT_EQ(0, g_ackAttempts) << "快照在,但不含本批结局:绝不在落盘之前确认(IB-3)";
	EXPECT_EQ(persistBeforeLeaving, g_persistCalls) << "也不为此压存盘";
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_EQ(1u, GmState(player_)->ackQueue.size()) << "留到下次落地补确认";
	EXPECT_EQ(1u, CapturedLinesWith({"metric=inbox_leave_ack", "result=not_durable"}));
}

TEST_F(PlayerInboxSystemTest, LeavingWithoutAnyPersistedSnapshotDoesNotAck)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	ASSERT_EQ(nullptr, tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player_))
		<< "前置:这个实体从没落过盘";
	const int persistBeforeLeaving = g_persistCalls;

	PlayerInboxSystem::OnPlayerLeaving(player_);
	EXPECT_EQ(0, g_ackAttempts) << "绝不在落盘之前确认(IB-3)";
	EXPECT_EQ(persistBeforeLeaving, g_persistCalls);
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_EQ(1u, GmState(player_)->ackQueue.size());
}

TEST_F(PlayerInboxSystemTest, LeavingDoesNotResendWhileAnAckIsInFlight)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	player_inbox::AckPlayerInboxRequest ack;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(player_, ack));

	ScopedLogCapture capture;
	PlayerInboxSystem::OnPlayerLeaving(player_);
	EXPECT_EQ(1u, g_sentAcks.size()) << "在途的 Ack 服务端照样处理,不依赖实体还在";
	EXPECT_EQ(1, g_ackAttempts);
	// 评审 LC-3:它若在实体销毁后失败,失败回调只打 DEBUG;这里留一行带 corr 的对账线索。
	EXPECT_EQ(1u, CapturedLinesWith({"metric=inbox_leave_ack", "result=ack_inflight",
									 "corr=" + std::to_string(ack.correlation_id()) + " ", "decisions=1"}));
}

TEST_F(PlayerInboxSystemTest, LeavingWithNoReachableInboxNodeIsHarmless)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	CompleteFakeSave(player_);

	g_ackSendOk = false;
	PlayerInboxSystem::OnPlayerLeaving(player_);
	EXPECT_EQ(1, g_ackAttempts);
	EXPECT_TRUE(g_sentAcks.empty());
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_EQ(PlayerInboxPhase::kAwaitDurable, GmState(player_)->phase);
	EXPECT_EQ(1u, GmState(player_)->ackQueue.size());
}

TEST_F(PlayerInboxSystemTest, LeavingWithoutRuntimeOrWithInvalidEntityDoesNothing)
{
	// 没在本节点落地过(或消费关闭):没有待确认的结局;离开路径不创建组件。
	PlayerInboxSystem::OnPlayerLeaving(player_);
	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerInboxRuntimeComp>(player_));
	PlayerInboxSystem::OnPlayerLeaving(entt::null);
	EXPECT_EQ(0, g_ackAttempts);
	EXPECT_EQ(0, g_fetchAttempts);
	EXPECT_EQ(0u, RuntimeCompCount());
}

TEST_F(PlayerInboxSystemTest, LeavingConfirmsOnlyTheDeferPrefixAndDropsTheRest)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	tlsEcs.actorRegistry.remove<PlayerCurrencyComp>(player_);
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {Voided(10, kBlockType, 1001, kCancelled), BlockGold(11, 1101)}));
	ASSERT_NE(nullptr, GmState(player_));
	ASSERT_EQ(PlayerInboxPhase::kApplying, GmState(player_)->phase);
	CompleteFakeSave(player_); // 退出存盘带上了已记账的前缀

	ScopedLogCapture capture;
	PlayerInboxSystem::OnPlayerLeaving(player_);
	ASSERT_EQ(1u, g_sentAcks.size());
	ASSERT_EQ(1, g_sentAcks.back().decisions_size());
	ExpectDecision(g_sentAcks.back().decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_SKIPPED, kCancelled, 1001);
	EXPECT_TRUE(GmState(player_)->pendingItems.empty()) << "未处理的 11 在服务端仍是 PENDING,下次落地重拉";
	EXPECT_EQ(10u, DecidedThrough(player_));
	// 11 已投递、没有结局:补发的 Ack 管不到它,只留一行对账线索(评审 M4 的缺口)。
	EXPECT_EQ(1u, CapturedLinesWith({"metric=inbox_leave_ack", "result=delivered_undecided", "source=leaving",
									 "items=1", "first_seq=11", "last_seq=11"}));
}

// 执行器在第一条就 DEFER:没有已定前缀,ackQueue 为空。离开时本地条目照样跟着实体丢掉,也要留下对账线索。
TEST_F(PlayerInboxSystemTest, LeavingDuringADeferOfTheFirstItemLeavesATraceAndSendsNothing)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	tlsEcs.actorRegistry.remove<PlayerCurrencyComp>(player_); // v1 执行器唯一的 DEFER 来源
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001), MakeItem(11, kUnblockType, 1101, GainPayload(kGold))}));
	ASSERT_NE(nullptr, GmState(player_));
	ASSERT_EQ(PlayerInboxPhase::kApplying, GmState(player_)->phase);
	ASSERT_TRUE(GmState(player_)->ackQueue.empty()) << "前置:停在第一条,没有已定前缀";
	MarkExiting(player_);
	CompleteFakeSave(player_);

	ScopedLogCapture capture;
	PlayerInboxSystem::OnPlayerLeaving(player_);
	EXPECT_EQ(0, g_ackAttempts) << "没有结局可确认";
	EXPECT_EQ(1u, CapturedLinesWith({"metric=inbox_leave_ack", "result=delivered_undecided", "source=leaving",
									 "items=2", "first_seq=10", "last_seq=11"}));
}

// 评审 M4 的缺口之一:Fetch 在途时退出开始,回包到达时应用前的前置闸判 kExiting,按 L1 丢掉本地副本。
// 服务端那两行已投递、仍是 PENDING,场景侧没有结局可确认 —— 离开时的补发 Ack 管不到它们,只留对账线索。
TEST_F(PlayerInboxSystemTest, ReplyDroppedByTheExitGateLeavesADeliveredUndecidedTrace)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	MarkExiting(player_);

	ScopedLogCapture capture;
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001), MakeItem(11, kUnblockType, 1101, GainPayload(kGold))}));
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_EQ(PlayerInboxPhase::kIdle, GmState(player_)->phase);
	EXPECT_EQ(PlayerInboxDeferReason::kExiting, GmState(player_)->deferReason);
	EXPECT_TRUE(GmState(player_)->pendingItems.empty());
	EXPECT_FALSE(GoldBlocked(player_)) << "退出期不执行";
	EXPECT_EQ(nullptr, GmCursor(player_)) << "不记账";
	EXPECT_EQ(1u, CapturedLinesWith({"metric=inbox_leave_ack", "result=delivered_undecided", "source=apply_gate_exiting",
									 "items=2", "first_seq=10", "last_seq=11"}));

	// 退出存盘落地后收尾:没有结局可确认,什么都不发。
	CompleteFakeSave(player_);
	PlayerInboxSystem::OnPlayerLeaving(player_);
	EXPECT_EQ(0, g_ackAttempts);
}

// 评审 LC-2:退出期闸不过时,批首账本里已有结局的条目(这里是上一会话已 APPLIED、Ack 丢了的 seq 10)先入确认队列,
// 只丢掉需要执行的剩余部分。旧实现整批丢掉,10 每次短会话都再留一行已投递未确认。
TEST_F(PlayerInboxSystemTest, ExitGateAcknowledgesTheReplayablePrefixAndDropsOnlyNewEntries)
{
	ASSERT_NO_FATAL_FAILURE(SeedLedger(10, {MakeDecision(10, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 1001, kBlockType)}));
	CompleteFakeSave(player_); // 这份账本就是从存档加载的:已落盘
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	MarkExiting(player_);

	ScopedLogCapture capture;
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001), MakeItem(11, kUnblockType, 1101, GainPayload(kGold))}));
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_TRUE(GmState(player_)->pendingItems.empty()) << "需要执行的 11 照旧丢掉,下次落地重拉";
	EXPECT_EQ(10u, DecidedThrough(player_)) << "退出期不执行、不记账";
	EXPECT_EQ(0, g_persistCalls) << "没有新记账就不请求存盘";
	EXPECT_EQ(1u, CapturedLinesWith({"metric=inbox_leave_ack", "result=decided_prefix_queued", "source=apply_gate_exiting",
									 "items=1", "first_seq=10", "last_seq=10"}));
	EXPECT_EQ(1u, CapturedLinesWith({"metric=inbox_leave_ack", "result=delivered_undecided", "source=apply_gate_exiting",
									 "items=1", "first_seq=11", "last_seq=11"}));

	// 回放的结局本来就在已落盘的账本里:当场确认,确认不受前置闸限制。
	ASSERT_EQ(1u, g_sentAcks.size());
	ASSERT_EQ(1, g_sentAcks.back().decisions_size());
	ExpectDecision(g_sentAcks.back().decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 0, 1001);
}

// 评审 LC-1:冻结 / 交接与退出同时存在时,前置闸先报 kFrozen / kHandoff,但玩家同样正在下线,
// 丢掉的已投递条目照样要留对账线索(按退出标记判断,不按闸返回的原因)。
TEST_F(PlayerInboxSystemTest, FrozenExitingPlayerStillLeavesADeliveredUndecidedTrace)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player_);
	MarkExiting(player_);

	ScopedLogCapture capture;
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	ASSERT_NE(nullptr, GmState(player_));
	EXPECT_EQ(PlayerInboxDeferReason::kFrozen, GmState(player_)->deferReason) << "闸按判定顺序先报冻结";
	EXPECT_FALSE(GoldBlocked(player_));
	EXPECT_EQ(1u, CapturedLinesWith({"metric=inbox_leave_ack", "result=delivered_undecided", "source=apply_gate_exiting",
									 "items=1", "first_seq=10", "last_seq=10"}));
}

// 评审 M4 的缺口之二:退出收敛得比 Fetch RTT 还快。离开时 Fetch 仍在途,留一行对账线索;实体销毁之后回包才到,丢弃。
TEST_F(PlayerInboxSystemTest, LeavingWithAFetchInFlightLeavesATraceAndTheLateReplyIsDropped)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	MarkExiting(player_);

	{
		ScopedLogCapture capture;
		PlayerInboxSystem::OnPlayerLeaving(player_);
		EXPECT_EQ(0, g_ackAttempts);
		EXPECT_EQ(1u, CapturedLinesWith({"metric=inbox_leave_ack", "result=fetch_inflight",
										 "corr=" + std::to_string(fetch.correlation_id()) + ";"}));
	}

	DestroyPlayer(player_);
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	EXPECT_EQ(0u, RuntimeCompCount()) << "回包按 player_id 找不到实体就丢弃,不建组件";
	EXPECT_EQ(0, g_ackAttempts);
}

TEST_F(PlayerInboxSystemTest, LeavingStillAcksTheDecidedPrefixOfAHaltedDomain)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001), MakeItem(11, kUnknownType, 1101, "opaque")}));
	ASSERT_NE(nullptr, GmState(player_));
	ASSERT_TRUE(GmState(player_)->halted);
	CompleteFakeSave(player_);

	// 停用只停拉取与应用;已落盘的结局照样补确认。
	PlayerInboxSystem::OnPlayerLeaving(player_);
	ASSERT_EQ(1u, g_sentAcks.size());
	ASSERT_EQ(1, g_sentAcks.back().decisions_size());
	ExpectDecision(g_sentAcks.back().decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 0, 1001);
}

// 同一玩家在本节点重新落地成新实体后,上一个实体离开时补发的 Ack 迟到:关联号是节点级单调的,对不上就丢弃。
TEST_F(PlayerInboxSystemTest, LateAckFromAPreviousEntityIsDiscardedAfterRelanding)
{
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	CompleteFakeSave(player_);
	PlayerInboxSystem::OnPlayerLeaving(player_);
	ASSERT_EQ(1u, g_sentAcks.size());
	const auto leaveAck = g_sentAcks.back();

	// 新实体从存档加载:账本就是上一个实体最后一次落盘的那份;新登录铸了新 epoch。
	const PlayerInboxLedgerComp persistedLedger = tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player_)
													  .snapshot->player_database_data()
													  .inbox_ledger();
	DestroyPlayer(player_);
	const entt::entity relanded = CreateLoadedPlayer(kEpoch + 1, persistedLedger);
	player_inbox::FetchPlayerInboxRequest relandFetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(relanded, relandFetch));

	PlayerInboxSystem::OnAckReply(
		leaveAck.player_id(), static_cast<uint32_t>(leaveAck.domain()), leaveAck.correlation_id(),
		AckReplyFor(leaveAck));
	ASSERT_NE(nullptr, GmState(relanded));
	EXPECT_EQ(PlayerInboxPhase::kFetching, GmState(relanded)->phase) << "旧实体的回包不能结束新实体的在途 Fetch";
	EXPECT_EQ(relandFetch.correlation_id(), GmState(relanded)->correlationId);

	// 服务端在处理那次 Ack 之前先收到了新的 Fetch:10 再投一遍,新实体按账本回放(不重做效果)。
	PlayerInboxSystem::OnFetchReply(
		relandFetch.player_id(), static_cast<uint32_t>(relandFetch.domain()), relandFetch.correlation_id(),
		FetchReplyFor(relandFetch, {BlockGold(10, 1001)}));
	EXPECT_FALSE(GoldBlocked(relanded)) << "回放不调执行器";
	player_inbox::AckPlayerInboxRequest relandAck;
	ASSERT_NO_FATAL_FAILURE(SaveAndAck(relanded, relandAck));
	EXPECT_EQ(kEpoch + 1, relandAck.owner_epoch());
	ASSERT_EQ(1, relandAck.decisions_size());
	ExpectDecision(relandAck.decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 0, 1001);

	PlayerInboxSystem::OnAckReply(
		leaveAck.player_id(), static_cast<uint32_t>(leaveAck.domain()), leaveAck.correlation_id(),
		AckReplyFor(leaveAck));
	PlayerInboxSystem::OnAckFailed(leaveAck.player_id(), static_cast<uint32_t>(leaveAck.domain()), leaveAck.correlation_id(), kGrpcStatusUnavailable, "late failure");
	EXPECT_EQ(PlayerInboxPhase::kAcking, GmState(relanded)->phase) << "旧实体的回包不能结束新实体的在途 Ack";
	EXPECT_EQ(relandAck.correlation_id(), GmState(relanded)->correlationId);

	PlayerInboxSystem::OnAckReply(
		relandAck.player_id(), static_cast<uint32_t>(relandAck.domain()), relandAck.correlation_id(),
		AckReplyFor(relandAck));
	EXPECT_EQ(PlayerInboxPhase::kIdle, GmState(relanded)->phase);
	EXPECT_TRUE(GmState(relanded)->ackQueue.empty());
}

// ── 退出收尾接缝(评审 F1)──────────────────────────────────────────────────

// 上面的 Leaving* 用例都直接调 OnPlayerLeaving,钉不住生产里那一行调用:它还在不在、是不是排在 DestroyPlayer 之前
// (OnPlayerLeaving 在实体无效时直接返回)。这里走真正的收尾入口 FinishExitOrAwaitDbTaskAck(闸门判 kFinish)
// → FinishExitAfterPersist,实体带着生产里此刻一定在的退出标记;把那一行挪到销毁之后或合并时弄丢,这个用例就会红。
// 同步确认是依赖允许的边界，不代表真实异步Redis回包能赶在实体销毁之前。
TEST_F(PlayerInboxSystemTest, ExitFinishAcksBeforeDestroyWhenSqlConfirmationCompletesSynchronously)
{
	BindSession(player_);
	player_inbox::FetchPlayerInboxRequest fetch;
	ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
	PlayerInboxSystem::OnFetchReply(
		fetch.player_id(), static_cast<uint32_t>(fetch.domain()), fetch.correlation_id(),
		FetchReplyFor(fetch, {BlockGold(10, 1001)}));
	ASSERT_NE(nullptr, GmState(player_));
	ASSERT_EQ(PlayerInboxPhase::kAwaitDurable, GmState(player_)->phase);

	// GM 处置之后紧接着踢线:HandleExitGameNode 挂上退出标记,随后退出存盘落地,快照带上了 seq 10 的结局。
	MarkExiting(player_);
	CompleteFakeSave(player_);
	ASSERT_EQ(0, g_ackAttempts) << "前置:常规路径还没来得及确认(没有 tick)";

	PlayerInboxSystem::SetTransportForTest(&FakeSendFetch, &ProbingSendAck);
	EXPECT_TRUE(PlayerLifecycleSystem::FinishExitOrAwaitDbTaskAck(kTestPlayerId))
		<< "前置:没有在途 DBTask、也不欠 MySQL,闸门判 kFinish、当场收尾";

	ASSERT_EQ(1, g_ackAttempts) << "退出收尾恰好补发一次 Ack";
	ASSERT_EQ(1u, g_sentAcks.size());
	const auto &leaveAck = g_sentAcks.back();
	EXPECT_EQ(kEpoch, leaveAck.owner_epoch()) << "退出前本节点的 owner_epoch";
	ASSERT_EQ(1, leaveAck.decisions_size());
	ExpectDecision(leaveAck.decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 0, 1001);
	EXPECT_TRUE(g_ackMoment.entityLive) << "补发必须排在 DestroyPlayer 之前";
	EXPECT_TRUE(g_ackMoment.exitMarkersPresent) << "补发发生在退出收尾里,不是别的路径";
	EXPECT_EQ(kEpoch, g_ackMoment.epochOnEntity) << "PlayerOwnerEpochComp 此刻还在";

	EXPECT_FALSE(tlsEcs.actorRegistry.valid(player_)) << "收尾之后实体已销毁";
	EXPECT_EQ(entt::entity{entt::null}, tlsEcs.GetPlayer(kTestPlayerId));
}

// 正式退出路径不等待只读SQL探针：迟到回包不能碰新实体，新E依永久账本补确认而不重复入账。
TEST_F(PlayerInboxSystemTest, ExitFinishDropsAsyncSqlReplyAndRelandingAcksWithoutPayingAgain)
{
    g_transportDomain = player_inbox::PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT;
    BindSession(player_);
    auto& attributes = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player_);
    attributes.set_health(100); attributes.set_mana(50);
    auto& derived = tlsEcs.actorRegistry.emplace<DerivedAttributesComp>(player_);
    derived.set_max_health(100); derived.set_max_mana(50);
    BattleSettlementData settlement;
    settlement.set_battle_id(792); settlement.set_player_id(kTestPlayerId);
    settlement.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN);
    settlement.set_health(80); settlement.set_mana(30); settlement.set_gold_gain(5);
    const auto item = MakeItem(10, player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT, 272,
        settlement.SerializeAsString());
    player_inbox::FetchPlayerInboxRequest fetch;
    ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
    PlayerInboxSystem::OnFetchReply(kTestPlayerId, g_transportDomain, fetch.correlation_id(), FetchReplyFor(fetch, {item}));
    ASSERT_EQ(5u, CurrencySystem::GetBalance(player_, kCurrencyGold));
    MarkExiting(player_);
    CompleteFakeSave(player_);
    const PlayerAllData persisted = *tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player_).snapshot;
    const auto originalAck = g_sqlAck;
    g_deferSqlReply = true;
    EXPECT_TRUE(PlayerLifecycleSystem::FinishExitOrAwaitDbTaskAck(kTestPlayerId));
    ASSERT_EQ(1u, g_sqlReplies.size());
    auto oldReply = g_sqlReplies.front();
    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player_));
    oldReply(originalAck);
    EXPECT_TRUE(g_sentAcks.empty()) << "实体销毁后不得从旧SQL确认补发Ack";

    player_ = CreateLoadedPlayer(kEpoch + 1, persisted.player_database_data().inbox_ledger());
    tlsEcs.actorRegistry.get<CurrencyComp>(player_).CopyFrom(persisted.player_database_data().currency());
    auto& reloaded = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player_);
    reloaded.set_health(80); reloaded.set_mana(30);
    auto& reloadedDerived = tlsEcs.actorRegistry.emplace<DerivedAttributesComp>(player_);
    reloadedDerived.set_max_health(100); reloadedDerived.set_max_mana(50);
    g_deferSqlReply = false;
    ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
    PlayerInboxSystem::OnFetchReply(kTestPlayerId, g_transportDomain, fetch.correlation_id(), FetchReplyFor(fetch, {item}));
    EXPECT_EQ(5u, CurrencySystem::GetBalance(player_, kCurrencyGold)) << "永久水位回放不得重发金币";
    EXPECT_EQ(80u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player_).health());
    CompleteFakeSave(player_, false);
    PlayerInboxSystem::Tick();
    EXPECT_TRUE(g_sentAcks.empty()) << "原E双ACK不能证明新E提交";
    CompleteFakeSqlSave(player_);
    PlayerInboxSystem::Tick();
    ASSERT_EQ(1u, g_sentAcks.size());
    const auto ack = g_sentAcks.back();
    EXPECT_EQ(kEpoch + 1, ack.owner_epoch());
    ASSERT_EQ(1, ack.decisions_size());
    ExpectDecision(ack.decisions(0), 10, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, 0, 272);
    oldReply(originalAck);
    EXPECT_EQ(1u, g_sentAcks.size()) << "旧闭包再到也不能给新实体补资格";
    EXPECT_EQ(5u, CurrencySystem::GetBalance(player_, kCurrencyGold));
    PlayerInboxSystem::OnAckReply(kTestPlayerId, g_transportDomain, ack.correlation_id(), AckReplyFor(ack));
}

// Battle专域只回放APPLIED，效果先和永久水位同snapshot保存，再等原E/q双SQLACK。
TEST_F(PlayerInboxSystemTest, BattleDomainPaysOnceAcrossRedeliveryAndWaitsForBothSqlRows)
{
    g_transportDomain = player_inbox::PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT;
    auto& attributes = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player_);
    attributes.set_health(100); attributes.set_mana(50);
    auto& derived = tlsEcs.actorRegistry.emplace<DerivedAttributesComp>(player_);
    derived.set_max_health(100); derived.set_max_mana(50);
    tlsEcs.actorRegistry.emplace<InBattleComp>(player_).set_battle_id(791);
    BattleSettlementData settlement;
    settlement.set_battle_id(791); settlement.set_player_id(kTestPlayerId);
    settlement.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN); settlement.set_health(80); settlement.set_mana(30);
    settlement.set_gold_gain(5);
    const auto item = MakeItem(10, player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT, 271,
        settlement.SerializeAsString());
    player_inbox::FetchPlayerInboxRequest fetch;
    ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
    PlayerInboxSystem::OnFetchReply(kTestPlayerId, g_transportDomain, fetch.correlation_id(), FetchReplyFor(fetch, {item}));
    EXPECT_EQ(5u, CurrencySystem::GetBalance(player_, kCurrencyGold));
    EXPECT_EQ(80u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player_).health());
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<InBattleComp>(player_)) << "Redis/SQL提交前不能先解冻";
    CompleteFakeSave(player_, false);
    PlayerInboxSystem::Tick();
    EXPECT_TRUE(g_sentAcks.empty());
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<InBattleComp>(player_));
    CompleteFakeSqlSave(player_);
    PlayerInboxSystem::Tick();
    ASSERT_EQ(1u, g_sentAcks.size());
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player_));
    const auto ack = g_sentAcks.back();
    ASSERT_EQ(1, ack.decisions_size());
    EXPECT_EQ(player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, ack.decisions(0).outcome());
    PlayerInboxSystem::OnAckReply(kTestPlayerId, g_transportDomain, ack.correlation_id(), AckReplyFor(ack));
    tlsEcs.actorRegistry.get<BaseAttributesComp>(player_).set_health(90);
    Advance(rules::kMinCheckGapMs + 1);
    PlayerInboxSystem::OnNotifyEvent(kTestPlayerId, g_transportDomain);
    PlayerInboxSystem::Tick();
    ASSERT_EQ(2u, g_sentFetches.size());
    fetch = g_sentFetches.back();
    PlayerInboxSystem::OnFetchReply(kTestPlayerId, g_transportDomain, fetch.correlation_id(), FetchReplyFor(fetch, {item}));
    EXPECT_EQ(5u, CurrencySystem::GetBalance(player_, kCurrencyGold)) << "永久域游标拦重复发奖";
    EXPECT_EQ(90u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player_).health()) << "重复载荷不回写旧HP";
    ASSERT_EQ(2u, g_sentAcks.size());
}

TEST_F(PlayerInboxSystemTest, BattleDomainNeverTurnsVoidIntoTerminalSkipped)
{
    g_transportDomain = player_inbox::PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT;
    player_inbox::FetchPlayerInboxRequest fetch;
    ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
    PlayerInboxSystem::OnFetchReply(kTestPlayerId, g_transportDomain, fetch.correlation_id(),
        FetchReplyFor(fetch, {Voided(10, player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT, 271, kCancelled)}));
    EXPECT_TRUE(g_sentAcks.empty());
    EXPECT_EQ(nullptr, rules::FindCursor(tlsEcs.actorRegistry.get<PlayerInboxLedgerComp>(player_), g_transportDomain));
    const auto& domains = tlsEcs.actorRegistry.get<PlayerInboxRuntimeComp>(player_).domains;
    const auto state = std::find_if(domains.begin(), domains.end(), [](const auto& row) { return row.domain == g_transportDomain; });
    ASSERT_NE(domains.end(), state); EXPECT_TRUE(state->halted);
}

TEST_F(PlayerInboxSystemTest, BattleDomainBelowWatermarkWithoutReceiptNeverClaimsAppliedOrUnknown)
{
    g_transportDomain = player_inbox::PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT;
    auto& cursor = rules::MutableCursor(tlsEcs.actorRegistry.get<PlayerInboxLedgerComp>(player_), g_transportDomain);
    cursor.set_decided_through_seq(100);
    const auto before = tlsEcs.actorRegistry.get<PlayerInboxLedgerComp>(player_).SerializeAsString();
    player_inbox::FetchPlayerInboxRequest fetch;
    ASSERT_NO_FATAL_FAILURE(LandAndFetch(player_, fetch));
    PlayerInboxSystem::OnFetchReply(kTestPlayerId, g_transportDomain, fetch.correlation_id(),
        FetchReplyFor(fetch, {MakeItem(10, player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT, 271, "unread")}));
    EXPECT_TRUE(g_sentAcks.empty());
    EXPECT_EQ(before, tlsEcs.actorRegistry.get<PlayerInboxLedgerComp>(player_).SerializeAsString());
}
