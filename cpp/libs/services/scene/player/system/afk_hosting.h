#pragma once

// 挂机月卡离线托管的粘合(docs/design/player-connection-lost-hold.md §4.1):
// 断线保留期间读玩家域 Redis 的 player:afk_pass:{<player_id>},把结果写回 PlayerConnectionLostComp;
// 第一次确认月卡有效时算"托管开始",交给组队跟随由服务器开启跟随(PlayerTeamSystem::OnOfflineHostingStarted)。
// 留人 / 退出的判定在 connection_lost_hold::JudgeHold(纯函数),由 PlayerLifecycleSystem::ExpireConnectionLostHolds 每秒调用。
//
// 调用方:PlayerLifecycleSystem::HandleClientConnectionLost(进入保留时查一次)、ExpireConnectionLostHolds(判定要再查时)。
// 线程模型:只在 scene 节点 loop 线程调用;Redis 回调也在 loop 线程执行,回调里按玩家号重新找实体并核对是不是同一次保留。

#include <cstdint>
#include <string_view>

#include "entt/src/entt/entity/entity.hpp"
#include "player/system/connection_lost_hold.h"

class AfkHostingSystem
{
public:
	// 对一个保留中的实体发一次月卡查询。不在保留中、已有查询在途、Redis 没连上或命令没被收下时什么都不做
	// (状态不变;普通窗口内的下一次扫描会再来问)。
	static void Probe(entt::entity player);

	// 一次查询的结果(Redis 回调与单测共用的入口)。只认 lostAt 相同的那一次保留:接回、退出、或断开后又重新进入保留的,
	// 迟到的结果直接丢弃。failed = true 表示查询失败(空回包、错误回包、类型不对):清掉在途标记、状态不变;
	// 否则 found / value 是 GET 的结果。nowUnixSec 是墙钟 Unix 秒。
	static void ApplyProbeResult(uint64_t playerId, connection_lost_hold::Clock::time_point lostAt, bool failed, bool found,
								 std::string_view value, uint64_t nowUnixSec);
};
