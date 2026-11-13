// 玩家域 Redis 的路由契约(docs/design/redis-player-store-cluster.md §5.2、§5.3、§14.1)。
// 把 scene 发往 tlsPlayerRedis 的每一种命令过一遍真正的路由判定,守住三件事:
//   1. 多键 Lua 的 KEYS 同 slot 且都带 hash tag,否则路由层当场拒绝(单实例开发也一样);
//   2. 同一玩家的全部键落在同一个 slot。路由层只在同 slot 内保序,「删锁先于读锁」这类顺序靠的就是这一条;
//   3. 控制域的键和命令上不了这个句柄。
// 文件末尾另有控制集群句柄(tlsControlRedis)的四处用法:同一套路由判定,按控制集群的策略。
// battle 节点不访问 Redis(battle_room_manager.h:结算走 match SQL 与收件箱),这里没有它的命令;
// battle:lock / battle:ctx 的写者是 scene。
//
// 只测纯函数:不连 Redis、不建实体。
// 新增一段发往 tlsPlayerRedis 的脚本,在 FenceScripts 或下面的用例里加一行。漏了也不会静默出错:运行时的守卫同样拒绝。

#include <gtest/gtest.h>

#include <hiredis/hiredis.h>

#include <cstddef>
#include <cstdint>
#include <initializer_list>
#include <limits>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

#include "battle/system/battle_lock_rules.h"
#include "engine/infra/storage/redis_cluster/redis_command_route.h"
#include "engine/infra/storage/redis_cluster/redis_slot.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/handoff_sql_barrier.h"
#include "player/system/player_landing.h"
#include "player/system/player_mystery_merchant.h"
#include "player/system/player_scene.h"
#include "player/system/relocate_confirm.h"
#include "world/scene_ownership_rules.h"

namespace
{
	using redis_cluster::CommandRoute;
	using redis_cluster::HashTagPolicy;
	using redis_cluster::ResolveCommandRoute;
	using redis_cluster::RouteKind;
	using redis_cluster::RouteRejectReason;

	const std::vector<uint64_t> kPlayerIds = {1, 42, 12345, 950200002, (std::numeric_limits<uint64_t>::max)()};

	std::string Resp(std::initializer_list<std::string_view> args)
	{
		std::string out = "*" + std::to_string(args.size()) + "\r\n";
		for (const std::string_view arg : args)
		{
			out += "$" + std::to_string(arg.size()) + "\r\n";
			out += arg;
			out += "\r\n";
		}
		return out;
	}

	std::string Eval(std::string_view script, const std::vector<std::string>& keys)
	{
		std::string out = "*" + std::to_string(keys.size() + 4) + "\r\n";
		const auto append = [&out](std::string_view arg) {
			out += "$" + std::to_string(arg.size()) + "\r\n";
			out += arg;
			out += "\r\n";
		};
		const std::string numKeys = std::to_string(keys.size());
		append("EVAL");
		append(script);
		append(numKeys);
		for (const std::string& key : keys)
		{
			append(key);
		}
		append("argv");
		return out;
	}

	// 与生产的玩家域句柄同一组策略(cpp/libs/services/scene/core/system/redis.cpp:kAllKeys + KeyDomain::kPlayer)。
	CommandRoute PlayerRoute(const std::string& formatted)
	{
		return ResolveCommandRoute(formatted, std::nullopt, HashTagPolicy::kAllKeys, redis_cluster::KeyDomain::kPlayer);
	}

	// 与生产的控制集群句柄同一组策略(同一文件:kMultiKeyOnly + KeyDomain::kControl)。
	CommandRoute ControlRoute(const std::string& formatted)
	{
		return ResolveCommandRoute(formatted, std::nullopt, HashTagPolicy::kMultiKeyOnly, redis_cluster::KeyDomain::kControl);
	}

	// 按生产的格式串格式化:{%llu} 这种键形要过 hiredis 才知道最后长什么样。
	template <typename... Args>
	std::string Formatted(const std::string& format, Args... args)
	{
		char* raw = nullptr;
		const int length = ::redisFormatCommand(&raw, format.c_str(), args...);
		if (length < 0)
		{
			ADD_FAILURE() << "hiredis could not format: " << format;
			return {};
		}
		std::string formatted(raw, static_cast<std::size_t>(length));
		::redisFreeCommand(raw);
		return formatted;
	}

	void ExpectRoutesToPlayer(const std::string& formatted, uint64_t playerId, std::string_view what)
	{
		const CommandRoute route = PlayerRoute(formatted);
		ASSERT_EQ(route.kind, RouteKind::kSlot)
			<< what << " player=" << playerId << " rejected: " << redis_cluster::RouteRejectReasonName(route.reason)
			<< " key=" << route.offendingKey;
		EXPECT_EQ(route.slot, redis_slot::PlayerSlot(playerId)) << what << " player=" << playerId;
	}

	struct FenceScript
	{
		std::string_view name;
		std::string_view script;
	};

	// 脚本串是有动态初始化的 inline 变量,所以在用例里取,不放进全局表。
	// KEYS 依次是 owner_epoch、handoff_fence、handoff。
	// player_lifecycle.cpp:撤回、A1′ 与补写、会话结束凭证、A2′、发布标记。role_admission_system.cpp:登记、撤销登记。
	std::vector<FenceScript> FenceScripts()
	{
		return {
			{"kRevokeFenceLua", handoff_sql_barrier::kRevokeFenceLua},
			{"kReleaseMarkLua", handoff_sql_barrier::kReleaseMarkLua},
			{"kCredentialMarkLua", handoff_sql_barrier::kCredentialMarkLua},
			{"kConfirmFenceLua", handoff_sql_barrier::kConfirmFenceLua},
			{"kPublishMarkDirectLua", handoff_sql_barrier::kPublishMarkDirectLua},
			{"kRegisterFenceLua", handoff_sql_barrier::kRegisterFenceLua},
			{"kCancelRegistrationLua", handoff_sql_barrier::kCancelRegistrationLua},
		};
	}

	// KEYS 比上面多一个 location。player_lifecycle.cpp:取证、恢复。
	std::vector<FenceScript> FenceScriptsWithLocation()
	{
		return {
			{"kJudgeOutcomeLua", handoff_sql_barrier::kJudgeOutcomeLua},
			{"kResumeFenceLua", handoff_sql_barrier::kResumeFenceLua},
		};
	}

	TEST(PlayerRedisRouteContract, FenceScriptsStayInThePlayersSlot)
	{
		for (const uint64_t playerId : kPlayerIds)
		{
			const std::string owner = player_ownership::OwnerEpochRedisKey(playerId);
			const std::string fence = player_ownership::HandoffFenceRedisKey(playerId);
			const std::string mark = player_ownership::HandoffRedisKey(playerId);
			const std::string location = player_ownership::LocationRedisKey(playerId);
			for (const FenceScript& entry : FenceScripts())
			{
				ExpectRoutesToPlayer(Eval(entry.script, {owner, fence, mark}), playerId, entry.name);
			}
			for (const FenceScript& entry : FenceScriptsWithLocation())
			{
				ExpectRoutesToPlayer(Eval(entry.script, {owner, fence, mark, location}), playerId, entry.name);
			}
			// 改派核实的只读脚本:owner_epoch、location、handoff。
			ExpectRoutesToPlayer(Eval(relocate_confirm::kLuaReadPlacement, {owner, location, mark}), playerId, "kLuaReadPlacement");
		}
	}

	TEST(PlayerRedisRouteContract, ScriptsTouchOnlyDeclaredKeys)
	{
		// Cluster 只替 KEYS 做同 slot 校验:脚本里现拼的键不受任何检查,迁移时还可能读到空。
		const auto expectNoLiteralKeys = [](const FenceScript& entry) {
			EXPECT_EQ(entry.script.find("player:"), std::string_view::npos) << entry.name;
			EXPECT_EQ(entry.script.find("battle:"), std::string_view::npos) << entry.name;
		};
		for (const FenceScript& entry : FenceScripts())
		{
			expectNoLiteralKeys(entry);
		}
		for (const FenceScript& entry : FenceScriptsWithLocation())
		{
			expectNoLiteralKeys(entry);
		}
	}

	TEST(PlayerRedisRouteContract, BattleLockScriptsStayInThePlayersSlot)
	{
		// player_battle.cpp 的 7 处 EVAL 用同一组 KEYS,脚本是该文件私有的;路由只看 KEYS,所以这里用占位脚本。
		const std::string keys = std::string(" ") + battle_lock::kBattleLockKeyFmt + " " + battle_lock::kBattleCtxKeyFmt;
		for (const uint64_t playerId : kPlayerIds)
		{
			const auto id = static_cast<unsigned long long>(playerId);
			ExpectRoutesToPlayer(Formatted("EVAL %s 2" + keys + " %llu", "return 1", id, id, 7ULL), playerId, "conditional delete / read ctx");
			ExpectRoutesToPlayer(Formatted("EVAL %s 2" + keys, "return 1", id, id), playerId, "read lock and ctx");
		}
	}

	TEST(PlayerRedisRouteContract, SingleKeyCommandsShareTheSlotSoStopAndWaitKeepsTheirOrder)
	{
		for (const uint64_t playerId : kPlayerIds)
		{
			const auto id = static_cast<unsigned long long>(playerId);
			const std::string lockFormat = battle_lock::kBattleLockKeyFmt;
			const std::string ctxFormat = battle_lock::kBattleCtxKeyFmt;
			const std::string digestFormat = player_landing::kSyncDigestKeyFmt;
			const std::string fence = player_ownership::HandoffFenceRedisKey(playerId);
			const std::string location = player_ownership::LocationRedisKey(playerId);

			ExpectRoutesToPlayer(Formatted("SET " + lockFormat + " %llu EX %llu", id, 7ULL, 60ULL), playerId, "SET battle:lock");
			ExpectRoutesToPlayer(Formatted("SET " + ctxFormat + " %s EX %llu", id, "ctx", 60ULL), playerId, "SET battle:ctx");
			ExpectRoutesToPlayer(Formatted("GET " + lockFormat, id), playerId, "GET battle:lock");
			ExpectRoutesToPlayer(Formatted("HGETALL " + digestFormat, id), playerId, "HGETALL sync_digest");
			ExpectRoutesToPlayer(Resp({"HGET", fence, "handoff_fence"}), playerId, "HGET handoff_fence");
			ExpectRoutesToPlayer(Resp({"GET", location}), playerId, "GET location");
		}
	}

	TEST(PlayerRedisRouteContract, ControlDomainStaysOffThePlayerHandle)
	{
		// 控制域的键带本 zone 的控制集群 tag {ctl:<z>}(docs/design/redis-cluster-all-domains-20261110.md §3),发到玩家域
		// 句柄当场拒绝,原因是 tag 属于另一个域。两把键取自生产的拼键函数,同时钉住与 Go 侧逐字节相同的键形
		// (go/scene_manager 的 worldChannelDirectoryKey、mysteryMerchantKey)。
		const std::string directoryKey = PlayerSceneSystem::WorldChannelDirectoryKey(1, 2);
		const std::string merchantKey = PlayerMysteryMerchantSystem::RecordKey(1);
		EXPECT_EQ(directoryKey, "world_channel_directory:{ctl:1}:2");
		EXPECT_EQ(merchantKey, "mystery_merchant:{ctl:1}");
		EXPECT_EQ(PlayerRoute(Resp({"GET", directoryKey})).reason, RouteRejectReason::kWrongDomainTag);
		EXPECT_EQ(PlayerRoute(Resp({"HGET", merchantKey, "state"})).reason, RouteRejectReason::kWrongDomainTag);
		// 唤醒的分片频道带控制集群 aw 族的 tag,发到玩家域句柄同样按域拒绝。
		// 玩家域也不用 PUBLISH(Cluster 下普通 PUBLISH 全集群广播,设计 §5.12)。
		const std::string wakeChannel = player_landing::AssetWakeChannel(42);
		EXPECT_EQ(PlayerRoute(Resp({"SPUBLISH", wakeChannel, "42"})).reason, RouteRejectReason::kWrongDomainTag);
		EXPECT_EQ(PlayerRoute(Resp({"PUBLISH", wakeChannel, "42"})).reason, RouteRejectReason::kUnknownCommand);
	}

	// ---- 控制集群句柄 tlsControlRedis(docs/design/redis-cluster-all-domains-20261110.md §4.2、§4.6)----
	// scene 在这个句柄上只有四处用法:三处读落在本 zone 的 {ctl:<z>} 那一个 slot,唤醒按 player_id 分到 aw 族的分片频道。
	// 键与频道都取自生产的拼键函数(同 scene_ownership_reconciler.cpp、player_scene.cpp、player_mystery_merchant.cpp、
	// player_landing.cpp),漏改哪一处,这里与运行时的守卫一样当场拒绝。

	TEST(ControlRedisRouteContract, SceneReadsLandOnTheZoneSlot)
	{
		constexpr uint32_t kZone = 7;
		const uint16_t zoneSlot =
			redis_slot::HashSlot(redis_slot::ZoneControlTag(redis_slot::ControlFamily::kSceneControl, kZone));
		const auto expectZoneSlot = [zoneSlot](const std::string& formatted, std::string_view what) {
			const CommandRoute route = ControlRoute(formatted);
			ASSERT_EQ(route.kind, RouteKind::kSlot)
				<< what << " rejected: " << redis_cluster::RouteRejectReasonName(route.reason) << " key=" << route.offendingKey;
			EXPECT_EQ(route.slot, zoneSlot) << what;
		};
		expectZoneSlot(Resp({"GET", PlayerSceneSystem::WorldChannelDirectoryKey(kZone, 2)}), "GET world channel directory");
		expectZoneSlot(Resp({"HGET", PlayerMysteryMerchantSystem::RecordKey(kZone), "state"}), "HGET mystery merchant");
		// 归属对账一批:本节点号的持有者加若干场景的属主键,同一个 tag。
		expectZoneSlot(Resp({"MGET", scene_ownership::NodeHolderKey(kZone, "3"), scene_ownership::SceneNodeKey(kZone, 101),
							 scene_ownership::SceneNodeKey(kZone, 102)}),
					   "MGET ownership batch");
	}

	TEST(ControlRedisRouteContract, OwnershipReadRefusesKeysOfAnotherZone)
	{
		// 对账只按本节点的 zone 拼键;混进别的 zone 的场景键时当场拒绝(碰巧同 slot 也拒绝),不会发出跨槽的 MGET。
		const CommandRoute route =
			ControlRoute(Resp({"MGET", scene_ownership::NodeHolderKey(7, "3"), scene_ownership::SceneNodeKey(8, 101)}));
		EXPECT_EQ(route.kind, RouteKind::kRejected);
		EXPECT_EQ(route.reason, RouteRejectReason::kMixedTags);
	}

	TEST(ControlRedisRouteContract, AssetWakeShardsByPlayerId)
	{
		// 分片规则与资产通道的工作分片相同(player_id % 64),频道名与 Go 侧 go/shared/assetopwake 逐字一致。
		EXPECT_EQ(player_landing::kAssetWakeShardCount, 64u);
		EXPECT_EQ(player_landing::AssetWakeChannel(130), "assetop:wake:{aw:2}");
		EXPECT_EQ(player_landing::AssetWakeChannel(63), "assetop:wake:{aw:63}");
		EXPECT_EQ(player_landing::AssetWakeChannel(64), "assetop:wake:{aw:0}");
		// 与 Go 侧 go/shared/assetopwake 的金样同一组值(130、64、uint64 上限)。
		EXPECT_EQ(player_landing::AssetWakeChannel((std::numeric_limits<uint64_t>::max)()), "assetop:wake:{aw:63}");
		for (const uint64_t playerId : kPlayerIds)
		{
			const std::string channel = player_landing::AssetWakeChannel(playerId);
			const CommandRoute route =
				ControlRoute(Formatted("SPUBLISH %s %llu", channel.c_str(), static_cast<unsigned long long>(playerId)));
			ASSERT_EQ(route.kind, RouteKind::kSlot)
				<< "player=" << playerId << " rejected: " << redis_cluster::RouteRejectReasonName(route.reason);
			EXPECT_EQ(route.slot,
					  redis_slot::HashSlot(redis_slot::ControlTag(redis_slot::ControlFamily::kAssetWake,
																  std::to_string(playerId % player_landing::kAssetWakeShardCount))))
				<< "player=" << playerId;
		}
		// 集群里普通 PUBLISH 经总线广播到每个节点,控制集群句柄同样不放行。
		EXPECT_EQ(ControlRoute(Resp({"PUBLISH", player_landing::AssetWakeChannel(42), "42"})).reason,
				  RouteRejectReason::kUnknownCommand);
	}
}
