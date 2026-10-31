#include "redis_command_route.h"

#include <charconv>
#include <system_error>

#include "redis_slot.h"

namespace redis_cluster
{
	namespace
	{
		enum class KeyLayout : uint8_t
		{
			kFirstArg,        // 第 1 个参数是键
			kAllArgs,         // 全部参数都是键
			kScriptNumKeys,   // args[2] 是 numkeys,其后 numkeys 个参数是键
			kNoKeys,          // 没有键,必须显式给路由键
			kScriptAdmin,     // SCRIPT:只放行 LOAD,发往任一已连接节点
		};

		struct CommandKeySpec
		{
			std::string_view name;
			KeyLayout layout;
		};

		constexpr CommandKeySpec kCommandKeySpecs[] = {
			{"EVAL", KeyLayout::kScriptNumKeys},
			{"EVALSHA", KeyLayout::kScriptNumKeys},
			{"GET", KeyLayout::kFirstArg},
			{"SET", KeyLayout::kFirstArg},
			{"HGET", KeyLayout::kFirstArg},
			{"HSET", KeyLayout::kFirstArg},
			{"HMGET", KeyLayout::kFirstArg},
			{"HGETALL", KeyLayout::kFirstArg},
			{"ZADD", KeyLayout::kFirstArg},
			{"ZRANGE", KeyLayout::kFirstArg},
			{"ZREM", KeyLayout::kFirstArg},
			{"EXPIRE", KeyLayout::kFirstArg},
			{"DEL", KeyLayout::kAllArgs},
			{"EXISTS", KeyLayout::kAllArgs},
			{"MGET", KeyLayout::kAllArgs},
			{"PING", KeyLayout::kNoKeys},
			{"SCRIPT", KeyLayout::kScriptAdmin},
		};

		// upper 必须已是大写。Redis 的命令名不区分大小写。
		bool EqualsIgnoreAsciiCase(std::string_view text, std::string_view upper)
		{
			if (text.size() != upper.size())
			{
				return false;
			}
			for (std::size_t i = 0; i < text.size(); ++i)
			{
				char c = text[i];
				if (c >= 'a' && c <= 'z')
				{
					c = static_cast<char>(c - 'a' + 'A');
				}
				if (c != upper[i])
				{
					return false;
				}
			}
			return true;
		}

		const CommandKeySpec* FindCommandKeySpec(std::string_view name)
		{
			for (const CommandKeySpec& spec : kCommandKeySpecs)
			{
				if (EqualsIgnoreAsciiCase(name, spec.name))
				{
					return &spec;
				}
			}
			return nullptr;
		}

		// 读一行 "<prefix><非负十进制>\r\n",成功时把 pos 推到 \r\n 之后。
		bool ReadLengthLine(std::string_view data, std::size_t& pos, char prefix, std::size_t& value)
		{
			if (pos >= data.size() || data[pos] != prefix)
			{
				return false;
			}
			const std::size_t crlf = data.find("\r\n", pos + 1);
			if (crlf == std::string_view::npos)
			{
				return false;
			}
			const char* first = data.data() + pos + 1;
			const char* last = data.data() + crlf;
			const std::from_chars_result parsed = std::from_chars(first, last, value);
			if (parsed.ec != std::errc() || parsed.ptr != last)
			{
				return false;
			}
			pos = crlf + 2;
			return true;
		}

		std::optional<std::size_t> ParseNumKeys(std::string_view text)
		{
			std::size_t value = 0;
			const char* first = text.data();
			const char* last = first + text.size();
			const std::from_chars_result parsed = std::from_chars(first, last, value);
			if (parsed.ec != std::errc() || parsed.ptr != last)
			{
				return std::nullopt;
			}
			return value;
		}

		CommandRoute Reject(RouteRejectReason reason, std::string_view commandName, std::string_view offendingKey = {})
		{
			CommandRoute route;
			route.kind = RouteKind::kRejected;
			route.reason = reason;
			route.commandName = commandName;
			route.offendingKey = offendingKey;
			return route;
		}

		CommandRoute RouteToSlot(uint16_t slot, std::string_view commandName)
		{
			CommandRoute route;
			route.kind = RouteKind::kSlot;
			route.slot = slot;
			route.commandName = commandName;
			return route;
		}

		CommandRoute RouteToAnyNode(std::string_view commandName)
		{
			CommandRoute route;
			route.kind = RouteKind::kAnyNode;
			route.commandName = commandName;
			return route;
		}
	}

	std::optional<std::vector<std::string_view>> ParseFormattedCommand(std::string_view formatted)
	{
		std::size_t pos = 0;
		std::size_t argc = 0;
		if (!ReadLengthLine(formatted, pos, '*', argc) || argc == 0)
		{
			return std::nullopt;
		}
		std::vector<std::string_view> args;
		// argc 来自入参,畸形缓冲可能写得很大,不能照着它预留内存。
		constexpr std::size_t kMaxReserve = 64;
		args.reserve(argc < kMaxReserve ? argc : kMaxReserve);
		for (std::size_t i = 0; i < argc; ++i)
		{
			std::size_t length = 0;
			if (!ReadLengthLine(formatted, pos, '$', length))
			{
				return std::nullopt;
			}
			const std::size_t remaining = formatted.size() - pos;
			if (length > remaining || remaining - length < 2)
			{
				return std::nullopt;
			}
			if (formatted[pos + length] != '\r' || formatted[pos + length + 1] != '\n')
			{
				return std::nullopt;
			}
			args.push_back(formatted.substr(pos, length));
			pos += length + 2;
		}
		if (pos != formatted.size())
		{
			return std::nullopt;
		}
		return args;
	}

	CommandRoute ResolveCommandRoute(std::string_view formatted, std::optional<std::string_view> routingKey, HashTagPolicy tagPolicy)
	{
		const std::optional<std::vector<std::string_view>> parsed = ParseFormattedCommand(formatted);
		if (!parsed)
		{
			return Reject(RouteRejectReason::kMalformedCommand, {});
		}
		const std::vector<std::string_view>& args = *parsed;
		const std::string_view name = args.front();
		const CommandKeySpec* spec = FindCommandKeySpec(name);
		if (spec == nullptr)
		{
			return Reject(RouteRejectReason::kUnknownCommand, name);
		}
		if (routingKey && routingKey->empty())
		{
			return Reject(RouteRejectReason::kEmptyRoutingKey, name);
		}

		std::size_t firstKey = 1;
		std::size_t keyCount = 0;
		switch (spec->layout)
		{
		case KeyLayout::kFirstArg:
			if (args.size() < 2)
			{
				return Reject(RouteRejectReason::kMalformedCommand, name);
			}
			keyCount = 1;
			break;
		case KeyLayout::kAllArgs:
			if (args.size() < 2)
			{
				return Reject(RouteRejectReason::kMalformedCommand, name);
			}
			keyCount = args.size() - 1;
			break;
		case KeyLayout::kScriptNumKeys:
		{
			if (args.size() < 3)
			{
				return Reject(RouteRejectReason::kMalformedCommand, name);
			}
			const std::optional<std::size_t> numKeys = ParseNumKeys(args[2]);
			if (!numKeys || *numKeys > args.size() - 3)
			{
				return Reject(RouteRejectReason::kBadNumKeys, name);
			}
			firstKey = 3;
			keyCount = *numKeys;
			break;
		}
		case KeyLayout::kNoKeys:
			break;
		case KeyLayout::kScriptAdmin:
			if (args.size() < 3 || !EqualsIgnoreAsciiCase(args[1], "LOAD"))
			{
				return Reject(RouteRejectReason::kUnknownCommand, name);
			}
			if (!routingKey)
			{
				return RouteToAnyNode(name);
			}
			break;
		}

		if (keyCount == 0)
		{
			if (!routingKey)
			{
				return Reject(RouteRejectReason::kKeylessNeedsRoutingKey, name);
			}
			if (tagPolicy == HashTagPolicy::kAllKeys && !redis_slot::HasHashTag(*routingKey))
			{
				return Reject(RouteRejectReason::kMissingHashTag, name, *routingKey);
			}
			return RouteToSlot(redis_slot::HashSlot(*routingKey), name);
		}

		const uint16_t slot = redis_slot::HashSlot(args[firstKey]);
		if (keyCount > 1 || tagPolicy == HashTagPolicy::kAllKeys)
		{
			for (std::size_t i = firstKey; i < firstKey + keyCount; ++i)
			{
				if (!redis_slot::HasHashTag(args[i]))
				{
					return Reject(RouteRejectReason::kMissingHashTag, name, args[i]);
				}
				if (redis_slot::HashSlot(args[i]) != slot)
				{
					return Reject(RouteRejectReason::kCrossSlot, name, args[i]);
				}
			}
		}
		if (routingKey && redis_slot::HashSlot(*routingKey) != slot)
		{
			return Reject(RouteRejectReason::kRoutingKeyMismatch, name, *routingKey);
		}
		return RouteToSlot(slot, name);
	}
}
