#include "player/system/player_inbox_registry.h"

#include <algorithm>
#include <array>
#include <cstddef>
#include <iterator>

#include <google/protobuf/descriptor.h>

#include "player/system/gm_sanction_inbox_executor.h"
#include "battle/system/player_battle.h"
#include "proto/player_inbox/player_inbox.pb.h"

namespace player_inbox_registry
{
	namespace
	{
		// ── 执行器表:按 PlayerInboxMessageType 平铺,下标 = 枚举值(设计 §3.2)────────────
		//
		// 不用宏生成(AGENTS §11.2):普通函数指针数组 + static_assert 表长。下标 0 是 UNSPECIFIED,恒为空。
		// 新增类型时先在 proto 里追加枚举值,这里的 static_assert 会立即编不过,提醒补执行器。
		constexpr PlayerInboxExecutorFn kExecutors[] = {
			nullptr,                                       // PLAYER_INBOX_MSG_UNSPECIFIED
			&GmSanctionInboxExecutor::BlockCurrencyGain,   // PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN
			&GmSanctionInboxExecutor::UnblockCurrencyGain, // PLAYER_INBOX_MSG_GM_UNBLOCK_CURRENCY_GAIN
			&GmSanctionInboxExecutor::AttachCurrencyDebt,  // PLAYER_INBOX_MSG_GM_ATTACH_CURRENCY_DEBT
			&PlayerBattleSystem::ApplyInboxSettlement,    // PLAYER_INBOX_MSG_BATTLE_SETTLEMENT
		};
		static_assert(std::size(kExecutors) == static_cast<std::size_t>(player_inbox::PlayerInboxMessageType_ARRAYSIZE),
					  "执行器表与 PlayerInboxMessageType 脱节:proto 加了类型就在这里补执行器");
		// 表按下标对号入座,钉住每一行的枚举值,防止有人在中间插行导致整体错位。
		static_assert(player_inbox::PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN == 1, "执行器表第 1 行错位");
		static_assert(player_inbox::PLAYER_INBOX_MSG_GM_UNBLOCK_CURRENCY_GAIN == 2, "执行器表第 2 行错位");
		static_assert(player_inbox::PLAYER_INBOX_MSG_GM_ATTACH_CURRENCY_DEBT == 3, "执行器表第 3 行错位");
		static_assert(player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT == 4, "执行器表第 4 行错位");

		// 类型规格的缓存(读一次描述符,此后只读)。
		struct TypeEntry
		{
			bool hasSpec{false};
			uint32_t domain{0};
			uint32_t klass{0};
			std::string payloadMessage;
			std::string name{"unknown"};
		};

		using TypeTable = std::array<TypeEntry, static_cast<std::size_t>(player_inbox::PlayerInboxMessageType_ARRAYSIZE)>;
		using DomainTable =
			std::array<PlayerInboxDomainRule, static_cast<std::size_t>(player_inbox::PlayerInboxDomain_ARRAYSIZE)>;

		const TypeTable &Types()
		{
			static const TypeTable table = []
			{
				TypeTable built{};
				const auto *descriptor = player_inbox::PlayerInboxMessageType_descriptor();
				for (int i = 0; i < descriptor->value_count(); ++i)
				{
					const auto *value = descriptor->value(i);
					const int number = value->number();
					if (number < 0 || static_cast<std::size_t>(number) >= built.size())
					{
						continue;
					}
					auto &entry = built[static_cast<std::size_t>(number)];
					entry.name = std::string(value->name());
					const auto &options = value->options();
					if (!options.HasExtension(player_inbox::OptionPlayerInboxType))
					{
						continue;
					}
					const auto &spec = options.GetExtension(player_inbox::OptionPlayerInboxType);
					entry.hasSpec = true;
					entry.domain = static_cast<uint32_t>(spec.domain());
					entry.klass = static_cast<uint32_t>(spec.klass());
					entry.payloadMessage = spec.payload_message();
				}
				return built;
			}();
			return table;
		}

		const DomainTable &Domains()
		{
			static const DomainTable table = []
			{
				DomainTable built{};
				const auto *descriptor = player_inbox::PlayerInboxDomain_descriptor();
				for (int i = 0; i < descriptor->value_count(); ++i)
				{
					const auto *value = descriptor->value(i);
					const int number = value->number();
					if (number <= 0 || static_cast<std::size_t>(number) >= built.size())
					{
						continue;
					}
					const auto &options = value->options();
					if (!options.HasExtension(player_inbox::OptionPlayerInboxDomain))
					{
						continue;
					}
					const auto &spec = options.GetExtension(player_inbox::OptionPlayerInboxDomain);
					auto &rule = built[static_cast<std::size_t>(number)];
					rule.known = true;
					rule.maxPending = spec.max_pending();
					rule.requiresOutOfBattle = spec.requires_out_of_battle();
				}
				return built;
			}();
			return table;
		}

		const std::string kUnknownTypeName = "unknown";
	} // namespace

	PlayerInboxDomainRule DomainRule(uint32_t domain)
	{
		const auto &domains = Domains();
		if (domain >= domains.size())
		{
			return PlayerInboxDomainRule{};
		}
		return domains[domain];
	}

	PlayerInboxExecutorFn FindExecutor(uint32_t msgType, uint32_t domain)
	{
		if (msgType == 0 || msgType >= std::size(kExecutors))
		{
			return nullptr;
		}
		const auto &entry = Types()[msgType];
		if (!entry.hasSpec || entry.domain != domain || domain == 0)
		{
			return nullptr;
		}
		return kExecutors[msgType];
	}

	const std::string &TypeName(uint32_t msgType)
	{
		const auto &types = Types();
		return msgType < types.size() ? types[msgType].name : kUnknownTypeName;
	}

	const std::vector<uint32_t> &ConsumerDomains()
	{
		static const std::vector<uint32_t> domains = []
		{
			std::vector<uint32_t> collected;
			const auto &types = Types();
			for (std::size_t msgType = 1; msgType < types.size(); ++msgType)
			{
				const auto &entry = types[msgType];
				if (!entry.hasSpec || entry.domain == 0 || kExecutors[msgType] == nullptr)
				{
					continue;
				}
				if (std::find(collected.begin(), collected.end(), entry.domain) == collected.end())
				{
					collected.push_back(entry.domain);
				}
			}
			std::sort(collected.begin(), collected.end());
			return collected;
		}();
		return domains;
	}

	std::string SelfCheck()
	{
		const auto *descriptor = player_inbox::PlayerInboxMessageType_descriptor();
		const auto &types = Types();
		for (int i = 0; i < descriptor->value_count(); ++i)
		{
			const auto *value = descriptor->value(i);
			const int number = value->number();
			if (number == 0)
			{
				continue;
			}
			const std::string where = "msg_type " + std::string(value->name()) + "(" + std::to_string(number) + ")";
			if (number < 0 || static_cast<std::size_t>(number) >= types.size())
			{
				return where + ": out of executor table range";
			}
			const auto &entry = types[static_cast<std::size_t>(number)];
			if (!entry.hasSpec)
			{
				return where + ": missing OptionPlayerInboxType";
			}
			if (entry.domain == 0 || !DomainRule(entry.domain).known)
			{
				return where + ": domain " + std::to_string(entry.domain) + " has no OptionPlayerInboxDomain";
			}
			if (entry.klass != static_cast<uint32_t>(player_inbox::PLAYER_INBOX_CLASS_DURABLE))
			{
				return where + ": class " + std::to_string(entry.klass) + " is not DURABLE";
			}
			if (entry.payloadMessage.empty() ||
				google::protobuf::DescriptorPool::generated_pool()->FindMessageTypeByName(entry.payloadMessage) == nullptr)
			{
				return where + ": payload_message '" + entry.payloadMessage + "' does not resolve";
			}
			if (kExecutors[static_cast<std::size_t>(number)] == nullptr)
			{
				return where + ": no executor in player_inbox_registry.cpp";
			}
		}
		return {};
	}
} // namespace player_inbox_registry
