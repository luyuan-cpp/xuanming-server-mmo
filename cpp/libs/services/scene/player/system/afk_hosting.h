#pragma once

// 挂机月卡离线托管的粘合(docs/design/player-connection-lost-hold.md §4.1):
// 断线保留期间读本实体内存里的月卡到期时刻(PlayerUint64Comp.afk_pass_expires_at,AfkPassSystem::ExpiresAt;
// 事实源在存档,docs/design/afk-monthly-card.md §5.3 —— 不读 Redis 投影),把结果写回 PlayerConnectionLostComp;
// 第一次确认月卡有效时算"托管开始",交给组队跟随由服务器开启跟随(PlayerTeamSystem::OnOfflineHostingStarted)。
// 留人 / 退出的判定在 connection_lost_hold::JudgeHold(纯函数),由 PlayerLifecycleSystem::ExpireConnectionLostHolds 每秒调用。
//
// 调用方:PlayerLifecycleSystem::HandleClientConnectionLost(进入保留时查一次)、托管落点入场(EnterHostedHold)、
// ExpireConnectionLostHolds(判定要再查时)。
// 线程模型:只在 scene 节点 loop 线程调用;查询排到本轮之后执行(queueInLoop),执行时按玩家号重新找实体并核对是不是同一次保留。

#include <cstdint>
#include <string_view>

#include "entt/src/entt/entity/entity.hpp"
#include "player/system/connection_lost_hold.h"

class AfkHostingSystem
{
public:
	// 对一个保留中的实体发一次月卡查询:排到本轮之后读内存里的到期时刻,再走 ApplyProbeResult。不在保留中、已有查询在途、
	// 没有 EventLoop(单测宿主)时什么都不做(状态不变;普通窗口内的下一次扫描会再来问)。
	static void Probe(entt::entity player);

	// 一次查询的结果(Probe 排队的那一步与单测共用的入口)。只认 lostAt 相同的那一次保留:接回、退出、或断开后又重新进入保留的,
	// 迟到的结果直接丢弃。failed = true 表示查询失败:清掉在途标记、状态不变(读内存不会失败,这一支只剩单测在用);
	// 否则 found / value 是月卡到期时刻(十进制 Unix 秒,found = false 即从未开通或已清除)。nowUnixSec 是墙钟 Unix 秒。
	static void ApplyProbeResult(uint64_t playerId, connection_lost_hold::Clock::time_point lostAt, bool failed, bool found,
								 std::string_view value, uint64_t nowUnixSec);
};
