// 观战订阅中心 SpectateHub 的单测(spectate/gate_spectate_hub.h,docs/design/battle-spectate-tiered-fanout.md §15.5)。
//
// 不进 gate.vcxproj,登记在 cpp/tests/routing_identity_test/routing_identity_test.vcxproj。
// 发送函数、续发队列与时钟全部注入:不开 EventLoop、不用真实时钟,续发由用例手动推进,结果不依赖执行顺序。
// 订阅者的遍历顺序是哈希表决定的,所以用例只断言「哪些会话收到什么」,不断言分批里具体是谁。

#include <gtest/gtest.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <deque>
#include <functional>
#include <map>
#include <memory>
#include <set>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include "handler/gate_push_delivery.h"
#include "muduo/base/Logging.h"
#include "node/system/node/node_kafka_command_filter.h"
#include "proto/battle/player_battle.pb.h"
#include "proto/common/base/message.pb.h"
#include "proto/contracts/kafka/gate_event.pb.h"
#include "proto/gate/gate_spectate_ticket.pb.h"
#include "rpc/service_metadata/player_battle_service_metadata.h"
#include "spectate/gate_spectate_hub.h"

namespace
{
	using contracts::kafka::SpectateFrameEvent;
	using gate_spectate::FrameDelivery;
	using gate_spectate::HubLimits;
	using gate_spectate::HubPorts;
	using gate_spectate::SendResult;
	using gate_spectate::SpectateHub;
	using gate_spectate::SteadyTime;
	using std::chrono::seconds;

	constexpr uint64_t kBattle = 9001;
	constexpr uint64_t kOtherBattle = 9002;
	const std::string kInstance = "battle-instance-a";
	const std::string kOtherInstance = "battle-instance-b";
	constexpr uint64_t kDeadlineMs = 5'000'000;
	constexpr uint64_t kWallNowMs = 1'000'000; // 远早于 kDeadlineMs:只测空闲的用例不受期限影响

	constexpr uint32_t kStateId = BattleClientPlayerNotifySpectateStateMessageId;
	constexpr uint32_t kTurnId = BattleClientPlayerNotifySpectateTurnResultMessageId;
	constexpr uint32_t kEndId = BattleClientPlayerNotifySpectateEndMessageId;

	// 帧是怎么发出去的:解码回 MessageContent 核对内容,bytesAddress 用来判断是否复用了同一份编码。
	struct SentFrame
	{
		SessionId session{0};
		MessageContent content;
		std::uintptr_t bytesAddress{0};
		FrameDelivery delivery{FrameDelivery::kBestEffort};
	};

	MessageContent DecodeFrame(const gate_push::EncodedFrame &frame)
	{
		constexpr std::size_t kLengthPrefix = sizeof(int32_t);
		ProtobufCodec::ErrorCode error = ProtobufCodec::kNoError;
		const MessagePtr message =
			ProtobufCodec::parse(frame.data() + kLengthPrefix, static_cast<int>(frame.size() - kLengthPrefix), &error);
		EXPECT_EQ(error, ProtobufCodec::kNoError);
		const auto content = std::dynamic_pointer_cast<MessageContent>(message);
		EXPECT_NE(content, nullptr);
		return content != nullptr ? *content : MessageContent{};
	}

	class HubHarness
	{
	public:
		explicit HubHarness(HubLimits limits = {})
		{
			HubPorts ports;
			ports.sendFrame = [this](SessionId session, const gate_push::EncodedFrame &frame, FrameDelivery delivery)
			{
				if (gone.contains(session))
				{
					return SendResult::kSessionGone;
				}
				if (delivery == FrameDelivery::kBestEffort && bufferFull.contains(session))
				{
					return SendResult::kBufferFull;
				}
				sent.push_back(SentFrame{session, DecodeFrame(frame), reinterpret_cast<std::uintptr_t>(frame.data()), delivery});
				return SendResult::kSent;
			};
			ports.defer = [this](std::function<void()> task) { deferred.push_back(std::move(task)); };
			ports.steadyNow = [this] { return now; };
			hub = std::make_shared<SpectateHub>(std::move(ports), limits);
		}

		HubHarness(const HubHarness &) = delete;
		HubHarness &operator=(const HubHarness &) = delete;

		void RunOneDeferred()
		{
			ASSERT_FALSE(deferred.empty());
			auto task = std::move(deferred.front());
			deferred.pop_front();
			task();
		}

		void RunAllDeferred()
		{
			while (!deferred.empty())
			{
				RunOneDeferred();
			}
		}

		// 把会话订上并收完首帧(前提是条目已有缓存),之后 sent 从空开始。
		void SubscribeAndDrainFirstFrames(const std::vector<SessionId> &sessions)
		{
			for (const SessionId session : sessions)
			{
				hub->Subscribe(session, MakeTicket());
			}
			sent.clear();
		}

		static SpectateTicketPayload MakeTicket(uint64_t battleId = kBattle, const std::string &instance = kInstance)
		{
			SpectateTicketPayload ticket;
			ticket.set_battle_id(battleId);
			ticket.set_player_id(42);
			ticket.set_battle_instance_id(instance);
			ticket.set_deadline_ms(kDeadlineMs);
			ticket.set_expire_at_ms(kWallNowMs + 60'000);
			return ticket;
		}

		std::vector<SentFrame> sent;
		std::deque<std::function<void()>> deferred;
		std::set<SessionId> bufferFull;
		std::set<SessionId> gone;
		SteadyTime now{seconds(1000)};
		std::shared_ptr<SpectateHub> hub;
	};

	SpectateFrameEvent MakeFrame(contracts::kafka::eSpectateFrameKind kind, uint32_t round, uint32_t messageId,
								 const std::string &body, uint64_t battleId, const std::string &instance)
	{
		MessageContent content;
		content.set_message_id(messageId);
		content.set_serialized_message(body);

		SpectateFrameEvent event;
		event.set_battle_id(battleId);
		event.set_battle_instance_id(instance);
		event.set_round_index(round);
		event.set_kind(kind);
		event.set_client_frame(content.SerializeAsString());
		event.set_deadline_ms(kDeadlineMs);
		return event;
	}

	SpectateFrameEvent StateFrame(uint32_t round, uint64_t battleId = kBattle, const std::string &instance = kInstance)
	{
		SpectateStateS2C state;
		state.mutable_state()->set_battle_id(battleId);
		state.mutable_state()->set_round_index(round);
		state.set_observer_count(7); // 用来区分「原样转发」与「合成」:合成的恒为 0
		return MakeFrame(contracts::kafka::SPECTATE_FRAME_STATE, round, kStateId, state.SerializeAsString(), battleId,
						 instance);
	}

	SpectateFrameEvent TurnFrame(uint32_t round, uint64_t battleId = kBattle, const std::string &instance = kInstance)
	{
		TurnResultS2C turn;
		turn.set_battle_id(battleId);
		turn.set_round_index(round);
		turn.mutable_state()->set_battle_id(battleId);
		turn.mutable_state()->set_round_index(round);
		return MakeFrame(contracts::kafka::SPECTATE_FRAME_TURN, round, kTurnId, turn.SerializeAsString(), battleId,
						 instance);
	}

	SpectateFrameEvent EndFrame(uint32_t round, uint64_t battleId = kBattle, const std::string &instance = kInstance)
	{
		SpectateEndS2C end;
		end.set_battle_id(battleId);
		end.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN);
		end.set_reason(SPECTATE_END_BATTLE_FINISHED);
		return MakeFrame(contracts::kafka::SPECTATE_FRAME_END, round, kEndId, end.SerializeAsString(), battleId,
						 instance);
	}

	// 回合号:161 取 state.round_index,158 取 round_index。
	uint32_t RoundOf(const MessageContent &content)
	{
		if (content.message_id() == kStateId)
		{
			SpectateStateS2C state;
			EXPECT_TRUE(state.ParseFromString(content.serialized_message()));
			return state.state().round_index();
		}
		TurnResultS2C turn;
		EXPECT_EQ(content.message_id(), kTurnId);
		EXPECT_TRUE(turn.ParseFromString(content.serialized_message()));
		return turn.round_index();
	}

	std::set<SessionId> SessionsOf(const std::vector<SentFrame> &frames)
	{
		std::set<SessionId> sessions;
		for (const auto &frame : frames)
		{
			sessions.insert(frame.session);
		}
		return sessions;
	}

	// 每个会话依次收到的消息号。
	std::map<SessionId, std::vector<uint32_t>> MessageIdsBySession(const std::vector<SentFrame> &frames)
	{
		std::map<SessionId, std::vector<uint32_t>> messageIds;
		for (const auto &frame : frames)
		{
			messageIds[frame.session].push_back(frame.content.message_id());
		}
		return messageIds;
	}

	HubLimits LimitsWithBatch(std::size_t batch)
	{
		HubLimits limits;
		limits.fanoutBatch = batch;
		return limits;
	}
} // namespace

// ---- 订阅 ----

TEST(SpectateHub, NewSubscriptionReplacesTheOldOne)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(1));
	h.hub->OnFrame(StateFrame(1, kOtherBattle));
	h.hub->Subscribe(1, HubHarness::MakeTicket());
	h.hub->Subscribe(1, HubHarness::MakeTicket(kOtherBattle));
	EXPECT_EQ(h.hub->SubscriberCount(), 1u);
	h.sent.clear();

	h.hub->OnFrame(TurnFrame(2)); // 旧场的帧不再发给它
	EXPECT_TRUE(h.sent.empty());

	h.hub->OnFrame(TurnFrame(2, kOtherBattle));
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(h.sent[0].session, 1u);
	EXPECT_EQ(h.sent[0].content.message_id(), kTurnId);
}

TEST(SpectateHub, UnsubscribeOnlyAffectsTheCurrentBattle)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1});

	h.hub->Unsubscribe(1, kOtherBattle); // 与当前订阅不符:什么都不做
	EXPECT_EQ(h.hub->SubscriberCount(), 1u);

	h.hub->Unsubscribe(1, kBattle);
	EXPECT_EQ(h.hub->SubscriberCount(), 0u);
	h.hub->OnFrame(TurnFrame(2));
	EXPECT_TRUE(h.sent.empty());

	h.hub->Unsubscribe(1, kBattle); // 重复退订幂等
	EXPECT_EQ(h.hub->SubscriberCount(), 0u);
}

TEST(SpectateHub, ClosedSessionStopsReceivingFrames)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2});

	h.hub->OnSessionClosed(1);
	EXPECT_EQ(h.hub->SubscriberCount(), 1u);

	h.hub->OnFrame(TurnFrame(2));
	EXPECT_EQ(SessionsOf(h.sent), (std::set<SessionId>{2}));
}

TEST(SpectateHub, SessionGoneAtSendTimeIsDetached)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2});

	h.gone.insert(1);
	h.hub->OnFrame(TurnFrame(2));
	EXPECT_EQ(SessionsOf(h.sent), (std::set<SessionId>{2}));
	EXPECT_EQ(h.hub->SubscriberCount(), 1u);
}

// 回选角(LeaveGame,TCP 不断):gate_scene_entry::OnClientLeaveGame 经 OnSessionClosed 走到这里。连接还在,所以摘订阅不能
// 发帧、不能把会话当成没了;之后这个会话收不到帧(含正在进行的一轮),再订阅时从 161 重新开始。接线那一行没有单测夹具。
TEST(SpectateHub, SessionLeavingTheGameStopsReceivingAndCanSubscribeAgain)
{
	HubHarness h(LimitsWithBatch(1));
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2});

	h.hub->OnFrame(TurnFrame(2)); // 发出 1 个,另一个排在续发里
	ASSERT_EQ(h.sent.size(), 1u);
	const SessionId served = h.sent[0].session;
	const SessionId leaving = served == 1 ? 2 : 1;

	h.hub->OnSessionClosed(leaving);
	h.RunAllDeferred();
	EXPECT_EQ(h.sent.size(), 1u) << "摘订阅不发帧,排着的那一帧也不再发给它";
	EXPECT_EQ(h.hub->SubscriberCount(), 1u);

	h.hub->OnFrame(TurnFrame(3));
	EXPECT_EQ(SessionsOf(h.sent), (std::set<SessionId>{served}));

	h.sent.clear();
	h.hub->Subscribe(leaving, HubHarness::MakeTicket());
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(h.sent[0].session, leaving);
	EXPECT_EQ(h.sent[0].content.message_id(), kStateId);
	EXPECT_EQ(h.hub->SubscriberCount(), 2u);
}

// ---- 首帧(D69:收到 161 才算进入观战)----

TEST(SpectateHub, CachedStateFrameIsSentAsIs)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(3));
	h.hub->Subscribe(1, HubHarness::MakeTicket());

	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(h.sent[0].content.message_id(), kStateId);
	SpectateStateS2C state;
	ASSERT_TRUE(state.ParseFromString(h.sent[0].content.serialized_message()));
	EXPECT_EQ(state.state().round_index(), 3u);
	EXPECT_EQ(state.observer_count(), 7u) << "缓存是 161 时必须原样发,不重新合成";
}

TEST(SpectateHub, CachedTurnFrameIsSynthesizedIntoAStateFrame)
{
	HubHarness h;
	h.hub->OnFrame(TurnFrame(5)); // 没有订阅者也建条目缓存
	h.hub->Subscribe(1, HubHarness::MakeTicket());

	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(h.sent[0].content.message_id(), kStateId);
	SpectateStateS2C state;
	ASSERT_TRUE(state.ParseFromString(h.sent[0].content.serialized_message()));
	EXPECT_EQ(state.state().round_index(), 5u);
	EXPECT_EQ(state.state().battle_id(), kBattle);
	EXPECT_EQ(state.observer_count(), 0u) << "LOBBY 档 0 表示人数未知";
}

TEST(SpectateHub, FirstFrameIsSynthesizedOncePerRoundNoMatterHowManySubscribe)
{
	HubHarness h;
	h.hub->OnFrame(TurnFrame(5));
	h.hub->Subscribe(1, HubHarness::MakeTicket());
	h.hub->Subscribe(2, HubHarness::MakeTicket());
	ASSERT_EQ(h.sent.size(), 2u);
	EXPECT_EQ(h.sent[0].bytesAddress, h.sent[1].bytesAddress) << "同一轮的首帧应复用同一份编码";
}

TEST(SpectateHub, SubscriberWithoutCacheGetsTheNextStateFrame)
{
	HubHarness h;
	h.hub->Subscribe(1, HubHarness::MakeTicket());
	EXPECT_TRUE(h.sent.empty()) << "没有缓存:等下一帧";

	h.hub->OnFrame(StateFrame(1)); // 中继挂接时 battle 补发的那一帧
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(h.sent[0].content.message_id(), kStateId);
}

TEST(SpectateHub, SubscriberWithoutCacheGetsAStateFrameEvenIfTheNextFrameIsATurn)
{
	HubHarness h;
	h.hub->Subscribe(1, HubHarness::MakeTicket());

	h.hub->OnFrame(TurnFrame(2));
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(h.sent[0].content.message_id(), kStateId) << "第一帧必须是 161,不能是 158";
	EXPECT_EQ(RoundOf(h.sent[0].content), 2u);

	h.hub->OnFrame(TurnFrame(3));
	ASSERT_EQ(h.sent.size(), 2u);
	EXPECT_EQ(h.sent[1].content.message_id(), kTurnId);
}

// ---- 收帧规则 ----

TEST(SpectateHub, TurnFramesAreMonotonic)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1});

	h.hub->OnFrame(TurnFrame(2));
	h.hub->OnFrame(TurnFrame(2)); // Kafka 重复投递 / 升档时中继与广播重叠
	h.hub->OnFrame(TurnFrame(1)); // 乱序的旧帧
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(RoundOf(h.sent[0].content), 2u);

	h.hub->OnFrame(TurnFrame(3));
	ASSERT_EQ(h.sent.size(), 2u);
	EXPECT_EQ(RoundOf(h.sent[1].content), 3u);
}

TEST(SpectateHub, StateFrameOnlyUpdatesTheCacheForSessionsAlreadyWatching)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1});

	h.hub->OnFrame(StateFrame(1)); // >= cachedStateRound 收下
	h.hub->OnFrame(StateFrame(4));
	EXPECT_TRUE(h.sent.empty()) << "已在看的会话不收 STATE";

	h.hub->Subscribe(2, HubHarness::MakeTicket());
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(RoundOf(h.sent[0].content), 4u) << "新订阅者拿到更新后的缓存";
}

// 回归:挂接 / 升档时的 STATE(r) 是第 r 回合结算前的快照,不能吃掉紧接着的 TURN(r);
// 否则每次挂接少播一回合,发生在最后一回合时观众看不到终局那回合的结算。
TEST(SpectateHub, TurnOfTheSnapshotRoundIsStillFannedOut)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(5));
	h.SubscribeAndDrainFirstFrames({1});

	h.hub->OnFrame(TurnFrame(5));
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(h.sent[0].content.message_id(), kTurnId);
	EXPECT_EQ(RoundOf(h.sent[0].content), 5u);
}

// 第 r 回合结算之后才到的 STATE(r)(升档时两条 topic 没有相对顺序)是过期快照,不能换掉缓存里更新的状态。
TEST(SpectateHub, SnapshotOfAnAlreadySettledRoundDoesNotReplaceTheCache)
{
	HubHarness h;
	h.hub->OnFrame(TurnFrame(5));
	h.hub->OnFrame(StateFrame(5));
	h.hub->Subscribe(1, HubHarness::MakeTicket());
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(h.sent[0].content.message_id(), kStateId);
	SpectateStateS2C first;
	ASSERT_TRUE(first.ParseFromString(h.sent[0].content.serialized_message()));
	EXPECT_EQ(first.observer_count(), 0u) << "首帧来自 TURN(5) 合成,不是过期的 STATE(5) 原样转发(夹具里它的 observer_count 是 7)";
}

// 中继帧晚于升档后的广播帧到达:观众还没看过这回合,照常扇出;但新订阅者的首帧仍取更新的快照。
TEST(SpectateHub, LateTurnAfterANewerSnapshotIsFannedOutWithoutRollingBackTheCache)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(3));
	h.SubscribeAndDrainFirstFrames({1});

	h.hub->OnFrame(StateFrame(6));
	h.hub->OnFrame(TurnFrame(4));
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(RoundOf(h.sent[0].content), 4u);
	h.sent.clear();

	h.hub->Subscribe(2, HubHarness::MakeTicket());
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(RoundOf(h.sent[0].content), 6u);
}

TEST(SpectateHub, StateFrameOlderThanTheLastRoundIsIgnored)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(4));
	h.hub->OnFrame(StateFrame(3));
	h.hub->Subscribe(1, HubHarness::MakeTicket());
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(RoundOf(h.sent[0].content), 4u);
}

TEST(SpectateHub, FrameFromAnotherInstanceIsDropped)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1});

	h.hub->OnFrame(TurnFrame(2, kBattle, kOtherInstance));
	EXPECT_TRUE(h.sent.empty());
	EXPECT_EQ(h.hub->Counters().instanceMismatch, 1u);

	h.hub->OnFrame(TurnFrame(2)); // 被丢的帧不占回合号
	EXPECT_EQ(h.sent.size(), 1u);
}

TEST(SpectateHub, TicketInstanceWinsAndClearsTheCache)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(9)); // 条目来自旧实例(比如僵尸广播)
	h.hub->Subscribe(1, HubHarness::MakeTicket(kBattle, kOtherInstance));
	EXPECT_TRUE(h.sent.empty()) << "旧实例的缓存不能发给持新实例凭证的会话";

	h.hub->OnFrame(TurnFrame(10)); // 旧实例的帧被丢弃
	EXPECT_TRUE(h.sent.empty());

	h.hub->OnFrame(StateFrame(1, kBattle, kOtherInstance)); // 回合号从新实例重新算
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(RoundOf(h.sent[0].content), 1u);
}

TEST(SpectateHub, InvalidFramesAreRejectedWithoutCreatingAnEntry)
{
	HubHarness h;

	SpectateFrameEvent noKind = TurnFrame(1);
	noKind.set_kind(contracts::kafka::SPECTATE_FRAME_NONE);
	h.hub->OnFrame(noKind);

	SpectateFrameEvent wrongMessageId = TurnFrame(1);
	wrongMessageId.set_kind(contracts::kafka::SPECTATE_FRAME_STATE); // 消息体是 158,种类却写成 STATE
	h.hub->OnFrame(wrongMessageId);

	SpectateFrameEvent badBody = TurnFrame(1);
	badBody.set_client_frame(std::string("\xff\xff\xff", 3));
	h.hub->OnFrame(badBody);

	SpectateFrameEvent noBattle = TurnFrame(1);
	noBattle.set_battle_id(0);
	h.hub->OnFrame(noBattle);

	SpectateFrameEvent noInstance = TurnFrame(1);
	noInstance.clear_battle_instance_id();
	h.hub->OnFrame(noInstance);

	EXPECT_EQ(h.hub->Counters().invalidFrames, 5u);
	EXPECT_EQ(h.hub->EntryCount(), 0u);
}

// ---- 终局与墓碑 ----

TEST(SpectateHub, EndFansOutOnceAndTombstoneAnswersLateSubscribers)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2});

	h.hub->OnFrame(EndFrame(3));
	ASSERT_EQ(h.sent.size(), 2u);
	EXPECT_EQ(SessionsOf(h.sent), (std::set<SessionId>{1, 2}));
	for (const auto &frame : h.sent)
	{
		EXPECT_EQ(frame.content.message_id(), kEndId);
	}
	EXPECT_EQ(h.hub->SubscriberCount(), 0u) << "END 结束该场的全部订阅";
	EXPECT_EQ(h.hub->EntryCount(), 1u) << "条目留作墓碑";

	h.sent.clear();
	h.hub->OnFrame(EndFrame(3)); // END 只收一次
	h.hub->OnFrame(TurnFrame(9)); // 终局之后的帧也不再处理
	EXPECT_TRUE(h.sent.empty());

	h.hub->Subscribe(3, HubHarness::MakeTicket());
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(h.sent[0].session, 3u);
	EXPECT_EQ(h.sent[0].content.message_id(), kEndId);
	EXPECT_EQ(h.hub->SubscriberCount(), 0u) << "墓碑期内不建立订阅";
}

TEST(SpectateHub, TombstoneExpiresAfterTheRetentionWindow)
{
	HubHarness h;
	h.hub->OnFrame(EndFrame(3));
	EXPECT_EQ(h.hub->EntryCount(), 1u);

	h.hub->SweepIdle(h.now + seconds(599), kWallNowMs);
	EXPECT_EQ(h.hub->EntryCount(), 1u);

	h.hub->SweepIdle(h.now + seconds(600), kWallNowMs);
	EXPECT_EQ(h.hub->EntryCount(), 0u);
}

// ---- 没有订阅者的条目 ----

TEST(SpectateHub, EntryWithoutSubscribersIsRemovedAfterTheIdleTimeout)
{
	HubHarness h;
	h.hub->OnFrame(TurnFrame(1));
	EXPECT_EQ(h.hub->EntryCount(), 1u);

	h.hub->SweepIdle(h.now + seconds(29), kWallNowMs);
	EXPECT_EQ(h.hub->EntryCount(), 1u);

	h.hub->SweepIdle(h.now + seconds(30), kWallNowMs);
	EXPECT_EQ(h.hub->EntryCount(), 0u);
	EXPECT_TRUE(h.sent.empty());
	EXPECT_EQ(h.hub->Counters().sourceLost, 0u) << "没人看就没有人需要收尾";
}

TEST(SpectateHub, EntryOutlivesItsLastSubscriberAndKeepsCaching)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1});
	h.hub->Unsubscribe(1, kBattle);

	h.hub->OnFrame(TurnFrame(2));
	h.hub->Subscribe(2, HubHarness::MakeTicket());
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(RoundOf(h.sent[0].content), 2u);
}

// ---- 源站失联 ----

TEST(SpectateHub, IdleSubscribersGetSourceLostAndTheEntryIsDeleted)
{
	HubHarness h;
	h.hub->Subscribe(1, HubHarness::MakeTicket());

	h.hub->SweepIdle(h.now + seconds(29), kWallNowMs);
	EXPECT_TRUE(h.sent.empty());

	h.hub->SweepIdle(h.now + seconds(30), kWallNowMs);
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(h.sent[0].content.message_id(), kEndId);
	SpectateEndS2C end;
	ASSERT_TRUE(end.ParseFromString(h.sent[0].content.serialized_message()));
	EXPECT_EQ(end.battle_id(), kBattle);
	EXPECT_EQ(end.reason(), SPECTATE_END_SOURCE_LOST);
	EXPECT_EQ(end.outcome(), BATTLE_OUTCOME_ONGOING);

	EXPECT_EQ(h.hub->SubscriberCount(), 0u);
	EXPECT_EQ(h.hub->EntryCount(), 0u) << "失联不留墓碑:battle 可能还活着";
	EXPECT_EQ(h.hub->Counters().sourceLost, 1u);
}

TEST(SpectateHub, FreshFramesKeepTheEntryAlive)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1});

	h.now += seconds(20);
	h.hub->OnFrame(TurnFrame(2));
	h.sent.clear();

	h.hub->SweepIdle(h.now + seconds(20), kWallNowMs); // 距开始 40s,距最后一帧 20s
	EXPECT_TRUE(h.sent.empty());
	EXPECT_EQ(h.hub->EntryCount(), 1u);
}

TEST(SpectateHub, DuplicateFramesStillCountAsSourceLiveness)
{
	HubHarness h;
	h.hub->OnFrame(TurnFrame(2));
	h.SubscribeAndDrainFirstFrames({1});

	h.now += seconds(20);
	h.hub->OnFrame(TurnFrame(2)); // 被去重,但说明源站活着
	h.hub->SweepIdle(h.now + seconds(20), kWallNowMs);
	EXPECT_TRUE(h.sent.empty());
	EXPECT_EQ(h.hub->EntryCount(), 1u);
}

TEST(SpectateHub, SubscribersPastTheBattleDeadlineGetSourceLost)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1});

	h.hub->SweepIdle(h.now, kDeadlineMs + 60'000); // 还在宽限内
	EXPECT_TRUE(h.sent.empty());

	h.hub->SweepIdle(h.now, kDeadlineMs + 60'001);
	ASSERT_EQ(h.sent.size(), 1u);
	SpectateEndS2C end;
	ASSERT_TRUE(end.ParseFromString(h.sent[0].content.serialized_message()));
	EXPECT_EQ(end.reason(), SPECTATE_END_SOURCE_LOST);
	EXPECT_EQ(h.hub->EntryCount(), 0u);
}

TEST(SpectateHub, FramesAfterSourceLostRebuildTheEntry)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1});
	h.hub->SweepIdle(h.now + seconds(30), kWallNowMs);
	EXPECT_EQ(h.hub->EntryCount(), 0u);
	h.sent.clear();

	h.hub->OnFrame(TurnFrame(2)); // 只是暂时失联,battle 回来了
	h.hub->Subscribe(2, HubHarness::MakeTicket());
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(h.sent[0].content.message_id(), kStateId);
}

// ---- 扇出:分批、编码一次、合并 ----

TEST(SpectateHub, FanoutIsBatchedAndEncodedOnce)
{
	HubHarness h(LimitsWithBatch(2));
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2, 3, 4, 5});

	h.hub->OnFrame(TurnFrame(2));
	EXPECT_EQ(h.sent.size(), 2u);
	ASSERT_EQ(h.deferred.size(), 1u);

	h.RunOneDeferred();
	EXPECT_EQ(h.sent.size(), 4u);
	ASSERT_EQ(h.deferred.size(), 1u);

	h.RunOneDeferred();
	EXPECT_EQ(h.sent.size(), 5u);
	EXPECT_TRUE(h.deferred.empty());

	EXPECT_EQ(SessionsOf(h.sent), (std::set<SessionId>{1, 2, 3, 4, 5}));
	for (const auto &frame : h.sent)
	{
		EXPECT_EQ(frame.bytesAddress, h.sent[0].bytesAddress) << "同一帧只编码一次,各会话复用同一份字节";
		EXPECT_EQ(frame.delivery, FrameDelivery::kBestEffort);
	}
	EXPECT_EQ(h.hub->Counters().framesFannedOut, 1u);
	EXPECT_EQ(h.hub->Counters().coalesced, 0u);
}

TEST(SpectateHub, NewFrameTakesOverTheUnfinishedFanout)
{
	HubHarness h(LimitsWithBatch(2));
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2, 3, 4, 5});

	h.hub->OnFrame(TurnFrame(2)); // 发出 2 个,余 3 个
	ASSERT_EQ(h.sent.size(), 2u);
	h.hub->OnFrame(TurnFrame(3)); // 余下的 3 个直接改发新帧
	EXPECT_EQ(h.hub->Counters().coalesced, 3u);
	EXPECT_LE(h.deferred.size(), 1u) << "至多排一个续发";
	h.RunAllDeferred();

	std::vector<SessionId> round2;
	std::vector<SessionId> round3;
	for (const auto &frame : h.sent)
	{
		(RoundOf(frame.content) == 2 ? round2 : round3).push_back(frame.session);
	}
	EXPECT_EQ(round2.size(), 2u);
	std::sort(round3.begin(), round3.end());
	EXPECT_EQ(round3, (std::vector<SessionId>{1, 2, 3, 4, 5})) << "新帧发给所有订阅者,每人恰好一次";
}

// 回归:合并之后每轮都从名单头发的话,过载时表头帧帧都收到、表尾一帧也收不到。每轮只来得及发一批时,大家必须轮流:
// 五帧、每帧两个目标、五个会话,恰好人人两帧。只按上一轮的 fanoutNext 旋转(不累加)过不了这条:第三轮起都回到第二轮的起点。
TEST(SpectateHub, OverloadedFanoutTakesTurnsInsteadOfStarvingTheTail)
{
	HubHarness h(LimitsWithBatch(2));
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2, 3, 4, 5});

	constexpr uint32_t kLastRound = 6;
	for (uint32_t round = 2; round <= kLastRound; ++round)
	{
		h.hub->OnFrame(TurnFrame(round)); // 续发一直没机会跑,每帧只发出第一批
	}
	const auto frames = MessageIdsBySession(h.sent);
	ASSERT_EQ(frames.size(), 5u);
	for (const auto &[session, messageIds] : frames)
	{
		EXPECT_EQ(messageIds.size(), 2u) << "session=" << session;
	}
	EXPECT_EQ(h.hub->Counters().coalesced, 12u);

	h.RunAllDeferred(); // loop 缓过来:最后一帧发给还没收到它的会话
	std::vector<SessionId> lastRound;
	for (const auto &frame : h.sent)
	{
		if (RoundOf(frame.content) == kLastRound)
		{
			lastRound.push_back(frame.session);
		}
	}
	std::sort(lastRound.begin(), lastRound.end());
	EXPECT_EQ(lastRound, (std::vector<SessionId>{1, 2, 3, 4, 5})) << "最后一帧每人恰好一次";
}

// 上一轮停下的下标可能超出变短之后的名单(这里是 4,名单只剩 2 个):取模,不能越界。
TEST(SpectateHub, TurnFanoutStartStaysInRangeWhenSubscribersLeave)
{
	HubHarness h(LimitsWithBatch(2));
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2, 3, 4, 5});

	h.hub->OnFrame(TurnFrame(2));
	h.RunOneDeferred(); // 发到第 4 个,还差 1 个
	ASSERT_EQ(h.sent.size(), 4u);
	for (const SessionId session : {3u, 4u, 5u})
	{
		h.hub->Unsubscribe(session, kBattle);
	}
	h.sent.clear();

	h.hub->OnFrame(TurnFrame(3)); // 第一批就发完;之前排着的续发已经没有要续的
	h.RunAllDeferred();
	EXPECT_EQ(h.sent.size(), 2u);
	EXPECT_EQ(SessionsOf(h.sent), (std::set<SessionId>{1, 2}));
}

TEST(SpectateHub, SessionsLeavingDuringFanoutAreSkipped)
{
	HubHarness h(LimitsWithBatch(1));
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2, 3});

	h.hub->OnFrame(TurnFrame(2));
	ASSERT_EQ(h.sent.size(), 1u);

	// 还没轮到的两个:一个退订,一个断线。
	std::vector<SessionId> pending;
	for (const SessionId session : {1u, 2u, 3u})
	{
		if (session != h.sent[0].session)
		{
			pending.push_back(session);
		}
	}
	h.hub->Unsubscribe(pending[0], kBattle);
	h.hub->OnSessionClosed(pending[1]);
	h.RunAllDeferred();
	EXPECT_EQ(h.sent.size(), 1u);
}

TEST(SpectateHub, SubscriberJoiningDuringFanoutOnlyGetsTheFirstFrame)
{
	HubHarness h(LimitsWithBatch(1));
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2});

	h.hub->OnFrame(TurnFrame(2));
	h.hub->Subscribe(3, HubHarness::MakeTicket());
	h.RunAllDeferred();

	std::vector<const SentFrame *> forNewcomer;
	for (const auto &frame : h.sent)
	{
		if (frame.session == 3)
		{
			forNewcomer.push_back(&frame);
		}
	}
	ASSERT_EQ(forNewcomer.size(), 1u);
	EXPECT_EQ(forNewcomer[0]->content.message_id(), kStateId);
	EXPECT_EQ(RoundOf(forNewcomer[0]->content), 2u);
}

TEST(SpectateHub, ContinuationAfterTheHubIsGoneDoesNothing)
{
	HubHarness h(LimitsWithBatch(1));
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2});

	h.hub->OnFrame(TurnFrame(2));
	ASSERT_EQ(h.sent.size(), 1u);
	ASSERT_EQ(h.deferred.size(), 1u);

	h.hub.reset(); // 续发闭包只持弱引用
	h.RunAllDeferred();
	EXPECT_EQ(h.sent.size(), 1u);
}

TEST(SpectateHub, StaleContinuationDoesNotTouchARebuiltEntry)
{
	HubHarness h(LimitsWithBatch(1));
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2});
	h.hub->OnFrame(TurnFrame(2));
	ASSERT_EQ(h.deferred.size(), 1u);

	// 全部退订后条目到期删除,同一场又被帧重建;排着的旧续发必须作废。
	h.hub->Unsubscribe(1, kBattle);
	h.hub->Unsubscribe(2, kBattle);
	h.hub->SweepIdle(h.now + seconds(30), kWallNowMs);
	ASSERT_EQ(h.hub->EntryCount(), 0u);
	h.hub->OnFrame(StateFrame(5));
	h.sent.clear();

	h.RunAllDeferred();
	EXPECT_TRUE(h.sent.empty());
}

TEST(SpectateHub, EndFanoutIsBatchedAndReleasesSubscribersWhenFinished)
{
	HubHarness h(LimitsWithBatch(2));
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2, 3, 4, 5});

	h.hub->OnFrame(EndFrame(2));
	EXPECT_EQ(h.sent.size(), 2u);
	EXPECT_EQ(h.hub->SubscriberCount(), 5u) << "没发完之前订阅关系还在";

	h.hub->Subscribe(6, HubHarness::MakeTicket()); // 发 END 途中来的新订阅者直接收 166
	EXPECT_EQ(h.sent.size(), 3u);
	EXPECT_EQ(h.sent.back().session, 6u);
	EXPECT_EQ(h.sent.back().content.message_id(), kEndId);

	h.RunAllDeferred();
	EXPECT_EQ(h.sent.size(), 6u);
	EXPECT_EQ(SessionsOf(h.sent), (std::set<SessionId>{1, 2, 3, 4, 5, 6}));
	EXPECT_EQ(h.hub->SubscriberCount(), 0u);
	EXPECT_EQ(h.hub->EntryCount(), 1u);
}

// 回归:battle 在同一调用栈里先后发最后一回合的 TURN 与 END,两帧几乎总在同一个 Kafka batch 里。166 不带状态也不带事件,
// END 抢占在途的 TURN 的话,没轮到的会话永远看不到最后一回合;单 gate 订阅者超过一批时每场结束都会发生。
TEST(SpectateHub, EndWaitsForTheLastTurnSoEverySessionSeesTurnThenEnd)
{
	HubHarness h(LimitsWithBatch(2));
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2, 3, 4, 5});

	h.hub->OnFrame(TurnFrame(9));
	h.hub->OnFrame(EndFrame(9));
	EXPECT_EQ(h.sent.size(), 2u) << "END 不抢占:这时还只发了第一批 TURN";
	EXPECT_EQ(h.hub->SubscriberCount(), 5u);
	h.RunAllDeferred();

	const auto frames = MessageIdsBySession(h.sent);
	ASSERT_EQ(frames.size(), 5u);
	for (const auto &[session, messageIds] : frames)
	{
		EXPECT_EQ(messageIds, (std::vector<uint32_t>{kTurnId, kEndId})) << "session=" << session;
	}
	for (const auto &frame : h.sent)
	{
		EXPECT_EQ(frame.delivery,
				  frame.content.message_id() == kEndId ? FrameDelivery::kMustDeliver : FrameDelivery::kBestEffort);
	}
	EXPECT_EQ(h.hub->Counters().coalesced, 0u);
	EXPECT_EQ(h.hub->Counters().framesFannedOut, 2u);
	EXPECT_EQ(h.hub->SubscriberCount(), 0u);
	EXPECT_EQ(h.hub->EntryCount(), 1u) << "条目留作墓碑";
}

// 名单尾部还没收过状态的会话:END 之后第一帧仍然必须是 161,所以缓存要留到 166 开始扇出才清。
TEST(SpectateHub, TailSessionWithoutStateStillGetsAStateFrameBeforeEnd)
{
	HubHarness h(LimitsWithBatch(1));
	h.hub->Subscribe(1, HubHarness::MakeTicket());
	h.hub->Subscribe(2, HubHarness::MakeTicket()); // 没有缓存:两个都在等第一帧

	h.hub->OnFrame(TurnFrame(9));
	h.hub->OnFrame(EndFrame(9));
	h.RunAllDeferred();

	const auto frames = MessageIdsBySession(h.sent);
	ASSERT_EQ(frames.size(), 2u);
	for (const auto &[session, messageIds] : frames)
	{
		EXPECT_EQ(messageIds, (std::vector<uint32_t>{kStateId, kEndId})) << "session=" << session;
	}
	for (const auto &frame : h.sent)
	{
		if (frame.content.message_id() == kStateId)
		{
			EXPECT_EQ(RoundOf(frame.content), 9u);
		}
	}
}

// 必达只看「这一轮发的是不是 166」,不看 phase:END 之后还在发完的最后一回合仍受软水位限制。
TEST(SpectateHub, LastTurnIsStillBestEffortWhileEndIsPending)
{
	HubHarness h(LimitsWithBatch(1));
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2});

	h.bufferFull = {1, 2};
	h.hub->OnFrame(TurnFrame(9));
	h.hub->OnFrame(EndFrame(9));
	h.RunAllDeferred();

	EXPECT_EQ(h.hub->Counters().softWatermarkSkips, 2u);
	ASSERT_EQ(h.sent.size(), 2u);
	EXPECT_EQ(SessionsOf(h.sent), (std::set<SessionId>{1, 2}));
	for (const auto &frame : h.sent)
	{
		EXPECT_EQ(frame.content.message_id(), kEndId);
		EXPECT_EQ(frame.delivery, FrameDelivery::kMustDeliver);
	}
}

// END 等在途的 TURN 期间来的订阅:新会话和还没轮到的老会话都立即收 166,之后不再出现在任何名单里(恰好一帧 166)。
TEST(SpectateHub, SubscribeWhileEndIsPendingGetsOnlyTheEnd)
{
	HubHarness h(LimitsWithBatch(1));
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2});

	h.hub->OnFrame(TurnFrame(9));
	h.hub->OnFrame(EndFrame(9));
	ASSERT_EQ(h.sent.size(), 1u);
	const SessionId served = h.sent[0].session;
	const SessionId pending = served == 1 ? 2 : 1;

	h.hub->Subscribe(3, HubHarness::MakeTicket());
	h.hub->Subscribe(pending, HubHarness::MakeTicket());
	h.RunAllDeferred();

	auto frames = MessageIdsBySession(h.sent);
	EXPECT_EQ(frames[served], (std::vector<uint32_t>{kTurnId, kEndId}));
	EXPECT_EQ(frames[pending], (std::vector<uint32_t>{kEndId}));
	EXPECT_EQ(frames[3], (std::vector<uint32_t>{kEndId}));
	EXPECT_EQ(h.hub->SubscriberCount(), 0u);
}

// 源站失联同一条规则:loop 卡住的极端情况下一轮扇出跨过了空闲判定,也先发完再收尾;巡检不重复收尾。
TEST(SpectateHub, SourceLostAlsoWaitsForTheInFlightFanout)
{
	HubHarness h(LimitsWithBatch(1));
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2});

	h.hub->OnFrame(TurnFrame(2));
	ASSERT_EQ(h.sent.size(), 1u);
	h.hub->SweepIdle(h.now + seconds(30), kWallNowMs);
	h.hub->SweepIdle(h.now + seconds(60), kWallNowMs);
	EXPECT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(h.hub->Counters().sourceLost, 1u);

	h.RunAllDeferred();
	const auto frames = MessageIdsBySession(h.sent);
	ASSERT_EQ(frames.size(), 2u);
	for (const auto &[session, messageIds] : frames)
	{
		EXPECT_EQ(messageIds, (std::vector<uint32_t>{kTurnId, kEndId})) << "session=" << session;
	}
	EXPECT_EQ(h.hub->SubscriberCount(), 0u);
	EXPECT_EQ(h.hub->EntryCount(), 0u);
}

TEST(SpectateHub, TombstoneIsKeptUntilTheEndFanoutFinishes)
{
	HubHarness h(LimitsWithBatch(1));
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2});
	h.hub->OnFrame(EndFrame(2));

	h.hub->SweepIdle(h.now + seconds(600), kWallNowMs); // loop 卡住的极端情况:墓碑到期时 END 还没发完
	EXPECT_EQ(h.hub->EntryCount(), 1u);
	h.RunAllDeferred();
	EXPECT_EQ(h.hub->SubscriberCount(), 0u);

	h.hub->SweepIdle(h.now + seconds(600), kWallNowMs);
	EXPECT_EQ(h.hub->EntryCount(), 0u);
}

TEST(SpectateHub, SourceLostFanoutIsBatchedAndDeletesTheEntryWhenFinished)
{
	HubHarness h(LimitsWithBatch(2));
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2, 3});

	h.hub->SweepIdle(h.now + seconds(30), kWallNowMs);
	EXPECT_EQ(h.sent.size(), 2u);
	EXPECT_EQ(h.hub->EntryCount(), 1u);

	h.RunAllDeferred();
	EXPECT_EQ(SessionsOf(h.sent), (std::set<SessionId>{1, 2, 3}));
	EXPECT_EQ(h.hub->SubscriberCount(), 0u);
	EXPECT_EQ(h.hub->EntryCount(), 0u);
}

// ---- 软水位 ----

TEST(SpectateHub, FullOutputBufferSkipsTheFrameButKeepsTheSubscription)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1, 2});

	h.bufferFull.insert(1);
	h.hub->OnFrame(TurnFrame(2));
	EXPECT_EQ(SessionsOf(h.sent), (std::set<SessionId>{2}));
	EXPECT_EQ(h.hub->Counters().softWatermarkSkips, 1u);
	EXPECT_EQ(h.hub->SubscriberCount(), 2u);

	h.bufferFull.clear();
	h.sent.clear();
	h.hub->OnFrame(TurnFrame(3)); // 下一帧自愈,不补发
	EXPECT_EQ(SessionsOf(h.sent), (std::set<SessionId>{1, 2}));
}

TEST(SpectateHub, SkippedFirstFrameIsRetriedAsAStateFrame)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(1));

	h.bufferFull.insert(1);
	h.hub->Subscribe(1, HubHarness::MakeTicket());
	EXPECT_TRUE(h.sent.empty());
	EXPECT_EQ(h.hub->Counters().softWatermarkSkips, 1u);

	h.bufferFull.clear();
	h.hub->OnFrame(TurnFrame(2));
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(h.sent[0].content.message_id(), kStateId) << "没收到过状态的会话第一帧仍然是 161";

	h.hub->OnFrame(TurnFrame(3));
	ASSERT_EQ(h.sent.size(), 2u);
	EXPECT_EQ(h.sent[1].content.message_id(), kTurnId);
}

TEST(SpectateHub, TerminalFramesIgnoreTheSoftWatermark)
{
	HubHarness h;
	h.hub->OnFrame(StateFrame(1));
	h.SubscribeAndDrainFirstFrames({1});

	h.bufferFull.insert(1);
	h.hub->OnFrame(EndFrame(2));
	ASSERT_EQ(h.sent.size(), 1u);
	EXPECT_EQ(h.sent[0].content.message_id(), kEndId);
	EXPECT_EQ(h.sent[0].delivery, FrameDelivery::kMustDeliver);
	EXPECT_EQ(h.hub->Counters().softWatermarkSkips, 0u);
}

// ---- 目标过滤:观战帧 topic 用 gate 的默认过滤字段(BuildDefaultKafkaOptions),这里钉住字段名与放行规则 ----

namespace
{
	constexpr uint32_t kSelfGate = 5;
	const std::string kSelfGateInstance = "gate-instance-self";

	node::kafka::KafkaCommandTargetFilter GateFilter()
	{
		node::kafka::KafkaCommandTargetFilter filter;
		filter.nodeIdFieldNames = {"target_gate_id", "target_node_id"};
		filter.instanceIdFieldNames = {"target_instance_id"};
		return filter;
	}

	bool PassesFilter(const SpectateFrameEvent &event, const node::kafka::KafkaCommandTargetFilter &filter = GateFilter())
	{
		return node::kafka::detail::ValidateCommandTarget("spectate-topic", event, kSelfGate, kSelfGateInstance, filter);
	}

	// 发给本 gate 前任实例的帧。
	SpectateFrameEvent FrameForPredecessorInstance()
	{
		SpectateFrameEvent event = TurnFrame(1);
		event.set_target_gate_id(kSelfGate);
		event.set_target_instance_id("gate-instance-predecessor");
		return event;
	}

	// 数「僵尸实例」WARN 的行数。muduo 的日志出口是全局函数指针,不能捕获,计数只能放全局;只在本文件的测试线程上用。
	int gStaleInstanceWarnLines = 0;

	void CountStaleInstanceWarn(const char *message, int length)
	{
		if (std::string_view(message, static_cast<std::size_t>(length)).find("stale instance ignored") != std::string_view::npos)
		{
			++gStaleInstanceWarnLines;
		}
	}

	void WriteLogToStdout(const char *message, int length)
	{
		std::fwrite(message, 1, static_cast<std::size_t>(length), stdout);
	}

	// 作用域守卫:构造时接管日志出口并清零,析构时还给 stdout(用例中途 ASSERT 退出也会还)。
	// muduo 不提供「取当前出口」的接口,它的默认出口同样是写 stdout,所以还成等价的这一个。
	class StaleInstanceWarnProbe
	{
	public:
		StaleInstanceWarnProbe()
		{
			gStaleInstanceWarnLines = 0;
			muduo::Logger::setOutput(CountStaleInstanceWarn);
		}
		~StaleInstanceWarnProbe() { muduo::Logger::setOutput(WriteLogToStdout); }
		StaleInstanceWarnProbe(const StaleInstanceWarnProbe &) = delete;
		StaleInstanceWarnProbe &operator=(const StaleInstanceWarnProbe &) = delete;
	};
} // namespace

TEST(SpectateFrameTargetFilter, RelayFrameOnlyPassesForItsTargetGateInstance)
{
	SpectateFrameEvent event = TurnFrame(1);
	event.set_target_gate_id(kSelfGate);
	event.set_target_instance_id(kSelfGateInstance);
	EXPECT_TRUE(PassesFilter(event));

	event.set_target_gate_id(kSelfGate + 256); // 同一分区上的另一台 gate
	EXPECT_FALSE(PassesFilter(event));

	event.set_target_gate_id(kSelfGate);
	event.set_target_instance_id("gate-instance-predecessor"); // node_id 被回收,发给前任的帧
	EXPECT_FALSE(PassesFilter(event));
}

TEST(SpectateFrameTargetFilter, BroadcastFrameWithEmptyTargetsPasses)
{
	EXPECT_TRUE(PassesFilter(TurnFrame(1)));
}

// 默认不采样(控制命令是偶发的,现有调用方行为不变),每一条实例不符都打一行。
TEST(SpectateFrameTargetFilter, StaleInstanceWarnIsNotSampledByDefault)
{
	const SpectateFrameEvent event = FrameForPredecessorInstance();
	const StaleInstanceWarnProbe probe;
	for (int drop = 0; drop < 3; ++drop)
	{
		EXPECT_FALSE(PassesFilter(event));
	}
	EXPECT_EQ(gStaleInstanceWarnLines, 3);
}

// 回归:gate 重启拿回同一 node_id 后,发给前任实例的中继帧每回合都走到这条分支。打开采样后第一条必打,此后每 1024 条一行。
TEST(SpectateFrameTargetFilter, StaleInstanceWarnIsSampledWhenTheFilterAsksForIt)
{
	const SpectateFrameEvent event = FrameForPredecessorInstance();
	auto filter = GateFilter();
	filter.staleInstanceDropCounter = std::make_shared<std::atomic<uint64_t>>(0);
	const StaleInstanceWarnProbe probe;

	EXPECT_FALSE(PassesFilter(event, filter));
	EXPECT_EQ(gStaleInstanceWarnLines, 1) << "第一条必打";

	for (int drop = 0; drop < 1023; ++drop)
	{
		EXPECT_FALSE(PassesFilter(event, filter));
	}
	EXPECT_EQ(gStaleInstanceWarnLines, 1);

	EXPECT_FALSE(PassesFilter(event, filter));
	EXPECT_EQ(gStaleInstanceWarnLines, 2) << "第 1025 条打下一行";
	EXPECT_EQ(filter.staleInstanceDropCounter->load(), 1025u);
}

// 采样只管僵尸实例这一条分支:放行的帧、发给同分区别的 gate 的帧都不计数,否则正常流量会把真正的僵尸告警采样掉。
TEST(SpectateFrameTargetFilter, OnlyStaleInstanceDropsAdvanceTheSampleCounter)
{
	auto filter = GateFilter();
	filter.staleInstanceDropCounter = std::make_shared<std::atomic<uint64_t>>(0);

	SpectateFrameEvent event = TurnFrame(1);
	EXPECT_TRUE(PassesFilter(event, filter)); // 广播帧

	event.set_target_gate_id(kSelfGate);
	event.set_target_instance_id(kSelfGateInstance);
	EXPECT_TRUE(PassesFilter(event, filter)); // 发给本实例的中继帧

	event.set_target_gate_id(kSelfGate + 256);
	EXPECT_FALSE(PassesFilter(event, filter)); // 同分区上别的 gate 的中继帧

	EXPECT_EQ(filter.staleInstanceDropCounter->load(), 0u);
}

// ---- gate_push 的共享接口:SpectateHub 复用的编码一次、以及单播 / 场景 / 全服推送的空内容判定 ----

TEST(GatePushEncodedFrame, RoundTripsThroughTheClientCodec)
{
	MessageContent content;
	content.set_message_id(kTurnId);
	content.set_serialized_message("body");
	const gate_push::EncodedFrame frame(content);

	const MessageContent decoded = DecodeFrame(frame);
	EXPECT_EQ(decoded.message_id(), kTurnId);
	EXPECT_EQ(decoded.serialized_message(), "body");
}

// 缺 message_content 时 message_content() 是默认实例,直接往下传就是给客户端推一个空帧;
// 单播、场景、全服入口靠这里的空指针整条拒绝(投递函数拿到空指针之后的行为见 gate_push_delivery_test.cpp)。
TEST(GatePushContentOf, MissingMessageContentIsReportedAsNull)
{
	contracts::kafka::PushToPlayerEvent push;
	EXPECT_EQ(gate_push::ContentOf(push), nullptr);
	contracts::kafka::BroadcastToSceneEvent scene;
	EXPECT_EQ(gate_push::ContentOf(scene), nullptr);
	contracts::kafka::BroadcastToAllEvent all;
	EXPECT_EQ(gate_push::ContentOf(all), nullptr);

	push.mutable_message_content(); // 字段存在但内容全默认,仍是发送方明确给的一条消息
	EXPECT_EQ(gate_push::ContentOf(push), &push.message_content());
}
