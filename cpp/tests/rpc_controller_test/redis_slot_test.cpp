// 玩家域 Redis slot 契约的金样测试(docs/design/redis-player-store-cluster.md §5.2)。
// 向量由独立实现(Python,按 Redis Cluster 规范)算出,go/shared/redisslot/redisslot_test.go 钉同一组值。

#include <gtest/gtest.h>

#include <cstddef>
#include <cstdint>
#include <set>
#include <string>
#include <string_view>

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

	// ---- 控制集群的族 tag(docs/design/redis-cluster-all-domains-20261110.md §3)----
	// Go 的 go/shared/redisslot/control_keys_test.go 钉同一组金样,并读本头核对族表逐项一致。

	TEST(RedisSlotTest, ControlFamilyTableIsComplete)
	{
		std::set<std::string_view> seen;
		for (std::size_t i = 0; i < static_cast<std::size_t>(redis_slot::ControlFamily::kCount); ++i)
		{
			const std::string_view prefix = redis_slot::ControlFamilyPrefix(static_cast<redis_slot::ControlFamily>(i));
			EXPECT_FALSE(prefix.empty()) << "family " << i;
			EXPECT_EQ(prefix.find_first_of(":{}"), std::string_view::npos) << "family " << i;
			EXPECT_TRUE(seen.insert(prefix).second) << "prefix registered twice: " << prefix;
		}
	}

	TEST(RedisSlotTest, ControlTagGoldenShapes)
	{
		using redis_slot::ControlFamily;
		EXPECT_EQ(redis_slot::ZoneControlTag(ControlFamily::kSceneControl, 7), "{ctl:7}");
		EXPECT_EQ(redis_slot::ZoneControlTag(ControlFamily::kLoginQueue, 3), "{q:3}");
		EXPECT_EQ(redis_slot::ZoneControlTag(ControlFamily::kGateMark, 0), "{gm:0}");
		EXPECT_EQ(redis_slot::ZoneControlTag(ControlFamily::kGuildRank, 12), "{gr:12}");
		EXPECT_EQ(redis_slot::ControlTag(ControlFamily::kAssetWake, "63"), "{aw:63}");
		EXPECT_EQ(redis_slot::ControlTag(ControlFamily::kGuild, "900"), "{g:900}");
		EXPECT_EQ(redis_slot::ControlTag(ControlFamily::kPlayerGuild, "42"), "{pg:42}");
		EXPECT_EQ(redis_slot::ControlTag(ControlFamily::kPlayerLocator, redis_slot::kControlPlayerLocatorIdentity), "{pl:gix}");
		EXPECT_EQ(redis_slot::ControlTag(ControlFamily::kFriend, "42"), "{f:42}");
		EXPECT_EQ(redis_slot::ControlTag(ControlFamily::kFriendRequestQuota, "42"), "{rq:42}");
		EXPECT_EQ(redis_slot::ControlTag(ControlFamily::kFriendDirectory, "42"), "{directory:42}");
		EXPECT_EQ(redis_slot::ControlTag(ControlFamily::kGuildApply, "42"), "{apply:42}");

		// tag 写在原来 zone 或实体号的位置,同一个 zone 的控制域键必然同槽。
		const std::string tag = redis_slot::ZoneControlTag(ControlFamily::kSceneControl, 7);
		EXPECT_EQ(redis_slot::HashSlot("node:" + tag + ":3:scenes"), redis_slot::HashSlot("scene:" + tag + ":1234:node"));
	}

	TEST(RedisSlotTest, IsControlTagSeparatesTheTwoDomains)
	{
		const std::string_view control[] = {"ctl:7", "q:0", "gm:12", "aw:63", "g:1", "pg:1", "gr:1", "pl:gix", "f:1", "rq:1",
			"directory:1", "apply:18446744073709551615", "ctl:7:extra"};
		for (const std::string_view tag : control)
		{
			EXPECT_TRUE(redis_slot::IsControlTag(tag)) << tag;
		}
		const std::string_view notControl[] = {"", "ctl", "ctl:", ":7", "unknown:7", "CTL:7", "player:7",
			"42",                                       // 玩家 tag
			"afps",                                     // slot-tag
			"7a1c2d3e-0b4f-4f6e-9c1d-2e3f4a5b6c7d:3"};  // 登录连接身份
		for (const std::string_view tag : notControl)
		{
			EXPECT_FALSE(redis_slot::IsControlTag(tag)) << tag;
		}
		// 玩家域的 tag 形状一个都不能被认成控制族。
		for (uint32_t s = 0; s < redis_slot::kSlotCount; ++s)
		{
			ASSERT_FALSE(redis_slot::IsControlTag(redis_slot::SlotTag(static_cast<uint16_t>(s)))) << "slot " << s;
		}
		const std::string playerKey = "player:" + redis_slot::PlayerTag(42) + ":location";
		EXPECT_FALSE(redis_slot::IsControlTag(redis_slot::HashTagContent(playerKey)));
	}
}
