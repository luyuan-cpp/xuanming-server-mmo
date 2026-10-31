#include "player/system/player_inbox_rules.h"

#include <algorithm>
#include <cstddef>
#include <iterator>
#include <vector>

#include "proto/player_inbox/player_inbox.pb.h"

namespace player_inbox_rules
{
	namespace
	{
		constexpr const char *kDeliveryClassNames[] = {"new", "replay", "mismatch", "unknown"};
		static_assert(std::size(kDeliveryClassNames) == static_cast<std::size_t>(DeliveryClass::kCount),
					  "名字表与 DeliveryClass 脱节");

		constexpr const char *kDurabilityNames[] = {"durable", "not_yet", "outcome_differs", "ring_missing"};
		static_assert(std::size(kDurabilityNames) == static_cast<std::size_t>(Durability::kCount),
					  "名字表与 Durability 脱节");

		constexpr const char *kItemActionNames[] = {"execute",     "skip_voided",   "replay",
													"ack_unknown", "halt_mismatch", "halt_unknown_directive"};
		static_assert(std::size(kItemActionNames) == static_cast<std::size_t>(ItemAction::kCount),
					  "名字表与 ItemAction 脱节");

		// 环按 seq 升序且 ≤ kRecentCapacity 条:线性扫即可,不值得为 32 条建索引。
		const PlayerInboxDecision *FindRecent(const PlayerInboxDomainCursor &cursor, uint64_t seq)
		{
			for (const auto &decision : cursor.recent())
			{
				if (decision.seq() == seq)
				{
					return &decision;
				}
			}
			return nullptr;
		}

		// baseMs × [1 - spread%, 1 + spread%],抖动位置由 jitterPermille ∈ [0, 1000] 线性决定。
		// 上溢余量:base 最大是 periodic 的 UINT32_MAX 分钟 ≈ 2.6e14 ms,乘 1200 仍远小于 uint64 上限。
		uint64_t ApplyJitter(uint64_t baseMs, uint32_t jitterPermille, uint32_t spreadPercent)
		{
			const uint64_t clamped = std::min(jitterPermille, kJitterPermilleMax);
			const uint64_t lowPermille = 1000 - static_cast<uint64_t>(spreadPercent) * 10;
			const uint64_t spanPermille = static_cast<uint64_t>(spreadPercent) * 20;
			const uint64_t factorPermille = lowPermille + spanPermille * clamped / kJitterPermilleMax;
			return baseMs * factorPermille / 1000;
		}

		constexpr uint32_t kBackoffJitterPercent = 20;
		constexpr uint32_t kBackoffMaxShift = 6; // 1s << 6 = 64s,随即被 60s 封顶
	} // namespace

	const PlayerInboxDomainCursor *FindCursor(const PlayerInboxLedgerComp &ledger, uint32_t domain)
	{
		for (const auto &cursor : ledger.cursors())
		{
			if (cursor.domain() == domain)
			{
				return &cursor;
			}
		}
		return nullptr;
	}

	PlayerInboxDomainCursor &MutableCursor(PlayerInboxLedgerComp &ledger, uint32_t domain)
	{
		auto *cursors = ledger.mutable_cursors();
		for (int index = 0; index < cursors->size(); ++index)
		{
			if (cursors->Get(index).domain() == domain)
			{
				return *cursors->Mutable(index);
			}
		}
		cursors->Add()->set_domain(domain);
		// 保持 domain 升序:新条目从尾部往前换到位。条数 = 域数,极少。顺序本身没有业务含义
		// (查找一律按 domain 字段),只是让同一份账本的序列化结果确定、便于比对。
		int index = cursors->size() - 1;
		while (index > 0 && cursors->Get(index - 1).domain() > domain)
		{
			cursors->SwapElements(index, index - 1);
			--index;
		}
		return *cursors->Mutable(index);
	}

	bool IsRecordableOutcome(uint32_t outcome)
	{
		return outcome == static_cast<uint32_t>(player_inbox::PLAYER_INBOX_OUTCOME_APPLIED) ||
			   outcome == static_cast<uint32_t>(player_inbox::PLAYER_INBOX_OUTCOME_REJECTED) ||
			   outcome == static_cast<uint32_t>(player_inbox::PLAYER_INBOX_OUTCOME_SKIPPED);
	}

	bool RecordDecision(PlayerInboxDomainCursor &cursor, const PlayerInboxDecision &decision)
	{
		if (!IsRecordableOutcome(decision.outcome()))
		{
			return false;
		}
		if (decision.seq() == 0 || decision.seq() <= cursor.decided_through_seq())
		{
			return false;
		}
		auto *recent = cursor.mutable_recent();
		*recent->Add() = decision;
		if (recent->size() > kRecentCapacity)
		{
			recent->DeleteSubrange(0, recent->size() - kRecentCapacity);
		}
		cursor.set_decided_through_seq(decision.seq());
		return true;
	}

	std::string ValidateLedger(const PlayerInboxLedgerComp &ledger)
	{
		std::vector<uint32_t> seenDomains;
		seenDomains.reserve(static_cast<std::size_t>(ledger.cursors_size()));
		for (int i = 0; i < ledger.cursors_size(); ++i)
		{
			const auto &cursor = ledger.cursors(i);
			const std::string where = "cursor[" + std::to_string(i) + "] domain=" + std::to_string(cursor.domain());
			if (cursor.domain() == 0)
			{
				return where + ": domain is 0";
			}
			if (std::find(seenDomains.begin(), seenDomains.end(), cursor.domain()) != seenDomains.end())
			{
				return where + ": duplicate domain";
			}
			seenDomains.push_back(cursor.domain());
			if (cursor.recent_size() > kRecentCapacity)
			{
				return where + ": recent has " + std::to_string(cursor.recent_size()) + " entries (max " +
					   std::to_string(kRecentCapacity) + ")";
			}
			uint64_t previousSeq = 0;
			for (int j = 0; j < cursor.recent_size(); ++j)
			{
				const auto &decision = cursor.recent(j);
				const std::string entry = where + " recent[" + std::to_string(j) + "] seq=" + std::to_string(decision.seq());
				if (decision.seq() == 0)
				{
					return entry + ": seq is 0";
				}
				if (decision.seq() <= previousSeq)
				{
					return entry + ": not strictly ascending";
				}
				if (decision.seq() > cursor.decided_through_seq())
				{
					return entry + ": beyond decided_through_seq=" + std::to_string(cursor.decided_through_seq());
				}
				if (!IsRecordableOutcome(decision.outcome()))
				{
					return entry + ": outcome " + std::to_string(decision.outcome()) + " is not recordable";
				}
				previousSeq = decision.seq();
			}
		}
		return {};
	}

	const char *DeliveryClassName(DeliveryClass deliveryClass)
	{
		const auto index = static_cast<std::size_t>(deliveryClass);
		return index < std::size(kDeliveryClassNames) ? kDeliveryClassNames[index] : "?";
	}

	DeliveryClass ClassifyDelivery(const PlayerInboxDomainCursor *cursor, uint64_t seq, uint64_t requestId,
								   uint32_t msgType, PlayerInboxDecision *replayed)
	{
		if (cursor == nullptr || seq > cursor->decided_through_seq())
		{
			return DeliveryClass::kNew;
		}
		const auto *recorded = FindRecent(*cursor, seq);
		if (recorded == nullptr)
		{
			return DeliveryClass::kUnknown;
		}
		// 先拷再核对:kMismatch 时也要把账本里那一条带出去,否则停用那行 ERROR 的 ledger_request_id /
		// ledger_msg_type 恒为 0,值班会误判成"账本损坏"而不是"同 seq 被另一个请求占用"(评审 F4)。
		// 拷出来的只供日志,能不能回放由返回值决定(契约见头文件)。
		if (replayed != nullptr)
		{
			*replayed = *recorded;
		}
		if (recorded->request_id() != requestId || recorded->msg_type() != msgType)
		{
			return DeliveryClass::kMismatch;
		}
		return DeliveryClass::kReplay;
	}

	bool HasRecentAppliedBattleSettlement(const PlayerInboxDomainCursor *cursor, uint64_t requestId)
	{
		if (cursor == nullptr || requestId == 0)
		{
			return false;
		}
		const auto battleSettlement = static_cast<uint32_t>(player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT);
		const auto applied = static_cast<uint32_t>(player_inbox::PLAYER_INBOX_OUTCOME_APPLIED);
		// 环 ≤ kRecentCapacity 条,线性扫即可(同 FindRecent)。
		for (const auto &decision : cursor->recent())
		{
			if (decision.request_id() == requestId && decision.msg_type() == battleSettlement &&
				decision.outcome() == applied)
			{
				return true;
			}
		}
		return false;
	}

	const char *ItemActionName(ItemAction action)
	{
		const auto index = static_cast<std::size_t>(action);
		return index < std::size(kItemActionNames) ? kItemActionNames[index] : "?";
	}

	ItemAction PlanItem(DeliveryClass deliveryClass, uint32_t directive)
	{
		switch (deliveryClass)
		{
		case DeliveryClass::kReplay:
			return ItemAction::kReplay;
		case DeliveryClass::kUnknown:
			return ItemAction::kAckUnknown;
		case DeliveryClass::kNew:
			break;
		case DeliveryClass::kMismatch:
		case DeliveryClass::kCount:
		default:
			return ItemAction::kHaltMismatch;
		}
		if (directive == static_cast<uint32_t>(player_inbox::PLAYER_INBOX_DIRECTIVE_VOID))
		{
			return ItemAction::kSkipVoided;
		}
		if (directive != static_cast<uint32_t>(player_inbox::PLAYER_INBOX_DIRECTIVE_NONE))
		{
			return ItemAction::kHaltUnknownDirective;
		}
		return ItemAction::kExecute;
	}

	const char *DurabilityName(Durability durability)
	{
		const auto index = static_cast<std::size_t>(durability);
		return index < std::size(kDurabilityNames) ? kDurabilityNames[index] : "?";
	}

	Durability ClassifyDurability(const PlayerInboxLedgerComp &persisted, uint32_t domain,
								  const PlayerInboxDecision &decision)
	{
		const auto *cursor = FindCursor(persisted, domain);
		if (cursor == nullptr || cursor->decided_through_seq() < decision.seq())
		{
			return Durability::kNotYet;
		}
		if (decision.outcome() == static_cast<uint32_t>(player_inbox::PLAYER_INBOX_OUTCOME_UNKNOWN))
		{
			return Durability::kDurable;
		}
		const auto *onDisk = FindRecent(*cursor, decision.seq());
		if (onDisk == nullptr)
		{
			return Durability::kRingMissing;
		}
		if (onDisk->outcome() != decision.outcome() || onDisk->reason() != decision.reason() ||
			onDisk->request_id() != decision.request_id() || onDisk->msg_type() != decision.msg_type())
		{
			return Durability::kOutcomeDiffers;
		}
		return Durability::kDurable;
	}

	bool IsNewLanding(bool hasRuntime, uint64_t landedEpoch, uint64_t currentEpoch)
	{
		return !hasRuntime || landedEpoch != currentEpoch;
	}

	uint64_t BackoffMs(uint32_t failStreak, uint32_t jitterPermille)
	{
		const uint32_t streak = std::max<uint32_t>(failStreak, 1);
		const uint32_t shift = std::min<uint32_t>(streak - 1, kBackoffMaxShift);
		const uint64_t baseMs = std::min<uint64_t>(kBackoffMinMs << shift, kBackoffMaxMs);
		const uint64_t jittered = ApplyJitter(baseMs, jitterPermille, kBackoffJitterPercent);
		return std::clamp<uint64_t>(jittered, kBackoffMinMs, kBackoffMaxMs);
	}

	uint64_t PeriodicDelayMs(uint32_t minutes, uint32_t jitterPermille)
	{
		if (minutes == 0)
		{
			return 0;
		}
		constexpr uint64_t kMsPerMinute = 60ULL * 1000ULL;
		return ApplyJitter(static_cast<uint64_t>(minutes) * kMsPerMinute, jitterPermille, kPeriodicJitterPercent);
	}

	bool IsStuckWarnDue(uint32_t failStreak)
	{
		return failStreak != 0 && failStreak % kStuckWarnEveryFailures == 0;
	}
} // namespace player_inbox_rules
