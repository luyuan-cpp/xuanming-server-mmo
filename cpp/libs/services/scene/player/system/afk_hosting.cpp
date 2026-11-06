#include "afk_hosting.h"

#include <string>

#include <hiredis/hiredis.h>

#include "muduo/base/Logging.h"

#include "engine/core/type_define/player_guid_comp.h"
#include "infra/storage/redis_cluster/redis_cluster_client.h" // tlsPlayerRedis:月卡键属于玩家域
#include "infra/storage/redis_cluster/redis_slot.h"           // PlayerTag:与 go/shared/redisslot 同一键形
#include "player/system/player_team.h"                        // OnOfflineHostingStarted:托管开始时由服务器开启跟随
#include "thread_context/ecs_context.h"
#include "time/system/time.h"

namespace
{
	// 与 go/shared/redisslot.AfkPassKey 同形:player:afk_pass:{<player_id>}。
	std::string AfkPassKey(uint64_t playerId)
	{
		return "player:afk_pass:" + redis_slot::PlayerTag(playerId);
	}

	// 本线程的事件累计,随每条 [AfkHosting] 日志打出(C++ 节点没有 Prometheus 出口,口径同断线保留的计数)。
	struct AfkHostingCounters
	{
		uint64_t probes{0};
		uint64_t probeFailed{0};
		uint64_t hosted{0};
		uint64_t renewed{0};
		uint64_t ended{0};
	};
	thread_local AfkHostingCounters tlsAfkHostingCounters;

	entt::entity FindSamePlayer(uint64_t playerId)
	{
		const auto entity = tlsEcs.GetPlayer(playerId);
		if (entity == entt::null || !tlsEcs.actorRegistry.valid(entity))
		{
			return entt::null;
		}
		const auto *guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(entity);
		return guid != nullptr && guid->value == playerId ? entity : entt::null;
	}
} // namespace

void AfkHostingSystem::Probe(entt::entity player)
{
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return;
	}
	auto *held = registry.try_get<PlayerConnectionLostComp>(player);
	const auto *guid = registry.try_get<PlayerGuidComp>(player);
	if (held == nullptr || guid == nullptr || held->afkProbeInFlight)
	{
		return;
	}
	// 没连上:command 直接返回 REDIS_ERR,不如不发;状态不变,判定要再查时下一次扫描会再来。
	if (!tlsPlayerRedis.connected())
	{
		return;
	}
	const uint64_t playerId = guid->value;
	const auto lostAt = held->lostAt;
	const std::string key = AfkPassKey(playerId);
	// 只按值捕获玩家号与这次保留的开始时刻:回调里重新找实体并核对,不把实体句柄带进异步回调。
	const int status = tlsPlayerRedis.command(
		[playerId, lostAt](hiredis::Hiredis *, redisReply *reply) {
			const bool failed = reply == nullptr || reply->type == REDIS_REPLY_ERROR ||
								(reply->type != REDIS_REPLY_NIL && reply->type != REDIS_REPLY_STRING);
			const bool found = !failed && reply->type == REDIS_REPLY_STRING;
			const std::string_view value = found && reply->str != nullptr
											   ? std::string_view(reply->str, static_cast<std::size_t>(reply->len))
											   : std::string_view{};
			ApplyProbeResult(playerId, lostAt, failed, found, value, TimeSystem::NowSecondsUTC());
		},
		"GET %b", key.data(), key.size());
	if (status != REDIS_OK)
	{
		// 没被收下:回调不会来,不算在途。command 不会同步回调,held 此刻仍有效、也没被改过。
		return;
	}
	held->afkProbeInFlight = true;
	++tlsAfkHostingCounters.probes;
}

void AfkHostingSystem::ApplyProbeResult(uint64_t playerId, connection_lost_hold::Clock::time_point lostAt, bool failed,
	bool found, std::string_view value, uint64_t nowUnixSec)
{
	const auto player = FindSamePlayer(playerId);
	if (player == entt::null)
	{
		return; // 已退出、已销毁或槽位换了人
	}
	auto &registry = tlsEcs.actorRegistry;
	auto *held = registry.try_get<PlayerConnectionLostComp>(player);
	if (held == nullptr || held->lostAt != lostAt)
	{
		return; // 这一次保留已经结束(接回 / 退出),或者是后来的另一次断线:迟到的结果不算数
	}
	held->afkProbeInFlight = false;
	auto &counters = tlsAfkHostingCounters;

	if (failed)
	{
		// 查不出来:状态不变。还不知道的,普通窗口照常计;托管中的,快到期时 JudgeHold 会让下一次扫描再查。
		++counters.probeFailed;
		LOG_WARN << "[AfkHosting] metric=afk_pass_probe_failed player_id=" << playerId
				 << " known_state=" << static_cast<int>(held->afkPass) << " probe_failed_total=" << counters.probeFailed;
		return;
	}

	uint64_t expiresAtSec = 0;
	const auto pass = connection_lost_hold::ParseAfkPass(found, value, nowUnixSec, expiresAtSec);
	const bool wasActive = held->afkPass == connection_lost_hold::AfkPass::kActive;
	held->afkPass = pass;
	held->afkPassExpiresAtSec = expiresAtSec;

	if (pass != connection_lost_hold::AfkPass::kActive)
	{
		if (wasActive)
		{
			// 托管中查到月卡已到期 / 没续(或剩下不到提前退出的余量):下一次扫描按到期退出。
			++counters.ended;
			LOG_INFO << "[AfkHosting] metric=afk_hosting_ended player_id=" << playerId << " found=" << found
					 << " value=" << std::string(value) << " ended_total=" << counters.ended;
		}
		else if (found)
		{
			// 有键但不能托管:已到期、快到期或值坏了。值打出来,值坏了要能看见(与 player_locator 同口径按无月卡处理)。
			LOG_INFO << "[AfkHosting] AFK pass of player " << playerId << " does not allow hosting (value=" << std::string(value)
					 << " now=" << nowUnixSec << "); normal hold window applies";
		}
		return;
	}

	if (held->hostingStarted)
	{
		// 已在托管中:快到期时查到续了(或刚判成不能托管、下一次扫描退出之前又查到有效)。只更新到期时刻。
		++counters.renewed;
		LOG_INFO << "[AfkHosting] metric=afk_hosting_renewed player_id=" << playerId << " expires_at=" << expiresAtSec
				 << " renewed_total=" << counters.renewed;
		return;
	}

	held->hostingStarted = true;
	++counters.hosted;
	LOG_INFO << "[AfkHosting] metric=afk_hosting_started player_id=" << playerId << " expires_at=" << expiresAtSec
			 << " hosted_total=" << counters.hosted << " (character stays in the world until reconnect or pass expiry)";
	// 托管开始:由服务器开启组队跟随,不等客户端声明(team-follow-server-trail.md §3.6)。之后不再用 held:
	// 跟随这一步会挂别的组件、可能发 RPC,不碰保留组件,但没有理由再拿着指针。
	PlayerTeamSystem::OnOfflineHostingStarted(player);
}
