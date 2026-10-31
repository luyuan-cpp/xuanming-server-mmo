#include "gate_spectate_hub.h"

#include <algorithm>
#include <iterator>
#include <utility>

#include "muduo/base/Logging.h"

#include "handler/gate_push_delivery.h"
#include "proto/battle/player_battle.pb.h"
#include "proto/common/base/message.pb.h"
#include "proto/contracts/kafka/gate_event.pb.h"
#include "proto/gate/gate_spectate_ticket.pb.h"
#include "rpc/service_metadata/player_battle_service_metadata.h"

namespace gate_spectate
{
	namespace
	{
		using gate_push::EncodedFrame;

		// 种类与客户端消息号必须对得上:首帧合成靠消息号区分 161 / 158,错配的帧发下去客户端状态机会错乱。
		bool KindMatchesMessageId(contracts::kafka::eSpectateFrameKind kind, uint32_t messageId)
		{
			switch (kind)
			{
			case contracts::kafka::SPECTATE_FRAME_STATE:
				return messageId == BattleClientPlayerNotifySpectateStateMessageId;
			case contracts::kafka::SPECTATE_FRAME_TURN:
				return messageId == BattleClientPlayerNotifySpectateTurnResultMessageId;
			case contracts::kafka::SPECTATE_FRAME_END:
				return messageId == BattleClientPlayerNotifySpectateEndMessageId;
			default:
				return false;
			}
		}

		// 坏帧在建条目之前拒绝,不留空条目。battle 是可信来源,走到这里只可能是版本不符或生产者缺陷。
		std::shared_ptr<const MessageContent> ParseClientFrame(const contracts::kafka::SpectateFrameEvent &event)
		{
			if (event.battle_id() == 0 || event.battle_instance_id().empty())
			{
				return nullptr;
			}
			auto content = std::make_shared<MessageContent>();
			if (!content->ParseFromString(event.client_frame()) ||
				!KindMatchesMessageId(event.kind(), content->message_id()))
			{
				return nullptr;
			}
			return content;
		}

		// 采样口径与 gate 其它丢弃日志相同:第一条必打,此后每 1024 条一行。
		bool IsSampled(uint64_t total)
		{
			return (total & 0x3FF) == 1;
		}
	} // namespace

	SpectateHub::SpectateHub(HubPorts ports, HubLimits limits)
		: ports_(std::move(ports)), limits_(limits)
	{
	}

	std::vector<SessionId> SpectateHub::AllSubscribers(const Entry &entry)
	{
		std::vector<SessionId> sessions;
		sessions.reserve(entry.subscribers.size());
		for (const auto &[sessionId, subscriber] : entry.subscribers)
		{
			sessions.push_back(sessionId);
		}
		return sessions;
	}

	std::vector<SessionId> SpectateHub::NextTurnTargets(Entry &entry)
	{
		std::vector<SessionId> targets = AllSubscribers(entry);
		// 名单总是哈希表迭代序(没有 rehash 时每轮相同),每轮从头发的话,过载时表头帧帧都收到、表尾一帧也收不到,连续黑屏。
		// fanoutNext 是相对上一轮名单的下标,而上一轮名单本身已经转过,所以起点要累加:只按 fanoutNext 转,第三轮又会回到
		// 第二轮的起点,表尾照样饿死。上一轮发完时 fanoutNext 是 0,起点不动。
		// 订阅者增减、rehash 或上一轮是 STATE 补发时下标只是近似位置;这里要的是大家轮流,不要求精确,取模保证不越界。
		entry.turnFanoutStart = (entry.turnFanoutStart + entry.fanoutNext) % targets.size();
		std::rotate(targets.begin(), targets.begin() + static_cast<std::ptrdiff_t>(entry.turnFanoutStart), targets.end());
		return targets;
	}

	bool SpectateHub::IsSourceSilent(const Entry &entry, SteadyTime nowSteady, uint64_t nowWallMs) const
	{
		if (nowSteady - entry.lastFrameSteady >= limits_.idleTimeout)
		{
			return true;
		}
		return entry.deadlineMs != 0 && nowWallMs > entry.deadlineMs + limits_.deadlineGraceMs;
	}

	void SpectateHub::Subscribe(SessionId sessionId, const SpectateTicketPayload &ticket)
	{
		const uint64_t battleId = ticket.battle_id();
		if (const auto current = battleOfSession_.find(sessionId);
			current != battleOfSession_.end() && current->second != battleId)
		{
			DetachSession(sessionId);
		}

		const auto [it, inserted] = entries_.try_emplace(battleId);
		Entry &entry = it->second;
		if (inserted)
		{
			entry.battleInstanceId = ticket.battle_instance_id();
			entry.lastFrameSteady = ports_.steadyNow(); // 空闲计时从订阅开始:一直等不到帧也要能收尾
		}
		else if (entry.battleInstanceId != ticket.battle_instance_id())
		{
			RebindToInstance(entry, ticket.battle_instance_id());
		}
		if (ticket.deadline_ms() != 0)
		{
			entry.deadlineMs = ticket.deadline_ms();
		}

		if (entry.phase != Phase::kLive)
		{
			// 这一场已经结束。这个会话若还在 END 的扇出名单里(又发了一次订阅),先摘掉,免得收到两帧 166。
			DetachSession(sessionId);
			ports_.sendFrame(sessionId, *entry.finalFrame, FrameDelivery::kMustDeliver);
			return;
		}

		Subscriber &subscriber = entry.subscribers[sessionId];
		subscriber.receivedState = false;
		battleOfSession_[sessionId] = battleId;
		if (const auto firstFrame = FirstFrameOf(entry))
		{
			SendToSubscriber(sessionId, subscriber, *firstFrame, FrameDelivery::kBestEffort);
		}
	}

	void SpectateHub::Unsubscribe(SessionId sessionId, uint64_t battleId)
	{
		const auto it = battleOfSession_.find(sessionId);
		if (it != battleOfSession_.end() && it->second == battleId)
		{
			DetachSession(sessionId);
		}
	}

	void SpectateHub::OnSessionClosed(SessionId sessionId)
	{
		DetachSession(sessionId);
	}

	void SpectateHub::DetachSession(SessionId sessionId)
	{
		const auto it = battleOfSession_.find(sessionId);
		if (it == battleOfSession_.end())
		{
			return;
		}
		if (const auto entryIt = entries_.find(it->second); entryIt != entries_.end())
		{
			entryIt->second.subscribers.erase(sessionId);
		}
		battleOfSession_.erase(it);
		// 条目留着:帧还在来(L1 battle 不知道观众走了),缓存留给下一个订阅者;没人订阅且 idleTimeout 没帧时由巡检删除。
	}

	// 同一 battle_id 换了实例:凭证是 match 刚签的,比条目里的记录新。留在条目里的订阅者继续看新实例,
	// 从下一帧的 161 重新开始;旧实例的缓存、墓碑和没发完的扇出全部作废。
	void SpectateHub::RebindToInstance(Entry &entry, const std::string &battleInstanceId)
	{
		entry.battleInstanceId = battleInstanceId;
		entry.phase = Phase::kLive;
		entry.lastTurnRound = -1;
		entry.cachedStateRound = -1;
		entry.lastFrameSteady = ports_.steadyNow();
		entry.deadlineMs = 0;
		entry.latestState.reset();
		entry.firstFrame.reset();
		entry.finalFrame.reset();
		entry.fanoutFrame.reset();
		entry.fanoutTargets.clear();
		entry.fanoutNext = 0;
		entry.turnFanoutStart = 0;
		for (auto &[sessionId, subscriber] : entry.subscribers)
		{
			subscriber.receivedState = false;
		}
	}

	void SpectateHub::OnFrame(const contracts::kafka::SpectateFrameEvent &event)
	{
		auto content = ParseClientFrame(event);
		if (!content)
		{
			++counters_.invalidFrames;
			if (IsSampled(counters_.invalidFrames))
			{
				LOG_ERROR << "Spectate frame rejected: invalid frame (sampled), kind=" << static_cast<int>(event.kind())
						  << " invalid_total=" << counters_.invalidFrames;
			}
			return;
		}

		const uint64_t battleId = event.battle_id();
		const auto [it, inserted] = entries_.try_emplace(battleId);
		Entry &entry = it->second;
		if (inserted)
		{
			entry.battleInstanceId = event.battle_instance_id();
		}
		else if (entry.battleInstanceId != event.battle_instance_id())
		{
			++counters_.instanceMismatch;
			return;
		}
		if (entry.phase != Phase::kLive)
		{
			return; // END 只收一次;源站失联的条目正在发收尾帧,发完就删,之后的帧会重建条目
		}

		entry.lastFrameSteady = ports_.steadyNow();
		if (event.deadline_ms() != 0)
		{
			entry.deadlineMs = event.deadline_ms();
		}

		const int64_t round = event.round_index();
		switch (event.kind())
		{
		case contracts::kafka::SPECTATE_FRAME_STATE:
			OnStateFrame(battleId, entry, round, std::move(content));
			break;
		case contracts::kafka::SPECTATE_FRAME_TURN:
			OnTurnFrame(battleId, entry, round, std::move(content));
			break;
		case contracts::kafka::SPECTATE_FRAME_END:
			OnEndFrame(battleId, entry, *content);
			break;
		default:
			break; // ParseClientFrame 已拒绝其它种类
		}
	}

	void SpectateHub::OnStateFrame(uint64_t battleId, Entry &entry, int64_t round,
								   std::shared_ptr<const MessageContent> content)
	{
		// 比缓存旧的快照不要:包括第 r 回合结算后才到的 STATE(r)(cachedStateRound 已是 r + 1)。
		// 不推进 lastTurnRound:快照不是结算结果,紧接着的 TURN(r) 必须照常扇出。
		if (round < entry.cachedStateRound)
		{
			return;
		}
		entry.cachedStateRound = round;
		entry.latestState = std::move(content);
		entry.firstFrame.reset();

		// 已在看的会话不下发。正在进行的一轮扇出会给名单里还没收到过状态的会话补 161,不打断它;名单外的这类会话等下一回合。
		if (!entry.fanoutTargets.empty())
		{
			return;
		}
		std::vector<SessionId> waiting;
		for (const auto &[sessionId, subscriber] : entry.subscribers)
		{
			if (!subscriber.receivedState)
			{
				waiting.push_back(sessionId);
			}
		}
		if (!waiting.empty())
		{
			StartFanout(battleId, entry, FirstFrameOf(entry), std::move(waiting));
		}
	}

	void SpectateHub::OnTurnFrame(uint64_t battleId, Entry &entry, int64_t round,
								  std::shared_ptr<const MessageContent> content)
	{
		if (round <= entry.lastTurnRound)
		{
			return;
		}
		entry.lastTurnRound = round;
		// 升档时中继与广播两条 topic 没有相对顺序,TURN(r) 可能晚于更新的 STATE 到达:照常扇出(观众还没看过这回合),
		// 但不让它把缓存往回换。
		if (round + 1 >= entry.cachedStateRound)
		{
			entry.cachedStateRound = round + 1;
			entry.latestState = content;
			entry.firstFrame.reset();
		}
		if (entry.subscribers.empty())
		{
			return; // 没人看不编码;L2 每台 gate 都会收到全部广播场次的帧
		}
		StartFanout(battleId, entry, std::make_shared<EncodedFrame>(*content), NextTurnTargets(entry));
	}

	void SpectateHub::OnEndFrame(uint64_t battleId, Entry &entry, const MessageContent &content)
	{
		entry.phase = Phase::kEnded;
		entry.endedAt = ports_.steadyNow();
		entry.finalFrame = std::make_shared<EncodedFrame>(content);
		StartFinalFanoutUnlessBusy(battleId, entry);
	}

	void SpectateHub::CloseAsSourceLost(uint64_t battleId, Entry &entry)
	{
		++counters_.sourceLost;

		SpectateEndS2C end;
		end.set_battle_id(battleId);
		end.set_outcome(BATTLE_OUTCOME_ONGOING);
		end.set_reason(SPECTATE_END_SOURCE_LOST);
		MessageContent content;
		content.set_message_id(BattleClientPlayerNotifySpectateEndMessageId);
		content.set_serialized_message(end.SerializeAsString());

		entry.phase = Phase::kSourceLost;
		entry.finalFrame = std::make_shared<EncodedFrame>(content);
		StartFinalFanoutUnlessBusy(battleId, entry);
	}

	void SpectateHub::StartFinalFanoutUnlessBusy(uint64_t battleId, Entry &entry)
	{
		// 166 不带状态也不带事件,抢占在途的一轮,没轮到的会话就永远看不到那一帧。等待有上限:phase 已不是 kLive,新帧一律丢弃,
		// 在途的一轮不会再被接手,发完由 FinishFanout 回到这里。
		if (entry.fanoutNext < entry.fanoutTargets.size())
		{
			return;
		}
		// 缓存到这里才清:在途的一轮还要给名单尾部没收过状态的会话合成 161(D69)。
		entry.latestState.reset();
		entry.firstFrame.reset();
		StartFanout(battleId, entry, entry.finalFrame, AllSubscribers(entry));
	}

	std::shared_ptr<const EncodedFrame> SpectateHub::FirstFrameOf(Entry &entry)
	{
		if (entry.firstFrame)
		{
			return entry.firstFrame;
		}
		if (!entry.latestState)
		{
			return nullptr;
		}
		if (entry.latestState->message_id() == BattleClientPlayerNotifySpectateStateMessageId)
		{
			entry.firstFrame = std::make_shared<EncodedFrame>(*entry.latestState);
			return entry.firstFrame;
		}

		// 只在有人需要首帧时解析一次,结果留到换帧,开场涌入再多订阅者也只合成一次(§7)。
		TurnResultS2C turn;
		if (!turn.ParseFromString(entry.latestState->serialized_message()) || !turn.has_state())
		{
			++counters_.invalidFrames;
			if (IsSampled(counters_.invalidFrames))
			{
				LOG_ERROR << "Spectate first frame not synthesized: cached turn frame has no usable state (sampled)"
						  << " invalid_total=" << counters_.invalidFrames;
			}
			return nullptr;
		}
		SpectateStateS2C state;
		state.mutable_state()->Swap(turn.mutable_state());
		state.set_observer_count(0); // LOBBY 档 gate 不知道全服人数,0 = 未知(§15.1)
		MessageContent content;
		content.set_message_id(BattleClientPlayerNotifySpectateStateMessageId);
		content.set_serialized_message(state.SerializeAsString());
		entry.firstFrame = std::make_shared<EncodedFrame>(content);
		return entry.firstFrame;
	}

	void SpectateHub::SendToSubscriber(SessionId sessionId, Subscriber &subscriber, const EncodedFrame &frame,
									   FrameDelivery delivery)
	{
		switch (ports_.sendFrame(sessionId, frame, delivery))
		{
		case SendResult::kSent:
			subscriber.receivedState = true;
			return;
		case SendResult::kBufferFull:
			++counters_.softWatermarkSkips;
			return;
		case SendResult::kSessionGone:
			DetachSession(sessionId);
			return;
		}
	}

	void SpectateHub::StartFanout(uint64_t battleId, Entry &entry, std::shared_ptr<const EncodedFrame> frame,
								  std::vector<SessionId> targets)
	{
		if (!frame)
		{
			return; // 只会来自首帧合成失败,已计数
		}
		counters_.coalesced += entry.fanoutTargets.size() - entry.fanoutNext;
		entry.fanoutFrame = std::move(frame);
		entry.fanoutTargets = std::move(targets);
		entry.fanoutNext = 0;
		if (!entry.fanoutTargets.empty())
		{
			++counters_.framesFannedOut;
		}
		PumpFanout(battleId, entry);
	}

	void SpectateHub::PumpFanout(uint64_t battleId, Entry &entry)
	{
		const std::size_t batchEnd = (std::min)(entry.fanoutNext + limits_.fanoutBatch, entry.fanoutTargets.size());
		while (entry.fanoutNext < batchEnd)
		{
			DeliverToTarget(entry, entry.fanoutTargets[entry.fanoutNext++]);
		}
		if (entry.fanoutNext >= entry.fanoutTargets.size())
		{
			FinishFanout(battleId, entry);
			return;
		}
		// 新帧取代旧帧时续发可能已经排着,它会接着发新帧,不必再排一个。
		if (entry.continuationToken == 0)
		{
			entry.continuationToken = ++lastContinuationToken_;
			ports_.defer([weakHub = weak_from_this(), battleId, token = entry.continuationToken]
						 {
				if (const auto hub = weakHub.lock())
				{
					hub->ContinueFanout(battleId, token);
				} });
		}
	}

	void SpectateHub::ContinueFanout(uint64_t battleId, uint64_t token)
	{
		const auto it = entries_.find(battleId);
		if (it == entries_.end() || it->second.continuationToken != token)
		{
			return; // 条目已删除或已重建
		}
		Entry &entry = it->second;
		entry.continuationToken = 0;
		// 没有在途的一轮:续发排队期间,接手的新帧第一批就发完了(订阅者变少),或条目换了实例。没有要续的。
		if (entry.fanoutFrame)
		{
			PumpFanout(battleId, entry);
		}
	}

	void SpectateHub::DeliverToTarget(Entry &entry, SessionId sessionId)
	{
		const auto it = entry.subscribers.find(sessionId);
		if (it == entry.subscribers.end())
		{
			return; // 这一轮开始之后退订或断线了
		}
		// 必达只看「这一轮发的是不是 166」,不看 phase:END 之后还在发完的最后一回合仍是状态帧,受软水位限制,也照常补 161。
		if (entry.fanoutFrame == entry.finalFrame)
		{
			SendToSubscriber(sessionId, it->second, *entry.fanoutFrame, FrameDelivery::kMustDeliver);
			return;
		}
		if (it->second.receivedState)
		{
			SendToSubscriber(sessionId, it->second, *entry.fanoutFrame, FrameDelivery::kBestEffort);
			return;
		}
		// 还没收到过状态的会话:第一帧必须是 161(D69)。
		if (const auto firstFrame = FirstFrameOf(entry))
		{
			SendToSubscriber(sessionId, it->second, *firstFrame, FrameDelivery::kBestEffort);
		}
	}

	void SpectateHub::FinishFanout(uint64_t battleId, Entry &entry)
	{
		const bool finalFrameSent = entry.fanoutFrame == entry.finalFrame;
		entry.fanoutFrame.reset();
		entry.fanoutTargets.clear();
		entry.fanoutNext = 0;
		if (entry.phase == Phase::kLive)
		{
			return;
		}
		if (!finalFrameSent)
		{
			// 刚发完的是收尾之前在途的那一轮,现在轮到 166。
			StartFinalFanoutUnlessBusy(battleId, entry);
			return;
		}
		// 终局帧发完,这一场的订阅到此结束。
		for (const auto &[sessionId, subscriber] : entry.subscribers)
		{
			battleOfSession_.erase(sessionId);
		}
		entry.subscribers.clear();
		if (entry.phase == Phase::kSourceLost)
		{
			entries_.erase(battleId);
		}
	}

	void SpectateHub::SweepIdle(SteadyTime nowSteady, uint64_t nowWallMs)
	{
		std::vector<uint64_t> lostBattles;
		for (auto it = entries_.begin(); it != entries_.end();)
		{
			const Entry &entry = it->second;
			bool expired = false;
			switch (entry.phase)
			{
			case Phase::kEnded:
				// END 还没发完时条目还有订阅者,等发完(FinishFanout 清空)才算到期,否则订阅者索引会留下悬空记录。
				expired = entry.subscribers.empty() && nowSteady - entry.endedAt >= limits_.endedRetention;
				break;
			case Phase::kSourceLost:
				break; // 合成的 166 发完就删,不经巡检
			case Phase::kLive:
				if (IsSourceSilent(entry, nowSteady, nowWallMs))
				{
					if (entry.subscribers.empty())
					{
						expired = true;
					}
					else
					{
						lostBattles.push_back(it->first);
					}
				}
				break;
			}
			it = expired ? entries_.erase(it) : std::next(it);
		}

		// 遍历结束再收尾:CloseAsSourceLost 可能同步删掉条目。
		for (const uint64_t battleId : lostBattles)
		{
			CloseAsSourceLost(battleId, entries_.at(battleId));
		}

		LogMetricsIfDue(nowSteady);
	}

	void SpectateHub::LogMetricsIfDue(SteadyTime now)
	{
		if (now - lastMetricAt_ < limits_.metricInterval)
		{
			return;
		}
		lastMetricAt_ = now;
		// 标签只有累计值与当前量,不带 battle_id / player_id(AGENTS.md §9)。
		LOG_INFO << "metric=gate_spectate subscribers=" << battleOfSession_.size() << " entries=" << entries_.size()
				 << " frames=" << counters_.framesFannedOut << " soft_watermark_skips=" << counters_.softWatermarkSkips
				 << " coalesced=" << counters_.coalesced << " ticket_rejects=" << counters_.ticketRejects
				 << " source_lost=" << counters_.sourceLost << " instance_mismatch=" << counters_.instanceMismatch
				 << " invalid_frames=" << counters_.invalidFrames;
	}
} // namespace gate_spectate
