#pragma once

// 疏散改派的去向提示(docs/design/scene-node-planned-drain.md §4.4):填进 EnterSceneRequest 的 scene_id / scene_conf_id,
// 同时置 evacuation。scene_manager 按「原频道 → 同图最空频道 → 默认大世界」依次解析,不选排空中的节点
// (go/scene_manager/internal/logic/enter_evacuation.go)。
//
// 「是不是世界地图」由 scene_manager 按 World 表判,这里只按场景自己的形态给提示:
//   - 副本(dungeon_config_id != 0):状态在本进程内存里,搬不走,回默认大世界 → {0, 0};
//   - 镜像(mirror_config_id != 0):镜像本身不迁,回同一张图最空的频道 → {0, 本图};
//   - 其它(世界频道):先找同一个频道(排空迁移之后它在别的节点上、号不变),再找同图 → {原频道, 本图}。
//
// 纯函数,不依赖 entt / redis / protobuf,单测见 cpp/tests/cross_zone_test/planned_drain_test.cpp。

#include <cstdint>

namespace relocate_target
{
struct Target
{
	uint64_t sceneId{0};
	uint64_t sceneConfId{0};
};

constexpr Target ForScene(const uint64_t sceneId, const uint32_t sceneConfigId, const uint32_t mirrorConfigId,
						  const uint32_t dungeonConfigId)
{
	if (dungeonConfigId != 0)
	{
		return Target{};
	}
	if (mirrorConfigId != 0)
	{
		return Target{0, sceneConfigId};
	}
	return Target{sceneId, sceneConfigId};
}
} // namespace relocate_target
