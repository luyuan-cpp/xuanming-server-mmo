// 收件箱按调用身份认领完成通知:回显逐字段核对,迟到调用不改变新请求,失败只退避。
// 时钟、传输与存盘均经现有接口注入;运行时组件只由 OnPlayerLanded 创建,不连接外部服务。

#include <gtest/gtest.h>

#include <algorithm>
#include <cstdint>
#include <chrono>
#include <functional>
#include <string>
#include <memory>
#include <vector>

#include "engine/core/type_define/player_guid_comp.h"
#include "core/network/grpc_client_call.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "node/system/grpc_call_deadline.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_inbox_runtime_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/player_inbox_registry.h"
#include "player/system/player_inbox_rules.h"
#include "player/system/player_inbox_system.h"
#include "player/system/role_admission_system.h"
#include "proto/common/base/node.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_inbox_ledger_comp.pb.h"
#include "proto/player_inbox/player_inbox.pb.h"
#include "table/proto/tip/player_inbox_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
constexpr uint64_t kPlayerId = 950300001;
constexpr uint32_t kDomain = player_inbox::PLAYER_INBOX_DOMAIN_SCENE_GM_SANCTION;
uint64_t gNowMs = 100000;
uint32_t gPersistCalls = 0;
std::vector<player_inbox::FetchPlayerInboxRequest> gFetchRequests;
std::vector<player_inbox::AckPlayerInboxRequest> gAckRequests;

uint64_t FakeNowMs() { return gNowMs; }

bool FakePersist(entt::entity /*player*/)
{
	++gPersistCalls;
	// 返回 false 不代表已落盘;用例单独完成快照,核对 Ack 的落盘前置。
	return false;
}

bool FakeSendFetch(const player_inbox::FetchPlayerInboxRequest &request)
{
	if (static_cast<uint32_t>(request.domain()) == kDomain)
	{
		gFetchRequests.push_back(request);
		return true;
	}
	return false;
}

bool FakeSendAck(const player_inbox::AckPlayerInboxRequest &request)
{
	if (static_cast<uint32_t>(request.domain()) == kDomain)
	{
		gAckRequests.push_back(request);
		return true;
	}
	return false;
}

player_inbox::FetchPlayerInboxResponse FetchReply(const player_inbox::FetchPlayerInboxRequest &request)
{
	player_inbox::FetchPlayerInboxResponse reply;
	reply.set_player_id(request.player_id());
	reply.set_domain(request.domain());
	reply.set_correlation_id(request.correlation_id());
	reply.set_fence(player_inbox::PLAYER_INBOX_FENCE_MATCH);
	return reply;
}

player_inbox::AckPlayerInboxResponse AckReply(const player_inbox::AckPlayerInboxRequest &request)
{
	player_inbox::AckPlayerInboxResponse reply;
	reply.set_player_id(request.player_id());
	reply.set_domain(request.domain());
	reply.set_correlation_id(request.correlation_id());
	reply.set_fence(player_inbox::PLAYER_INBOX_FENCE_MATCH);
	for (const auto &decision : request.decisions())
	{
		auto *result = reply.add_results();
		result->set_seq(decision.seq());
		result->set_result(player_inbox::PLAYER_INBOX_ACK_FINALIZED);
	}
	return reply;
}

enum class EchoField : uint8_t
{
	kPlayerId,
	kDomain,
	kCorrelationId,
	kCount
};

template <typename Response>
void ClearEcho(Response &reply)
{
	reply.clear_player_id();
	reply.clear_domain();
	reply.clear_correlation_id();
}

template <typename Response>
void SetMismatchedEcho(Response &reply, EchoField field, uint64_t correlationId)
{
	// 另外两项留 0,锁住逐字段核对,尤其不能因 correlation_id == 0 而放过错误玩家或域。
	ClearEcho(reply);
	switch (field)
	{
	case EchoField::kPlayerId:
		reply.set_player_id(kPlayerId + 1);
		break;
	case EchoField::kDomain:
		reply.set_domain(static_cast<player_inbox::PlayerInboxDomain>(kDomain + 1));
		break;
	case EchoField::kCorrelationId:
		reply.set_correlation_id(correlationId + 1);
		break;
	case EchoField::kCount:
		ADD_FAILURE() << "无效的回显字段测试参数";
		break;
	}
}

class PlayerInboxReplyTest : public ::testing::Test
{
protected:
	void SetUp() override
	{
		gNowMs = 100000;
		gPersistCalls = 0;
		gFetchRequests.clear();
		gAckRequests.clear();
		PlayerInboxSystem::SetNowFnForTest(&FakeNowMs);
		PlayerInboxSystem::SetPersistFnForTest(&FakePersist);
		PlayerInboxSystem::SetTransportForTest(&FakeSendFetch, &FakeSendAck);
		PlayerInboxSystem::SetConsumerConfigForTest(true, true, 0);
		ASSERT_TRUE(player_inbox_registry::SelfCheck().empty());
		PlayerInboxSystem::Initialize();
		CreateLoadedPlayer();
		PlayerInboxSystem::OnPlayerLanded(player_);
		ASSERT_TRUE(tlsEcs.actorRegistry.all_of<PlayerInboxRuntimeComp>(player_));
		const auto &runtime = tlsEcs.actorRegistry.get<PlayerInboxRuntimeComp>(player_);
		ASSERT_TRUE(std::any_of(runtime.domains.begin(), runtime.domains.end(),
			[](const PlayerInboxDomainState &state) { return state.domain == kDomain; }));
		PlayerInboxSystem::Tick();
		ASSERT_EQ(1u, gFetchRequests.size());
		ASSERT_EQ(PlayerInboxPhase::kFetching, State().phase);
		ASSERT_NE(0u, State().correlationId);
	}

	void TearDown() override
	{
		tlsEcs.playerList.erase(kPlayerId);
		if (tlsEcs.actorRegistry.valid(player_))
		{
			tlsEcs.actorRegistry.destroy(player_);
		}
		PlayerInboxSystem::SetNowFnForTest(nullptr);
		PlayerInboxSystem::SetPersistFnForTest(nullptr);
		PlayerInboxSystem::SetTransportForTest(nullptr, nullptr);
		PlayerInboxSystem::SetConsumerConfigForTest(false, false, 0);
		gFetchRequests.clear();
		gAckRequests.clear();
		admission_.reset();
	}

	void CreateLoadedPlayer()
	{
		admission_.reset();
		RoleAdmissionSystem::Dependencies deps;
		deps.localNode = [] { return RoleAdmissionSystem::LocalNode{"inbox-reply-test", "4242", 1}; };
		deps.now = [] { return RoleAdmissionSystem::Clock::time_point(std::chrono::milliseconds(gNowMs)); };
		common::base::RoleAdmissionIdentity identity;
		identity.set_player_id(kPlayerId); identity.set_owner_epoch(1);
		identity.set_generation(++generation_); identity.set_owner_revision(1);
		identity.set_scene_instance_id("inbox-reply-test");
		deps.confirmedAdmissions.push_back(identity);
		deps.readSqlAck = [this](Guid id, auto done) {
			EXPECT_EQ(kPlayerId, id); ++sqlReads_;
			done(sqlAck_); return true;
		};
		admission_ = std::make_unique<RoleAdmissionSystem::ScopedDependencies>(std::move(deps));
		player_ = tlsEcs.actorRegistry.create();
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player_, kPlayerId);
		tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player_).epoch = 1;
		tlsEcs.actorRegistry.emplace<PlayerRoleAdmissionComp>(player_, generation_, std::uint64_t{1});
		// 持久数据在加载创建点显式挂上;运行时组件由落地入口延迟创建。
		tlsEcs.actorRegistry.emplace<PlayerInboxLedgerComp>(player_);
		tlsEcs.actorRegistry.emplace<CurrencyComp>(player_);
		tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player_);
		tlsEcs.actorRegistry.emplace<PlayerLastPersistedSnapshotComp>(player_);
		tlsEcs.playerList[kPlayerId] = player_;
	}

	const PlayerInboxDomainState &State() const
	{
		const auto &runtime = tlsEcs.actorRegistry.get<PlayerInboxRuntimeComp>(player_);
		return *std::find_if(runtime.domains.begin(), runtime.domains.end(),
			[](const PlayerInboxDomainState &state) { return state.domain == kDomain; });
	}

	void StartAck()
	{
		const auto request = gFetchRequests.back();
		auto reply = FetchReply(request);
		auto *item = reply.add_items();
		item->set_seq(1);
		item->set_request_id(7600001);
		item->set_msg_type(player_inbox::PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN);
		item->set_directive(player_inbox::PLAYER_INBOX_DIRECTIVE_VOID);
		item->set_void_reason(player_inbox::PLAYER_INBOX_VOID_CANCELLED);
		PlayerInboxSystem::OnFetchReply(kPlayerId, kDomain, request.correlation_id(), reply);
		ASSERT_EQ(PlayerInboxPhase::kAwaitDurable, State().phase);
		ASSERT_EQ(1u, State().ackQueue.size());
		ASSERT_EQ(1u, gPersistCalls);
		ASSERT_TRUE(gAckRequests.empty()) << "未落盘时不能确认";

		// 同原保存链先捕获不可变E/q提交原件，成功hook先于全局Replace；Redis与SQL是两份独立证据。
		auto payload = std::make_shared<PlayerAllData>();
		payload->set_last_save_seq(1);
		payload->mutable_player_database_data()->set_player_id(kPlayerId);
		payload->mutable_player_database_1_data()->set_player_id(kPlayerId);
		payload->mutable_player_database_data()->mutable_inbox_ledger()->CopyFrom(
			tlsEcs.actorRegistry.get<PlayerInboxLedgerComp>(player_));
		auto committed = RoleAdmissionSystem::CaptureSnapshotCommit(player_, payload, 1, 1);
		ASSERT_TRUE(static_cast<bool>(committed));
		committed();
		auto &snapshot = tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player_);
		snapshot.Replace(*payload);
		const std::string version = "00000000000000000001:00000000000000000001";
		sqlAck_ = RoleAdmissionSystem::SqlAck{"1", version, version, version};
		PlayerInboxSystem::Tick();
		ASSERT_EQ(1u, gAckRequests.size());
		ASSERT_EQ(1u, sqlReads_);
		ASSERT_EQ(PlayerInboxPhase::kAcking, State().phase);
	}

	static uint64_t InflightTimeoutMs()
	{
		return static_cast<uint64_t>(grpc_call_deadline::Get(common::base::PlayerInboxNodeService).count()) + 5000;
	}

	entt::entity player_{entt::null};
	uint64_t generation_{0};
	uint32_t sqlReads_{0};
	std::optional<RoleAdmissionSystem::SqlAck> sqlAck_;
	std::unique_ptr<RoleAdmissionSystem::ScopedDependencies> admission_;
};

class PlayerInboxEchoMismatchTest : public PlayerInboxReplyTest, public ::testing::WithParamInterface<EchoField>
{
};

TEST_P(PlayerInboxEchoMismatchTest, FetchDiscardsEachNonzeroMismatchedEcho)
{
	const auto request = gFetchRequests.back();
	auto reply = FetchReply(request);
	SetMismatchedEcho(reply, GetParam(), request.correlation_id());
	PlayerInboxSystem::OnFetchReply(kPlayerId, kDomain, request.correlation_id(), reply);
	EXPECT_EQ(PlayerInboxPhase::kFetching, State().phase);
	EXPECT_EQ(request.correlation_id(), State().correlationId);
	EXPECT_EQ(gNowMs, State().inflightSinceMs);
	EXPECT_FALSE(State().halted);
	EXPECT_EQ(0u, gPersistCalls);
}

TEST_P(PlayerInboxEchoMismatchTest, AckDiscardsEachNonzeroMismatchedEcho)
{
	ASSERT_NO_FATAL_FAILURE(StartAck());
	const auto request = gAckRequests.back();
	auto reply = AckReply(request);
	SetMismatchedEcho(reply, GetParam(), request.correlation_id());
	PlayerInboxSystem::OnAckReply(kPlayerId, kDomain, request.correlation_id(), reply);
	EXPECT_EQ(PlayerInboxPhase::kAcking, State().phase);
	EXPECT_EQ(request.correlation_id(), State().correlationId);
	EXPECT_EQ(1u, State().ackQueue.size());
	EXPECT_FALSE(State().halted);
}

INSTANTIATE_TEST_SUITE_P(EachField, PlayerInboxEchoMismatchTest,
	::testing::Values(EchoField::kPlayerId, EchoField::kDomain, EchoField::kCorrelationId));

TEST_F(PlayerInboxReplyTest, FetchWithZeroEchoCompletesTheCapturedCall)
{
	const auto request = gFetchRequests.back();
	auto reply = FetchReply(request);
	ClearEcho(reply);
	PlayerInboxSystem::OnFetchReply(kPlayerId, kDomain, request.correlation_id(), reply);
	EXPECT_EQ(PlayerInboxPhase::kIdle, State().phase);
	EXPECT_EQ(0u, State().correlationId);
	EXPECT_EQ(0u, State().inflightSinceMs);
	EXPECT_FALSE(State().halted);
	EXPECT_TRUE(gAckRequests.empty());
}

TEST_F(PlayerInboxReplyTest, AckWithZeroEchoCompletesTheCapturedCall)
{
	ASSERT_NO_FATAL_FAILURE(StartAck());
	const auto request = gAckRequests.back();
	auto reply = AckReply(request);
	ClearEcho(reply);
	PlayerInboxSystem::OnAckReply(kPlayerId, kDomain, request.correlation_id(), reply);
	EXPECT_EQ(PlayerInboxPhase::kIdle, State().phase);
	EXPECT_EQ(0u, State().correlationId);
	EXPECT_EQ(0u, State().inflightSinceMs);
	EXPECT_TRUE(State().ackQueue.empty());
	EXPECT_FALSE(State().halted);
}

TEST_F(PlayerInboxReplyTest, FetchReplyAndFailureFromTimedOutCallLeaveTheRetryInFlight)
{
	const auto oldRequest = gFetchRequests.back();
	gNowMs += InflightTimeoutMs() + 1;
	PlayerInboxSystem::Tick();
	ASSERT_EQ(PlayerInboxPhase::kIdle, State().phase);
	ASSERT_GT(State().nextCheckMs, gNowMs);
	gNowMs = State().nextCheckMs;
	PlayerInboxSystem::Tick();
	ASSERT_EQ(2u, gFetchRequests.size());
	const uint64_t currentId = gFetchRequests.back().correlation_id();
	ASSERT_NE(oldRequest.correlation_id(), currentId);
	PlayerInboxSystem::OnFetchReply(kPlayerId, kDomain, oldRequest.correlation_id(), FetchReply(oldRequest));
	PlayerInboxSystem::OnFetchFailed(kPlayerId, kDomain, oldRequest.correlation_id(), kGrpcStatusUnavailable, "迟到失败");
	EXPECT_EQ(PlayerInboxPhase::kFetching, State().phase);
	EXPECT_EQ(currentId, State().correlationId);
	EXPECT_EQ(gNowMs, State().inflightSinceMs);
}

TEST_F(PlayerInboxReplyTest, AckReplyAndFailureFromTimedOutCallLeaveTheRetryInFlight)
{
	ASSERT_NO_FATAL_FAILURE(StartAck());
	const auto oldRequest = gAckRequests.back();
	gNowMs += InflightTimeoutMs() + 1;
	PlayerInboxSystem::Tick();
	ASSERT_EQ(PlayerInboxPhase::kAwaitDurable, State().phase);
	ASSERT_GT(State().nextAckMs, gNowMs);
	gNowMs = State().nextAckMs;
	PlayerInboxSystem::Tick();
	ASSERT_EQ(2u, gAckRequests.size());
	const uint64_t currentId = gAckRequests.back().correlation_id();
	ASSERT_NE(oldRequest.correlation_id(), currentId);
	PlayerInboxSystem::OnAckReply(kPlayerId, kDomain, oldRequest.correlation_id(), AckReply(oldRequest));
	PlayerInboxSystem::OnAckFailed(kPlayerId, kDomain, oldRequest.correlation_id(), kGrpcStatusUnavailable, "迟到失败");
	EXPECT_EQ(PlayerInboxPhase::kAcking, State().phase);
	EXPECT_EQ(currentId, State().correlationId);
	EXPECT_EQ(1u, State().ackQueue.size());
}

TEST_F(PlayerInboxReplyTest, RecreatedPlayerDiscardsThePreviousEntityCall)
{
	const auto oldRequest = gFetchRequests.back();
	tlsEcs.playerList.erase(kPlayerId);
	tlsEcs.actorRegistry.destroy(player_);
	CreateLoadedPlayer();
	PlayerInboxSystem::OnPlayerLanded(player_);
	PlayerInboxSystem::Tick();
	ASSERT_EQ(2u, gFetchRequests.size());
	const uint64_t currentId = gFetchRequests.back().correlation_id();
	ASSERT_NE(oldRequest.correlation_id(), currentId);
	auto reply = FetchReply(oldRequest);
	ClearEcho(reply);
	PlayerInboxSystem::OnFetchReply(kPlayerId, kDomain, oldRequest.correlation_id(), reply);
	EXPECT_EQ(PlayerInboxPhase::kFetching, State().phase);
	EXPECT_EQ(currentId, State().correlationId);
}

TEST_F(PlayerInboxReplyTest, FetchFailureClearsTheCallAndRetriesAfterBackoff)
{
	const auto request = gFetchRequests.back();
	PlayerInboxSystem::OnFetchFailed(kPlayerId, kDomain, request.correlation_id(), kGrpcStatusUnavailable, "本地不可用");
	EXPECT_EQ(PlayerInboxPhase::kIdle, State().phase);
	EXPECT_EQ(0u, State().correlationId);
	EXPECT_EQ(0u, State().inflightSinceMs);
	EXPECT_EQ(PlayerInboxDeferReason::kTransport, State().deferReason);
	ASSERT_GE(State().nextCheckMs, gNowMs + player_inbox_rules::kBackoffMinMs);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, gFetchRequests.size());
	gNowMs = State().nextCheckMs;
	PlayerInboxSystem::Tick();
	EXPECT_EQ(2u, gFetchRequests.size());
	EXPECT_EQ(PlayerInboxPhase::kFetching, State().phase);
}

TEST_F(PlayerInboxReplyTest, AckFailureKeepsDurableDecisionsAndRetriesAfterBackoff)
{
	ASSERT_NO_FATAL_FAILURE(StartAck());
	const auto request = gAckRequests.back();
	PlayerInboxSystem::OnAckFailed(kPlayerId, kDomain, request.correlation_id(), kGrpcStatusUnavailable, "本地不可用");
	EXPECT_EQ(PlayerInboxPhase::kAwaitDurable, State().phase);
	EXPECT_EQ(0u, State().correlationId);
	EXPECT_EQ(0u, State().inflightSinceMs);
	EXPECT_EQ(1u, State().ackQueue.size());
	EXPECT_EQ(1u, State().ackFailStreak);
	ASSERT_GE(State().nextAckMs, gNowMs + player_inbox_rules::kBackoffMinMs);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, gAckRequests.size());
	gNowMs = State().nextAckMs;
	PlayerInboxSystem::Tick();
	ASSERT_EQ(2u, gAckRequests.size());
	EXPECT_EQ(request.decisions(0).SerializeAsString(), gAckRequests.back().decisions(0).SerializeAsString());
	EXPECT_EQ(PlayerInboxPhase::kAcking, State().phase);
}

TEST_F(PlayerInboxReplyTest, FetchUnimplementedUsesTheLongBackoff)
{
	const auto request = gFetchRequests.back();
	PlayerInboxSystem::OnFetchFailed(kPlayerId, kDomain, request.correlation_id(), kGrpcStatusUnimplemented, "旧版本");
	EXPECT_EQ(PlayerInboxDeferReason::kUnimplemented, State().deferReason);
	EXPECT_EQ(gNowMs + player_inbox_rules::kUnimplementedBackoffMs, State().nextCheckMs);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, gFetchRequests.size());
}

TEST_F(PlayerInboxReplyTest, AckUnimplementedKeepsDecisionsDuringTheLongBackoff)
{
	ASSERT_NO_FATAL_FAILURE(StartAck());
	const auto request = gAckRequests.back();
	PlayerInboxSystem::OnAckFailed(kPlayerId, kDomain, request.correlation_id(), kGrpcStatusUnimplemented, "旧版本");
	EXPECT_EQ(PlayerInboxPhase::kAwaitDurable, State().phase);
	EXPECT_EQ(1u, State().ackQueue.size());
	EXPECT_EQ(gNowMs + player_inbox_rules::kUnimplementedBackoffMs, State().nextAckMs);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, gAckRequests.size());
}

TEST_F(PlayerInboxReplyTest, CapturedFetchStillHonorsEpochHalt)
{
	const auto request = gFetchRequests.back();
	auto reply = FetchReply(request);
	ClearEcho(reply);
	reply.set_fence(player_inbox::PLAYER_INBOX_FENCE_EPOCH_AHEAD);
	PlayerInboxSystem::OnFetchReply(kPlayerId, kDomain, request.correlation_id(), reply);
	EXPECT_EQ(PlayerInboxPhase::kIdle, State().phase);
	EXPECT_TRUE(State().halted);
	EXPECT_EQ(PlayerInboxHaltReason::kEpochAhead, State().haltReason);
	gNowMs += player_inbox_rules::kBackoffMaxMs;
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, gFetchRequests.size());
}

TEST_F(PlayerInboxReplyTest, CapturedAckStillHonorsProtocolHalt)
{
	ASSERT_NO_FATAL_FAILURE(StartAck());
	const auto request = gAckRequests.back();
	auto reply = AckReply(request);
	ClearEcho(reply);
	reply.mutable_results(0)->set_result(player_inbox::PLAYER_INBOX_ACK_CONFLICT);
	PlayerInboxSystem::OnAckReply(kPlayerId, kDomain, request.correlation_id(), reply);
	EXPECT_EQ(PlayerInboxPhase::kIdle, State().phase);
	EXPECT_TRUE(State().halted);
	EXPECT_EQ(PlayerInboxHaltReason::kAckAnomaly, State().haltReason);
	EXPECT_TRUE(State().ackQueue.empty());
}

TEST_F(PlayerInboxReplyTest, CapturedFetchStillBacksOffOnServerTip)
{
	const auto request = gFetchRequests.back();
	auto reply = FetchReply(request);
	ClearEcho(reply);
	reply.mutable_error_message()->set_id(kPlayerInboxStoreUnavailable);
	PlayerInboxSystem::OnFetchReply(kPlayerId, kDomain, request.correlation_id(), reply);
	EXPECT_EQ(PlayerInboxPhase::kIdle, State().phase);
	EXPECT_EQ(PlayerInboxDeferReason::kServerError, State().deferReason);
	EXPECT_FALSE(State().halted);
	ASSERT_GT(State().nextCheckMs, gNowMs);
	PlayerInboxSystem::Tick();
	EXPECT_EQ(1u, gFetchRequests.size());
}
} // namespace
