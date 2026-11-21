#pragma once

// 角色改名的纯规则(docs/design/rename-and-content-safety.md §4):零 ECS、零时钟、零网络依赖,供单测直接覆盖。
// 粘合逻辑在 player_rename.cpp(PlayerRenameSystem)。

#include <algorithm>
#include <cstdint>
#include <string>
#include <string_view>

#include "proto/data_service/data_service.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/login_error_tip.pb.h"

namespace player_rename
{
	inline constexpr uint32_t kStructuralMaxRunes = 32;
	inline constexpr uint32_t kMaxDurationSeconds = 365u * 86400u;
	inline constexpr uint64_t kHoldReviewAfterMs = 10ull * 60ull * 1000ull; // 10 分钟复核提示

	// RoleNameRule 表第一行中改名用到的字段(按值拷贝,不持有表指针)。
	struct Rule
	{
		uint32_t minChars{0};
		uint32_t maxChars{0};
		uint32_t cooldownSeconds{0};
		uint32_t oldNameHoldSeconds{0};
		uint32_t renameItemId{0};
		uint32_t renameItemCount{0};
	};

	// 校验规则表取值。合法返回 nullptr,不合法返回英文原因(供启动/调用日志记录)。
	constexpr const char *RuleProblem(const Rule &rule)
	{
		if (rule.minChars == 0 || rule.maxChars == 0 || rule.minChars > rule.maxChars || rule.maxChars > kStructuralMaxRunes)
		{
			return "min_chars/max_chars out of range 1..32";
		}
		if (rule.cooldownSeconds > kMaxDurationSeconds)
		{
			return "rename_cooldown_seconds exceeds 365 days";
		}
		if (rule.oldNameHoldSeconds > kMaxDurationSeconds)
		{
			return "old_name_hold_seconds exceeds 365 days";
		}
		if (rule.renameItemId == 0)
		{
			return "rename_item_id must be non-zero";
		}
		if (rule.renameItemCount == 0)
		{
			return "rename_item_count must be >= 1";
		}
		return nullptr;
	}

	// 去首尾 ASCII 空白并做基础 UTF-8 解码、控制字符拒绝、码点数范围检查。
	// 完整的 NFKC 规范化与字符集白名单以 data_service(go/shared/playername) 为权威。
	inline bool TrimAndValidateBasic(std::string_view raw, uint32_t minChars, uint32_t maxChars, std::string &outTrimmed)
	{
		std::size_t begin = 0;
		std::size_t end = raw.size();
		const auto isAsciiSpace = [](unsigned char ch) {
			return ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n';
		};
		while (begin < end && isAsciiSpace(static_cast<unsigned char>(raw[begin])))
		{
			++begin;
		}
		while (end > begin && isAsciiSpace(static_cast<unsigned char>(raw[end - 1])))
		{
			--end;
		}
		const std::string_view trimmed = raw.substr(begin, end - begin);
		if (trimmed.empty())
		{
			return false;
		}

		uint32_t runes = 0;
		for (std::size_t i = 0; i < trimmed.size();)
		{
			const unsigned char c0 = static_cast<unsigned char>(trimmed[i]);
			uint32_t cp = 0;
			std::size_t len = 0;
			if (c0 < 0x80)
			{
				cp = c0;
				len = 1;
			}
			else if ((c0 & 0xE0) == 0xC0)
			{
				len = 2;
				cp = c0 & 0x1F;
			}
			else if ((c0 & 0xF0) == 0xE0)
			{
				len = 3;
				cp = c0 & 0x0F;
			}
			else if ((c0 & 0xF8) == 0xF0)
			{
				len = 4;
				cp = c0 & 0x07;
			}
			else
			{
				return false;
			}
			if (i + len > trimmed.size())
			{
				return false;
			}
			for (std::size_t j = 1; j < len; ++j)
			{
				const unsigned char cx = static_cast<unsigned char>(trimmed[i + j]);
				if ((cx & 0xC0) != 0x80)
				{
					return false;
				}
				cp = (cp << 6) | (cx & 0x3F);
			}
			// 拒绝过长编码、UTF-16 代理对、超范围码点以及 ASCII / C1 控制字符。
			if ((len == 2 && cp < 0x80) || (len == 3 && cp < 0x800) || (len == 4 && cp < 0x10000) ||
				cp > 0x10FFFF || (cp >= 0xD800 && cp <= 0xDFFF) || cp < 0x20 || cp == 0x7F || (cp >= 0x80 && cp <= 0x9F))
			{
				return false;
			}
			++runes;
			if (runes > maxChars)
			{
				return false;
			}
			i += len;
		}
		if (runes < minChars)
		{
			return false;
		}
		outTrimmed.assign(trimmed.data(), trimmed.size());
		return true;
	}

	constexpr uint64_t NextAllowedMs(uint64_t renamedMs, uint32_t cooldownSeconds)
	{
		if (renamedMs == 0 || cooldownSeconds == 0)
		{
			return 0;
		}
		const uint64_t deltaMs = static_cast<uint64_t>(cooldownSeconds) * 1000ull;
		return renamedMs > UINT64_MAX - deltaMs ? UINT64_MAX : renamedMs + deltaMs;
	}

	constexpr bool IsInCooldown(uint64_t renamedMs, uint32_t cooldownSeconds, uint64_t nowMs)
	{
		const uint64_t nextMs = NextAllowedMs(renamedMs, cooldownSeconds);
		return nextMs != 0 && nowMs < nextMs;
	}

	// 瞬时故障(BUSY / gRPC 传输失败)的指数退避毫秒数,封顶 30 秒。
	constexpr uint64_t RetryDelayMs(uint32_t attempts)
	{
		constexpr uint64_t kCapMs = 30'000;
		if (attempts <= 1)
		{
			return 1'000;
		}
		const uint32_t shift = attempts - 1 >= 5 ? 5 : attempts - 1;
		const uint64_t delay = 1'000ull << shift;
		return delay > kCapMs ? kCapMs : delay;
	}

	constexpr bool IsTransientResult(data_service::RenameRoleResult result)
	{
		return result == data_service::RENAME_ROLE_BUSY || result == data_service::RENAME_ROLE_RESULT_UNSPECIFIED;
	}

	// 将 data_service 明确拒绝的枚举映射为客户端提示码。
	constexpr uint32_t TipForRenameResult(data_service::RenameRoleResult result)
	{
		switch (result)
		{
		case data_service::RENAME_ROLE_TAKEN:
			return static_cast<uint32_t>(kRoleNameTaken);
		case data_service::RENAME_ROLE_INVALID:
			return static_cast<uint32_t>(kRoleNameInvalid);
		case data_service::RENAME_ROLE_COOLDOWN:
			return static_cast<uint32_t>(kRoleRenameCooldown);
		case data_service::RENAME_ROLE_FROZEN:
			return static_cast<uint32_t>(kRoleRenameFrozen);
		case data_service::RENAME_ROLE_SAME_NAME:
			return static_cast<uint32_t>(kRoleRenameSameName);
		case data_service::RENAME_ROLE_EXPIRED:
			return static_cast<uint32_t>(kRoleRenameExpired);
		case data_service::RENAME_ROLE_STALE_SEQ:
		case data_service::RENAME_ROLE_NOT_FOUND:
		default:
			return static_cast<uint32_t>(kServiceUnavailable);
		}
	}
} // namespace player_rename
