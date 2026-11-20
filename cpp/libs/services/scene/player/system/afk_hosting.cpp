#include "afk_hosting.h"

#include <string>

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"

#include "engine/core/type_define/player_guid_comp.h"
#include "player/system/afk_pass.h"    // ExpiresAt:月卡的事实源就是本实体内存里的那一份
#include "player/system/player_team.h" // OnOfflineHostingStarted:托管开始时由服务器开启跟随
#include "thread_context/ecs_context.h"
#include "time/system/time.h"

namespace
{
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
	// 没有 EventLoop(单测宿主):状态不变,判定要再查时下一次扫描会再来。
	auto *loop = muduo::net::EventLoop::getEventLoopOfCurrentThread();
	if (loop == nullptr)
	{
		return;
	}
	const uint64_t playerId = guid->value;
	const auto lostAt = held->lostAt;
	// 月卡的事实源是本实体内存里的 PlayerUint64Comp.afk_pass_expires_at(加载时从存档来,此后只由 AfkPassSystem 改),
	// 不再读自己写的 Redis 投影:投影一丢就会把有月卡的人当成没有(docs/design/afk-monthly-card.md §5.3)。
	// 读这一下本身是同步的,但判定排到本轮之后执行,与原来等 Redis 回包同一个时序:调用方(断线保留、托管落点入场的
	// EnterScene 第 3.0 步)这一步走完之后才判、才开启跟随,不在进场中途去走跟随检查。
	// 只按值捕获玩家号与这次保留的开始时刻,不捕获实体句柄与 this;执行时重新找实体并核对。
	held->afkProbeInFlight = true;
	++tlsAfkHostingCounters.probes;
	loop->queueInLoop([playerId, lostAt]() {
		const auto entity = FindSamePlayer(playerId);
		const auto *current = entity != entt::null ? tlsEcs.actorRegistry.try_get<PlayerConnectionLostComp>(entity) : nullptr;
		if (current == nullptr || current->lostAt != lostAt || !current->afkProbeInFlight)
		{
			return; // 这一次保留已经结束,或这次查询已经有了结果(单测直接喂 ApplyProbeResult):不再覆盖
		}
		// 与原 Redis 值同一个格式(十进制到期 Unix 秒),判定走同一个入口;0 = 从未开通或已清除,按键不存在处理。
		const uint64_t expiresAt = AfkPassSystem::ExpiresAt(entity);
		ApplyProbeResult(playerId, lostAt, /*failed=*/false, /*found=*/expiresAt != 0, std::to_string(expiresAt),
						 TimeSystem::NowSecondsUTC());
	});
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
