#pragma once

#include <cstdint>

#include "entt/src/entt/entity/entity.hpp"
#include "engine/core/utils/registry/scene_entity.h"
#include "player/system/team_follow_formation.h"

// 服务器驱动组队跟随的运行时组件(docs/design/team-follow-server-trail.md)。
// 全部是按需状态(docs/design/ecs-component-access-rules.md):不落盘、不序列化、不跨进程;
// 玩家换到别的节点后由客户端重新声明跟随。挂 / 摘 / 读的唯一入口都在 TeamFollowSystem,
// 下面逐个写明例外的只读方。

// 队长身上:队形(轨迹 + 名次)。
// 挂:TeamFollowSystem::TryAttach(第一名队员接上时)。
// 摘:队长离队、不再是队长、换场景、交接离开本节点、退出、实体失效时(TeamFollowSystem 内部解散)。
struct TeamFollowFormationComp
{
	TeamFollowFormation formation;
	uint64_t teamId{0};                // 建队形时队长的 TeamId;队长的 TeamId 变了,队形作废
	SceneEntity scene{entt::null};     // 建队形时队长所在场景;队长换了场景,队形作废
};

// 正被服务器驱动走位的队员身上。
// 挂 / 摘:只在 TeamFollowSystem(TryAttach / Detach)。
// 只读方:移动上报 handler(TeamFollowSystem::IsDriven 为真时整条丢弃)、
//         MovementSystem::Update(exclude:位置由队形写,不按 Velocity 积分)。
struct TeamFollowerComp
{
	entt::entity leader{entt::null};
	uint64_t leaderPlayerId{0};
	// 移动广播节流(只在 TeamFollowSystem 里读写):上一次广播时是否在走、速度与帧号。
	bool broadcastMoving{false};
	double broadcastVelocityX{0.0};
	double broadcastVelocityY{0.0};
	uint32_t broadcastFrame{0};
};

// 队员客户端声明了"跟随"(TeamFollowC2S{follow=true});暂离(follow=false)时摘。
// 只有挂着它的队员才由服务器驱动;从不发 TeamFollowC2S 的旧客户端保持原来的客户端跟随与归队瞬移。
struct TeamFollowIntentComp
{
};

// 最近一次从组队权威读到的本人入队序号(决定名次)。
// 写:PlayerTeamSystem 每次应用成员关系时覆盖;读:TeamFollowSystem::TryAttach。缺失按 0 处理。
struct TeamFollowJoinSeqComp
{
	uint32_t joinSeq{0};
};
