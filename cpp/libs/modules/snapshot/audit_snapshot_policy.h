#pragma once

#include <cstdint>

#include "proto/common/base/config.pb.h"
#include "proto/common/rollback/player_snapshot.pb.h"

// 进场、离场审计快照的开关与抽样(game_config.yaml 的 audit_snapshot,config.cpp 的 readGameConfig 解析)。
// 只管这两个触发点:它们随每次进退场发整档,量最大;GM、交易前、维护前这些按需快照一律照发。
// 只影响审计证据,不影响存盘。
namespace audit_snapshot_policy
{

// 抽样千分比的上限,等于全量。
inline constexpr uint32_t kSampleAll = 1000;

// 抽样前再打散一次 player_id(splitmix64 的收尾步):player_id 来自发号器,已经打散,这里不依赖上游的分布。
inline uint64_t MixForSampling(uint64_t value)
{
	value ^= value >> 30;
	value *= 0xbf58476d1ce4e5b9ULL;
	value ^= value >> 27;
	value *= 0x94d049bb133111ebULL;
	value ^= value >> 31;
	return value;
}

// ShouldCapture 判断这一次要不要发快照。抽样按 player_id 稳定:同一玩家要么一直抽中、要么一直跳过,
// 进场和离场的快照成对出现,查一个玩家时证据是连贯的。
inline bool ShouldCapture(const GameConfig::AuditSnapshotConfig &config, SnapshotTrigger trigger, uint64_t playerId)
{
	switch (trigger)
	{
	case SNAPSHOT_LOGIN:
		if (!config.login_enabled())
		{
			return false;
		}
		break;
	case SNAPSHOT_LOGOUT:
		if (!config.logout_enabled())
		{
			return false;
		}
		break;
	default:
		return true;
	}
	if (config.sample_per_mille() >= kSampleAll)
	{
		return true;
	}
	return MixForSampling(playerId) % kSampleAll < config.sample_per_mille();
}

} // namespace audit_snapshot_policy
