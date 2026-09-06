#pragma once

#include <cstdint>

#include "entt/src/entt/entity/entity.hpp"

#include "proto/common/component/actor_comp.pb.h"

struct NavComp;

// 场景出生点与进场落位(服务器权威)。
//
// 背景:玩家 Transform 从 DB 反序列化而来,新号是 proto 默认值 (0,0,0),旧号
// 可能存着别的场景/旧地图坐标。此前服务器对进场位置零校验,客户端发现
// 出生点不可走会**本地**挪到 DefaultSpawn,而服务器仍停在 (0,0,0):首次移动
// 上报城内坐标 → 服务器从 (0,0,0) 做导航裁决 → MoveAck 把客户端拉回原点
// 附近 → 卡在不可走区域(2026-09-05 移动诊断日志 reconcile snap input_seq=1)。
//
// 本系统把"位置是否合法"的裁决收回服务器:进场时校验 Transform 是否在目标
// 场景的导航网格上,不在就落到场景出生点(吸附到网格),自身 ActorCreate /
// AOI 广播都拿到这个合法位置;客户端不再需要本地兜底。
class SceneSpawnSystem
{
public:
	// 场景配置 id → 出生点(服务器坐标,未吸附)。当前所有场景同一张天墉城图,
	// 统一返回 kTianyongSpawn*;分场景后改为读 BaseScene 表。
	static Location DefaultSpawnFor(uint32_t sceneConfigId);

	// 出生点吸附到该场景导航网格后的合法落点。nav 为空(场景无导航,fail-open)
	// 时原样返回未吸附出生点;出生点本身不在网格上(烘焙数据与出生点不匹配,
	// 属配置错误)时打 ERROR 并同样返回未吸附出生点,不让玩家卡在进场。
	static Location ResolveSpawnOnMesh(NavComp* nav, uint32_t sceneConfigId);

	// 进场落位:校验 player 的 Transform.location 是否在 scene 的导航网格上。
	//   - 在网格上 → 写回吸附后的点(修正高度/贴边毫米级误差),不算改写;
	//   - 不在网格上 → 改写为 ResolveSpawnOnMesh 的出生点;
	//   - 场景无导航(fail-open)→ 只把全零的"未初始化"位置改写为出生点。
	// 返回 true 表示位置被改写(调用方可据此打日志/置脏位)。
	// 必须在 SceneEntityComp 绑定之后、自身 ActorCreate 下发之前调用。
	static bool EnsureValidEnterLocation(entt::entity player, entt::entity scene);

	// 玩家当前所在场景的合法兜底点:移动裁决发现"服务器当前位置与上报位置
	// 都不在网格上"时用它代替原地不动(原地本身就是非法点,回 ack 只会把
	// 客户端拉进墙里)。无场景/无导航时退化为 DefaultSpawnFor(0)。
	static Location FallbackLocationForPlayer(entt::entity player, NavComp* nav);

	// (0,0,0) = proto 默认值,视为"从未落位"。
	static bool IsUnsetLocation(const Vector3& location);
};
