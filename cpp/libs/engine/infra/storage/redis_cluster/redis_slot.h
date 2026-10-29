#pragma once

// 玩家域 Redis 的 slot 契约(docs/design/redis-player-store-cluster.md §5.2),与 Go 的 go/shared/redisslot 逐字一致:
// Redis Cluster 的 HashSlot、玩家 tag、与玩家同槽的按 slot 索引 tag。两侧测试钉同一组金样向量
// (cpp/tests/rpc_controller_test/redis_slot_test.cpp、go/shared/redisslot/redisslot_test.go),改任何一个值都意味着
// 跨语言契约变了,必须两侧同批改;上线后禁止改(会让存量键换槽)。
//
// 键形规则:玩家域单玩家键里的玩家 id 用花括号包起来(PlayerTag);跨玩家的按 slot 索引用 PlayerIndexTag,
// 二者必然同槽,所以一名玩家的键与它所在 slot 的索引可以放进同一段 Lua。
//
// 只做纯计算,无 I/O、无全局可变状态(slot-tag 表是首次使用时构造的只读函数局部静态量,线程安全)。

#include <array>
#include <cassert>
#include <cstdint>
#include <string>
#include <string_view>

namespace redis_slot
{
	// Redis Cluster 的 slot 总数(规范常量)。
	inline constexpr uint16_t kSlotCount = 16384;

	namespace detail
	{
		// CRC16-XMODEM(多项式 0x1021,初值 0)查表,与 Redis 源码 crc16.c 相同。
		constexpr std::array<uint16_t, 256> MakeCrc16Table()
		{
			std::array<uint16_t, 256> table{};
			for (uint32_t i = 0; i < 256; ++i)
			{
				uint16_t crc = static_cast<uint16_t>(i << 8);
				for (int bit = 0; bit < 8; ++bit)
				{
					crc = (crc & 0x8000) != 0 ? static_cast<uint16_t>((crc << 1) ^ 0x1021) : static_cast<uint16_t>(crc << 1);
				}
				table[i] = crc;
			}
			return table;
		}

		inline constexpr std::array<uint16_t, 256> kCrc16Table = MakeCrc16Table();

		// slot-tag 的字母表与最大长度。只用小写字母:纯字母的 tag 不会被误解析成玩家 id。
		inline constexpr std::string_view kSlotTagAlphabet = "abcdefghijklmnopqrstuvwxyz";
		inline constexpr int kSlotTagMaxLen = 4;
	}

	// CRC16-XMODEM。金样:Crc16("123456789") == 0x31C3(Redis Cluster 规范原文例子)。
	constexpr uint16_t Crc16(std::string_view s)
	{
		uint16_t crc = 0;
		for (const char c : s)
		{
			const auto index = static_cast<uint8_t>((crc >> 8) ^ static_cast<uint8_t>(c));
			crc = static_cast<uint16_t>((crc << 8) ^ detail::kCrc16Table[index]);
		}
		return crc;
	}

	// 按 Redis Cluster 规范取键里参与哈希的部分:第一个 '{' 之后第一个 '}' 之前若非空就只取这段,否则取整个键。
	constexpr std::string_view HashTagContent(std::string_view key)
	{
		const auto open = key.find('{');
		if (open == std::string_view::npos)
		{
			return key;
		}
		const auto close = key.find('}', open + 1);
		if (close == std::string_view::npos || close == open + 1)
		{
			return key;
		}
		return key.substr(open + 1, close - open - 1);
	}

	// 键所在的 slot(CRC16 取低 14 位)。
	constexpr uint16_t HashSlot(std::string_view key)
	{
		return static_cast<uint16_t>(Crc16(HashTagContent(key)) & (kSlotCount - 1));
	}

	// 键是否带非空 hash tag。玩家域只接受带 tag 的键(设计 §5.1 的接缝规则)。
	constexpr bool HasHashTag(std::string_view key)
	{
		const auto open = key.find('{');
		if (open == std::string_view::npos)
		{
			return false;
		}
		const auto close = key.find('}', open + 1);
		return close != std::string_view::npos && close != open + 1;
	}

	// 玩家所在的 slot:对 player_id 十进制(无前导零)算 CRC16。
	inline uint16_t PlayerSlot(uint64_t playerId)
	{
		const std::string text = std::to_string(playerId);
		return static_cast<uint16_t>(Crc16(text) & (kSlotCount - 1));
	}

	// 玩家键里的 hash tag,例如 "{42}"。
	inline std::string PlayerTag(uint64_t playerId)
	{
		return "{" + std::to_string(playerId) + "}";
	}

	namespace detail
	{
		// 按「先短后长、同长按字典序」枚举字母表上的字符串,第一个哈希到 slot s 的串就是 s 的 tag。
		// 算法与 Go 侧 buildSlotTags 逐字一致。
		inline std::array<std::string, kSlotCount> BuildSlotTags()
		{
			std::array<std::string, kSlotCount> tags{};
			int filled = 0;
			const auto radix = static_cast<int>(kSlotTagAlphabet.size());
			std::string buf;
			for (int length = 1; length <= kSlotTagMaxLen && filled < kSlotCount; ++length)
			{
				int total = 1;
				for (int i = 0; i < length; ++i)
				{
					total *= radix;
				}
				buf.assign(static_cast<size_t>(length), 'a');
				for (int n = 0; n < total && filled < kSlotCount; ++n)
				{
					int v = n;
					for (int i = length - 1; i >= 0; --i)
					{
						buf[static_cast<size_t>(i)] = kSlotTagAlphabet[static_cast<size_t>(v % radix)];
						v /= radix;
					}
					const auto slot = static_cast<uint16_t>(Crc16(buf) & (kSlotCount - 1));
					if (tags[slot].empty())
					{
						tags[slot] = buf;
						++filled;
					}
				}
			}
			// 长度 4 以内必然覆盖全部 slot(最后填上的是 "cgoq");走不到这里说明实现被改坏了。
			assert(filled == kSlotCount);
			return tags;
		}
	}

	// slot 的 tag 内容(不含花括号),满足 HashSlot("{" + SlotTag(s) + "}") == s。调用方只应传 HashSlot / PlayerSlot 的结果。
	inline const std::string& SlotTag(uint16_t slot)
	{
		static const std::array<std::string, kSlotCount> tags = detail::BuildSlotTags();
		assert(slot < kSlotCount);
		return tags[slot & (kSlotCount - 1)];
	}

	// 与玩家同槽的按 slot 索引键 tag,例如 "{afps}"。补投索引、战斗结算索引等跨玩家索引用它。
	inline std::string PlayerIndexTag(uint64_t playerId)
	{
		return "{" + SlotTag(PlayerSlot(playerId)) + "}";
	}

	// slot 的索引键 tag(含花括号),供按 slot 遍历的后台扫描使用。
	inline std::string SlotIndexTag(uint16_t slot)
	{
		return "{" + SlotTag(slot) + "}";
	}
}
