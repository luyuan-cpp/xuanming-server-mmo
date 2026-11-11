#pragma once

// 待寄邮件的纯规则(docs/design/battle-overflow-mail.md)。零 ECS 依赖,只读写 PlayerMailOutboxComp,单测直接盯。
//
// 数据在 PlayerMailOutboxComp(proto/common/component/player_mail_outbox_comp.proto),写入与寄送在
// PlayerMailOutboxSystem(player_mail_outbox.h)。这里只回答「一批物品分成几封、一条记录合不合法、
// 一次寄送算什么结果、下次什么时候再试」,不知道背包、实体、时钟与网络,也不发日志。

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <iterator>
#include <map>
#include <set>
#include <string>
#include <utility>
#include <vector>

#include "proto/common/component/player_mail_outbox_comp.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/mail_error_tip.pb.h"

namespace mail_outbox_rules
{
	// 待寄的来源(落盘存 uint32)。底层类型按枚举项个数选;kCount 恒为最后一项。
	enum class MailOutboxSource : uint8_t
	{
		kUnspecified,    // 0:只会出现在坏数据里
		kBattleOverflow, // 1:战斗结算两包都放不下,source_id = battle_id

		kCount
	};

	inline constexpr const char* kMailOutboxSourceNames[] = {"unspecified", "battle_overflow"};
	static_assert(std::size(kMailOutboxSourceNames) == static_cast<std::size_t>(MailOutboxSource::kCount),
				  "kMailOutboxSourceNames 与 MailOutboxSource 逐项对应");

	// 只进日志。本版本不认识的来源回 "unknown"。
	constexpr const char* MailOutboxSourceName(const uint32_t storedSource)
	{
		return storedSource < std::size(kMailOutboxSourceNames) ? kMailOutboxSourceNames[storedSource] : "unknown";
	}

	// 一名玩家同时至多几条待寄。正常 0 条;邮件服务长期寄不出去(不可达、邮箱满)才会攒起来。
	// 攒满之后新的改投整笔不收,战斗结算回到「两包都放不下就整笔延后」,条目留在收件箱,不丢。
	inline constexpr std::size_t kMaxEntries = 32;
	// 一封邮件的附件上限,与 go/shared/mailgrant.Bundle 同值:超了邮件服务按附件无效拒收,这一封就永远寄不出去。
	inline constexpr std::size_t kMaxItemLinesPerMail = 16;
	inline constexpr uint64_t kMaxItemCountPerMail = 4096;

	// 一封邮件的物品:配置号 -> 数量。std::map 有序,写进 items 时天然按 config_id 升序。
	using MailItems = std::map<uint32_t, uint64_t>;

	// 把一批物品分成若干封,每封不超过附件上限:按 config_id 升序装填,这一封满了(种类到上限或件数到上限)就开下一封,
	// 同一种装不下就把余量拆到下一封。配置号或数量为 0 的项跳过(调用方的缺陷,不该出现)。空批次返回空。纯函数。
	inline std::vector<MailItems> SplitIntoMails(const MailItems& items)
	{
		std::vector<MailItems> mails;
		MailItems current;
		uint64_t currentTotal = 0;
		for (const auto& [configId, count] : items)
		{
			if (configId == 0 || count == 0)
			{
				continue;
			}
			uint64_t remaining = count;
			while (remaining > 0)
			{
				if (current.size() >= kMaxItemLinesPerMail || currentTotal >= kMaxItemCountPerMail)
				{
					mails.push_back(std::move(current));
					current = MailItems{};
					currentTotal = 0;
				}
				const uint64_t take = std::min(remaining, kMaxItemCountPerMail - currentTotal);
				current[configId] += take;
				currentTotal += take;
				remaining -= take;
			}
		}
		if (!current.empty())
		{
			mails.push_back(std::move(current));
		}
		return mails;
	}

	// 按号找一条;没有返回 nullptr。只读。
	inline const MailOutboxEntry* Find(const PlayerMailOutboxComp& outbox, const uint64_t requestId)
	{
		for (const auto& entry : outbox.entries())
		{
			if (entry.request_id() == requestId)
			{
				return &entry;
			}
		}
		return nullptr;
	}

	// 追加一条。余量、号与物品形状由调用方事先核过(PlayerMailOutboxSystem::PrepareBattleOverflow),这里不再判。
	inline void Append(PlayerMailOutboxComp& outbox, const uint64_t requestId, const MailOutboxSource source,
					   const uint64_t sourceId, const std::string& title, const std::string& body, const MailItems& items,
					   const uint64_t createdAtMs)
	{
		auto* entry = outbox.add_entries();
		entry->set_request_id(requestId);
		entry->set_source(static_cast<uint32_t>(source));
		entry->set_source_id(sourceId);
		entry->set_title(title);
		entry->set_body(body);
		for (const auto& [configId, count] : items)
		{
			auto* item = entry->add_items();
			item->set_config_id(configId);
			item->set_count(count);
		}
		entry->set_created_at_ms(createdAtMs);
	}

	// 摘掉这个号的全部条目(坏数据里可能重复),返回摘掉了几条;其余条目的相对顺序不变。
	inline std::size_t Remove(PlayerMailOutboxComp& outbox, const uint64_t requestId)
	{
		std::size_t removed = 0;
		auto* entries = outbox.mutable_entries();
		for (int index = entries->size() - 1; index >= 0; --index)
		{
			if (entries->Get(index).request_id() == requestId)
			{
				entries->DeleteSubrange(index, 1);
				++removed;
			}
		}
		return removed;
	}

	// 一条记录是不是一封邮件服务肯收的信:物品 1..16 种、按 config_id 严格升序、数量非 0、合计不超过 4096 件,标题非空。
	// 空串 = 合法;否则是第一处问题的原因(只进日志)。
	inline std::string ValidateEntry(const MailOutboxEntry& entry)
	{
		if (entry.request_id() == 0)
		{
			return "zero_request_id";
		}
		if (entry.source() == static_cast<uint32_t>(MailOutboxSource::kUnspecified))
		{
			return "unspecified_source";
		}
		if (entry.title().empty())
		{
			return "empty_title";
		}
		if (entry.items_size() == 0 || static_cast<std::size_t>(entry.items_size()) > kMaxItemLinesPerMail)
		{
			return "bad_item_line_count";
		}
		uint32_t previous = 0;
		uint64_t total = 0;
		for (const auto& item : entry.items())
		{
			if (item.config_id() == 0 || item.count() == 0)
			{
				return "zero_config_or_count";
			}
			if (item.config_id() <= previous)
			{
				return "items_not_ascending";
			}
			if (item.count() > kMaxItemCountPerMail - total)
			{
				return "too_many_items";
			}
			previous = item.config_id();
			total += item.count();
		}
		return {};
	}

	// 加载时的卫生检查:空串 = 合法;否则是第一处问题的原因(只进日志)。
	// 不合法的记录照样原样携带、照样尝试寄送,不在加载时改写:删掉它等于把欠玩家的物品抹掉,而它是不是坏数据要人看。
	// 本版本不认识的来源不算问题。
	inline std::string Validate(const PlayerMailOutboxComp& outbox)
	{
		if (static_cast<std::size_t>(outbox.entries_size()) > kMaxEntries)
		{
			return "too_many_entries";
		}
		std::set<uint64_t> seen;
		for (const auto& entry : outbox.entries())
		{
			if (const auto problem = ValidateEntry(entry); !problem.empty())
			{
				return problem;
			}
			if (!seen.insert(entry.request_id()).second)
			{
				return "duplicate_request_id";
			}
		}
		return {};
	}

	// 一次寄送的结果。底层类型按枚举项个数选;kCount 恒为最后一项。
	enum class SendVerdict : uint8_t
	{
		kDelivered, // 邮件回了 mail_id(含判重回原邮件):可以删条目
		kRejected,  // 邮件明确拒收:邮箱满(等玩家删信),或附件无效 / 请求冲突 / 参数错(要人处理)
		kUnknown,   // 结果未知:没拿到回包、邮件回故障码或本版本不认识的码、成功却没有 mail_id。邮件可能已经写了

		kCount
	};

	// callOk:传输层拿到了回包;tipId:回包 error_message.id;mailId:回包 mail_id。
	// 只有拒收码表里列出的业务拒绝算明确拒收;故障码与码表外的码一律按结果未知(与 go/rank 奖励工作器同一口径:
	// 邮件提交结果未知时回的就是故障码)。两种都不删条目,区别只在重试节奏与日志级别。纯函数。
	constexpr SendVerdict ClassifySend(const bool callOk, const uint32_t tipId, const uint64_t mailId)
	{
		if (!callOk)
		{
			return SendVerdict::kUnknown;
		}
		if (tipId == 0 || tipId == static_cast<uint32_t>(kSuccess))
		{
			return mailId != 0 ? SendVerdict::kDelivered : SendVerdict::kUnknown;
		}
		switch (tipId)
		{
		case static_cast<uint32_t>(kMailRecipientBoxFull):
		case static_cast<uint32_t>(kMailAttachmentInvalid):
		case static_cast<uint32_t>(kMailRequestConflict):
		case static_cast<uint32_t>(kInvalidParameter):
			return SendVerdict::kRejected;
		default:
			return SendVerdict::kUnknown;
		}
	}

	// 只有邮箱满是玩家删信就能自己好的拒收;其余拒收(附件无效、请求冲突、参数错)重试不会变,要人看。只决定日志级别。
	constexpr bool NeedsManualFollowUp(const uint32_t tipId)
	{
		return tipId == static_cast<uint32_t>(kMailAttachmentInvalid) ||
			   tipId == static_cast<uint32_t>(kMailRequestConflict) || tipId == static_cast<uint32_t>(kInvalidParameter);
	}

	// 寄送失败之后多久再试:按连续失败次数(含本次,>= 1)翻倍,最多翻 8 次再封顶。没有放弃:物品不能因为寄不出去而消失。
	//   结果未知(含没发出去):1 秒起,封顶 60 秒 —— 多是邮件短暂不可达,恢复要快;
	//   明确拒收:30 秒起,封顶 5 分钟 —— 邮箱满要等玩家删信,删完最多再等 5 分钟。
	inline constexpr uint64_t kUnknownRetryBaseMs = 1000;
	inline constexpr uint64_t kUnknownRetryCeilingMs = 60ULL * 1000ULL;
	inline constexpr uint64_t kRejectedRetryBaseMs = 30ULL * 1000ULL;
	inline constexpr uint64_t kRejectedRetryCeilingMs = 5ULL * 60ULL * 1000ULL;

	constexpr uint64_t RetryDelayMs(const bool rejected, const uint32_t failStreak)
	{
		const uint64_t base = rejected ? kRejectedRetryBaseMs : kUnknownRetryBaseMs;
		const uint64_t ceiling = rejected ? kRejectedRetryCeilingMs : kUnknownRetryCeilingMs;
		const uint32_t doublings = failStreak == 0 ? 0 : std::min<uint32_t>(failStreak - 1, 8);
		return std::min(base << doublings, ceiling);
	}

	// 失败日志的节拍:连续失败的第 1 次,之后每 8 次一行(同 player_inbox_rules::kStuckWarnEveryFailures 的取法,
	// 封顶节奏下两行落在 [10m] 日志窗口之内)。
	inline constexpr uint32_t kFailureLogEvery = 8;
	constexpr bool IsFailureLogDue(const uint32_t failStreak)
	{
		return failStreak == 1 || (failStreak != 0 && failStreak % kFailureLogEvery == 0);
	}
} // namespace mail_outbox_rules
