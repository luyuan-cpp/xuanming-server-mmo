// 玩家域 Redis 的路由契约(docs/design/redis-player-store-cluster.md §5.2、§5.3、§14.1)。
// 把 scene 发往 tlsPlayerRedis 的每一种命令过一遍真正的路由判定,守住三件事:
//   1. 多键 Lua 的 KEYS 同 slot 且都带 hash tag,否则路由层当场拒绝(单实例开发也一样);
//   2. 同一玩家的全部键落在同一个 slot。路由层只在同 slot 内保序,「删锁先于读锁」这类顺序靠的就是这一条;
//   3. 控制域的键和命令上不了这个句柄。
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
#include "player/system/relocate_confirm.h"

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

	CommandRoute PlayerRoute(const std::string& formatted)
	{
		return ResolveCommandRoute(formatted, std::nullopt, HashTagPolicy::kAllKeys);
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
		// 控制域的键不带 hash tag(设计 §5.1),发到玩家域句柄当场拒绝。这两个键形与 player_scene.cpp 的
		// world_channel_directory、player_mystery_merchant.cpp 的 mystery_merchant 相同,常量是那两个文件私有的。
		EXPECT_EQ(PlayerRoute(Resp({"GET", "world_channel_directory:zone:1:2"})).reason, RouteRejectReason::kMissingHashTag);
		EXPECT_EQ(PlayerRoute(Resp({"HGET", "mystery_merchant:zone:1", "state"})).reason, RouteRejectReason::kMissingHashTag);
		// 玩家域不用 PUBLISH(Cluster 下普通 PUBLISH 全集群广播,设计 §5.12)。
		EXPECT_EQ(PlayerRoute(Resp({"PUBLISH", player_landing::kAssetWakeChannel, "42"})).reason, RouteRejectReason::kUnknownCommand);
		EXPECT_FALSE(redis_slot::HasHashTag(player_landing::kAssetWakeChannel));
	}
}
