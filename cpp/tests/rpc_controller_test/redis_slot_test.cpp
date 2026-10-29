// 玩家域 Redis slot 契约的金样测试(docs/design/redis-player-store-cluster.md §5.2)。
// 向量由独立实现(Python,按 Redis Cluster 规范)算出,go/shared/redisslot/redisslot_test.go 钉同一组值。

#include <gtest/gtest.h>

#include <cstdint>
#include <string>

#include "engine/infra/storage/redis_cluster/redis_slot.h"

namespace
{
	TEST(RedisSlotTest, Crc16GoldenVector)
	{
		EXPECT_EQ(redis_slot::Crc16("123456789"), 0x31C3);
		static_assert(redis_slot::Crc16("123456789") == 0x31C3, "CRC16 must be usable at compile time");
	}

	TEST(RedisSlotTest, HashSlotGoldenVectors)
	{
		EXPECT_EQ(redis_slot::HashSlot("123456789"), 12739);
		EXPECT_EQ(redis_slot::HashSlot("{user1000}.following"), 3443);
		EXPECT_EQ(redis_slot::HashSlot("foo{}{bar}"), 8363);     // 空 tag:整键参与哈希
		EXPECT_EQ(redis_slot::HashSlot("foo{{bar}}zap"), 4015);  // tag 是 "{bar"
		EXPECT_EQ(redis_slot::HashSlot("foo{bar}{zap}"), 5061);  // 只取第一个 tag
		EXPECT_EQ(redis_slot::HashSlot("PlayerAllData:{42}"), 8000);
		EXPECT_EQ(redis_slot::HashSlot("player:{42}:owner_epoch"), 8000);
		EXPECT_EQ(redis_slot::HashSlot("42"), 8000);
		EXPECT_EQ(redis_slot::HashSlot("1"), 9842);
		EXPECT_EQ(redis_slot::HashSlot("18446744073709551615"), 5255);
	}

	TEST(RedisSlotTest, PlayerSlotAndTag)
	{
		EXPECT_EQ(redis_slot::PlayerSlot(42), 8000);
		EXPECT_EQ(redis_slot::PlayerSlot(UINT64_MAX), 5255);
		EXPECT_EQ(redis_slot::PlayerTag(42), "{42}");
		for (const uint64_t pid : {uint64_t{1}, uint64_t{42}, uint64_t{1000003}, UINT64_MAX})
		{
			const std::string playerKey = "player:" + redis_slot::PlayerTag(pid) + ":owner_epoch";
			const std::string indexKey = "player:save_pending:" + redis_slot::PlayerIndexTag(pid);
			EXPECT_EQ(redis_slot::HashSlot(playerKey), redis_slot::HashSlot(indexKey)) << "pid " << pid;
		}
	}

	TEST(RedisSlotTest, SlotTagGoldenVectors)
	{
		EXPECT_EQ(redis_slot::SlotTag(0), "afps");
		EXPECT_EQ(redis_slot::SlotTag(1), "emj");
		EXPECT_EQ(redis_slot::SlotTag(2), "ahwf");
		EXPECT_EQ(redis_slot::SlotTag(100), "aotc");
		EXPECT_EQ(redis_slot::SlotTag(12739), "aarc");
		EXPECT_EQ(redis_slot::SlotTag(16383), "hia");
	}

	// 全表覆盖每个 slot,且按 slot 顺序以 '\n' 连接后的 FNV-1a 64 与 Go 侧一致(Go 另核 SHA-256)。
	TEST(RedisSlotTest, SlotTagTableCoversEverySlot)
	{
		std::string joined;
		for (uint32_t s = 0; s < redis_slot::kSlotCount; ++s)
		{
			const auto slot = static_cast<uint16_t>(s);
			const std::string& tag = redis_slot::SlotTag(slot);
			ASSERT_FALSE(tag.empty()) << "slot " << s;
			ASSERT_LE(tag.size(), 4u) << "slot " << s;
			ASSERT_EQ(tag.find_first_not_of("abcdefghijklmnopqrstuvwxyz"), std::string::npos) << "slot " << s;
			ASSERT_EQ(redis_slot::HashSlot("{" + tag + "}"), slot) << "tag " << tag;
			if (s > 0)
			{
				joined.push_back('\n');
			}
			joined += tag;
		}
		uint64_t hash = 0xcbf29ce484222325ULL;
		for (const char c : joined)
		{
			hash ^= static_cast<uint8_t>(c);
			hash *= 0x100000001b3ULL;
		}
		EXPECT_EQ(joined.size(), 72467u);
		EXPECT_EQ(hash, 0xf4a7bdc8d20a28e5ULL);
	}

	TEST(RedisSlotTest, HasHashTag)
	{
		EXPECT_TRUE(redis_slot::HasHashTag("PlayerAllData:{42}"));
		EXPECT_TRUE(redis_slot::HasHashTag("player:save_pending:{afps}"));
		EXPECT_FALSE(redis_slot::HasHashTag("PlayerAllData:42"));
		EXPECT_FALSE(redis_slot::HasHashTag("foo{}bar"));
		EXPECT_FALSE(redis_slot::HasHashTag("foo{bar"));
	}
}
