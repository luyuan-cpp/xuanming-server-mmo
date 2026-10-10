#pragma once

#include <cstdint>

#include "entt/src/entt/entity/entity.hpp"

// 服务器主动瞬移之后、客户端确认落位之前挂在实体上(只由 MovementSystem 读写)。
//
// 移动是客户端上报、服务器用导航裁决:瞬移发出时,客户端可能还有按旧位置发出的
// MoveStart / MoveSync / MoveStop 在路上。它们到达时服务器已经站在新位置,照常裁决就会
// 把人沿直线拉回旧位置(中间没有墙的话整段都会被接受),瞬移等于白做。
// 这个组件记下瞬移目标:期间离目标太远的上报一律当作"瞬移之前发的"丢弃;
// 第一条落在目标附近的上报说明客户端已经落位,组件随即摘除。
// 超时之后仍在远处上报,说明客户端没有应用这次瞬移:同样摘除,之后照常相信客户端
// (与瞬移之前的信任模型一致,不会把人卡死)。没有定时器 —— 判定与摘除都发生在下一条移动上报到达时,
// 一直不上报的实体会一直带着它,无害。
// 只对瞬移发生的那个场景有效:期间实体换了场景(sceneRegistry 句柄不同),新场景里的坐标与
// 这个目标点没有可比性,组件作废。
// 运行时状态,不落盘,不进任何 proto。
struct TeleportSettleComp
{
	// 瞬移发生时实体所在的场景(sceneRegistry 句柄);没有场景时为 entt::null。
	entt::entity scene{entt::null};
	// 瞬移目标(服务器坐标;水平面是 x/y,z 为高度)。
	double targetX = 0.0;
	double targetY = 0.0;
	// 过期时刻:单调时钟(std::chrono::steady_clock)毫秒,只能与同一进程内的单调时钟读数比较。
	uint64_t expireAtMs = 0;
};
