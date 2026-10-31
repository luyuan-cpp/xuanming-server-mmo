#include "player_landing.h"
#include "engine/core/type_define/player_guid_comp.h"

#include <chrono>
#include <string>
#include <utility>
#include <vector>

#include "muduo/base/Logging.h"

#include "infra/storage/redis_cluster/redis_cluster_client.h"
#include "thread_context/ecs_context.h"
#include "thread_context/redis_manager.h"
// hiredis 应答类型(redisReply / REDIS_REPLY_* / REDIS_OK)。redis_manager.h 经 muduo 的 Hiredis.h 已带进来,
// 这里显式再包含一次,不靠传递包含拿这些符号(同 player_scene.cpp / player_team.h)。
#include <hiredis/hiredis.h>

#include "network/client_message_sender.h"
#include "rpc/service_metadata/player_scene_service_metadata.h"

#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_landing_comp.h"
#include "player/system/player_inbox_system.h" // OnEnteredScene:收件箱落地拉取(C1)

namespace
{
	uint64_t GuidOf(entt::entity player)
	{
		const auto *guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
		return guid != nullptr ? guid->value : 0;
	}

	// 唤醒频道在控制域(玩家域不用 PUBLISH,redis-player-store-cluster.md §5.12)。同步摘要在玩家域,直接看 tlsPlayerRedis。
	bool ControlRedisReady()
	{
		auto &redis = tlsRedis.GetZoneRedis();
		return redis && redis->connected();
	}

	// 进程内的时间窗口用单调时钟(AGENTS §11.3):系统时间被回拨时,合并窗口不能因此长期不放行。
	// 写法同 movement.cpp 的 SteadyNowMs。
	uint64_t SteadyNowMs()
	{
		return static_cast<uint64_t>(std::chrono::duration_cast<std::chrono::milliseconds>(
			std::chrono::steady_clock::now().time_since_epoch()).count());
	}

	// 异步回调里按 player_id 重新找实体:实体可能已销毁,槽位也可能已被别的玩家复用(Guid 不同)。
	// 找不到返回 entt::null。player_id 为 0 一律找不到:无从核对身份。
	entt::entity FindSamePlayer(uint64_t playerId)
	{
		if (playerId == 0)
		{
			return entt::null;
		}
		const entt::entity player = tlsEcs.GetPlayer(playerId);
		if (player == entt::null || !tlsEcs.actorRegistry.valid(player) || GuidOf(player) != playerId)
		{
			return entt::null;
		}
		return player;
	}

	void PushSyncDigest(entt::entity player, const SyncDigestS2C &digest)
	{
		SendMessageToClientViaGate(SceneSceneClientPlayerNotifySyncDigestMessageId, digest, player);
	}

	// 推一条 status = UNAVAILABLE 的摘要:客户端按全量拉处理,并靠它结束等待。
	void PushUnavailable(entt::entity player)
	{
		SyncDigestS2C digest;
		digest.set_status(SYNC_DIGEST_STATUS_UNAVAILABLE);
		PushSyncDigest(player, digest);
	}

	// HGETALL 应答 → 字段名 / 值对。只接受 RESP2 的数组形态:偶数个元素,每个元素都是字符串
	// (scene 的连接不发 HELLO,不会收到 RESP3 的 MAP)。先判 type 再读 str / len。
	// 返回 false 时 fields 内容不可用。
	bool CollectHashFields(const redisReply *reply, std::vector<std::pair<std::string, std::string>> &fields)
	{
		if (reply == nullptr || reply->type != REDIS_REPLY_ARRAY || reply->elements % 2 != 0)
		{
			return false;
		}
		if (reply->elements > 0 && reply->element == nullptr)
		{
			return false;
		}
		fields.clear();
		fields.reserve(reply->elements / 2);
		for (size_t i = 0; i + 1 < reply->elements; i += 2)
		{
			const redisReply *name = reply->element[i];
			const redisReply *value = reply->element[i + 1];
			if (name == nullptr || value == nullptr || name->type != REDIS_REPLY_STRING ||
				value->type != REDIS_REPLY_STRING || name->str == nullptr || value->str == nullptr)
			{
				return false;
			}
			fields.emplace_back(std::string(name->str, name->len), std::string(value->str, value->len));
		}
		return true;
	}

	// 回调里的应答处理:能解析就推摘要,否则推 UNAVAILABLE。日志分级:
	//   - 空应答(连接断开时 hiredis 用空应答回调挂起命令)/ Redis 报错是读失败:WARN;
	//   - 应答不是数组、元素不是字符串、值不是十进制无符号整数、非空 hash 缺 g,说明 Go 侧 BumpScript 与
	//     本端的键 / 字段契约被破坏(改了一边没改另一边):ERROR(设计 §4.2 第 4 步)。
	void HandleDigestReply(entt::entity player, uint64_t playerId, const redisReply *reply)
	{
		if (reply == nullptr)
		{
			LOG_WARN << "[PlayerLanding] sync digest read got no reply(Redis disconnected);UNAVAILABLE pushed, player_id="
					 << playerId;
			PushUnavailable(player);
			return;
		}
		if (reply->type == REDIS_REPLY_ERROR)
		{
			LOG_WARN << "[PlayerLanding] sync digest read rejected by Redis;UNAVAILABLE pushed, player_id=" << playerId
					 << " err=" << (reply->str != nullptr ? reply->str : "");
			PushUnavailable(player);
			return;
		}

		std::vector<std::pair<std::string, std::string>> fields;
		if (!CollectHashFields(reply, fields))
		{
			LOG_ERROR << "[PlayerLanding] invalid sync digest reply shape;UNAVAILABLE pushed, player_id=" << playerId
					  << " reply_type=" << reply->type << " elements=" << reply->elements;
			PushUnavailable(player);
			return;
		}

		SyncDigestS2C digest;
		if (!PlayerLandingSystem::ParseSyncDigestFields(fields, &digest))
		{
			LOG_ERROR << "[PlayerLanding] malformed sync digest hash;UNAVAILABLE pushed, player_id=" << playerId
					  << " fields=" << fields.size();
			PushUnavailable(player);
			return;
		}
		PushSyncDigest(player, digest);
	}
} // namespace

void PlayerLandingSystem::SendSyncDigest(entt::entity player)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		LOG_WARN << "[PlayerLanding] SendSyncDigest: invalid player entity;push skipped, entity="
				 << entt::to_integral(player);
		return;
	}
	const uint64_t playerId = GuidOf(player);
	// 没有 Guid 就无从在回调里核对"还是不是这名玩家",不走异步;同步推 UNAVAILABLE 让客户端结束等待。
	if (playerId == 0)
	{
		LOG_WARN << "[PlayerLanding] SendSyncDigest: player entity has no Guid;UNAVAILABLE pushed, entity="
				 << entt::to_integral(player);
		PushUnavailable(player);
		return;
	}
	// 1. Redis 未就绪:立即推 UNAVAILABLE。
	if (!tlsPlayerRedis.connected())
	{
		LOG_WARN << "[PlayerLanding] SendSyncDigest: Redis not connected;UNAVAILABLE pushed, player_id=" << playerId;
		PushUnavailable(player);
		return;
	}

	// 2. 只按值捕获 player_id:回调里重新找实体(第 3 步),不把实体句柄带进异步回调。
	const int ret = tlsPlayerRedis.command(
		[playerId = playerId](hiredis::Hiredis *, redisReply *reply) {
			// 3. 实体已销毁或槽位已换人:丢弃。客户端等不到推送会超时重试(设计 §4.3 第 2 步)。
			const entt::entity current = FindSamePlayer(playerId);
			if (current == entt::null)
			{
				LOG_DEBUG << "[PlayerLanding] sync digest reply discarded: player no longer on this node, player_id="
						  << playerId;
				return;
			}
			// 4–5. 解析并推送;任何失败都推 UNAVAILABLE。
			HandleDigestReply(current, playerId, reply);
		},
		(std::string("HGETALL ") + player_landing::kSyncDigestKeyFmt).c_str(),
		static_cast<unsigned long long>(playerId));
	if (ret != REDIS_OK)
	{
		// 命令没被 hiredis 收下(连接正在断开 / 释放,或格式化失败)时回调永远不会被调用,必须在这里补推,
		// 否则客户端等不到任何应答。command 不会同步执行回调,player 此刻仍有效。
		LOG_WARN << "[PlayerLanding] SendSyncDigest: HGETALL not sent;UNAVAILABLE pushed, player_id=" << playerId
				 << " ret=" << ret;
		PushUnavailable(player);
	}
}

void PlayerLandingSystem::OnEnteredScene(entt::entity player, uint32_t enterGsType)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}
	LOG_DEBUG << "[PlayerLanding] OnEnteredScene player_id=" << GuidOf(player) << " enter_gs_type=" << enterGsType;

	// 场景侧待办唤醒(设计 §5.2):guild / trade 资产通道收到后立即重投该玩家的 PENDING 行。
	PublishAssetWake(player, AssetWakeReason::kEnteredScene);

	// 后续落地钩子挂在这里,每个一行、互不依赖,顺序无业务含义:
	//   - 统一玩家收件箱服务在 scene 侧的消费触发(player-inbox-service.md §7.3 第 1 条,协调项 C1):
	//     由收件箱系统自己判断是不是"新落地"(实体在本节点新建 / owner_epoch 变了),是就无条件拉一次。
	PlayerInboxSystem::OnPlayerLanded(player);
	// 版本摘要不在这里推:客户端在收到 NotifyEnterScene 之后主动发 GetSyncDigest(设计 §4.3),
	// 同 zone 换图连接不变、不算落地,服务端无从判断"这次进场是不是一次落地"。
}

void PlayerLandingSystem::PublishAssetWake(entt::entity player, AssetWakeReason reason)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}
	const uint64_t playerId = GuidOf(player);
	if (playerId == 0)
	{
		return;
	}
	// 归属交接在途:资产写路径此时本来就会拒绝。交接结果若是销毁实体,新节点进场时会再唤醒;
	// 若是解冻,则退回原退避(≤60 秒)。
	if (tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player))
	{
		LOG_DEBUG << "[PlayerLanding] asset wake skipped: player frozen(ownership handoff in flight), player_id="
				  << playerId;
		return;
	}

	// 合并窗口只对进场来源生效(见头文件 AssetWakeReason 的说明)。
	const bool coalesce = reason == AssetWakeReason::kEnteredScene;
	const uint64_t nowMs = SteadyNowMs();
	if (const auto *wake = tlsEcs.actorRegistry.try_get<PlayerLandingWakeComp>(player);
		coalesce && wake != nullptr && nowMs - wake->lastWakeMs < player_landing::kAssetWakeCoalesceMs)
	{
		LOG_DEBUG << "[PlayerLanding] asset wake coalesced, player_id=" << playerId;
		return;
	}

	if (!ControlRedisReady())
	{
		LOG_WARN << "[PlayerLanding] asset wake not published(Redis disconnected);falls back to retry backoff, player_id="
				 << playerId;
		return;
	}

	const int ret = tlsRedis.GetZoneRedis()->command(
		[playerId = playerId](hiredis::Hiredis *, redisReply *reply) {
			// 只在出错时打日志:PUBLISH 的正常应答是收到的订阅者数(整数),0 个订阅者也不算错
			// (guild / trade 的资产通道可能未开启)。
			if (reply == nullptr)
			{
				LOG_WARN << "[PlayerLanding] asset wake got no reply(Redis disconnected), player_id=" << playerId;
				return;
			}
			if (reply->type == REDIS_REPLY_ERROR)
			{
				LOG_WARN << "[PlayerLanding] asset wake rejected by Redis, player_id=" << playerId
						 << " err=" << (reply->str != nullptr ? reply->str : "");
			}
		},
		(std::string("PUBLISH ") + player_landing::kAssetWakeChannel + " %llu").c_str(),
		static_cast<unsigned long long>(playerId));
	if (ret != REDIS_OK)
	{
		// 没发出去不记合并窗口:下一次进场照发。唤醒丢了退回资产通道原有的退避重投(设计 L-4)。
		LOG_WARN << "[PlayerLanding] asset wake PUBLISH not sent;falls back to retry backoff, player_id=" << playerId
				 << " ret=" << ret;
		return;
	}
	if (coalesce)
	{
		tlsEcs.actorRegistry.emplace_or_replace<PlayerLandingWakeComp>(player).lastWakeMs = nowMs;
	}
}
