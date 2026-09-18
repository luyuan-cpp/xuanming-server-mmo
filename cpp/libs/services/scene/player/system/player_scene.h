#pragma once

#include "entt/src/entt/entity/entity.hpp"

#include <cstdint>

class PlayerSceneSystem
{
public:
	static void HandleEnterScene(entt::entity player, entt::entity scene);

	// RequestEnterMirrorScene asks SceneManager to create a mirror of the
	// player's current scene and then route the player into it. The
	// mirror is created with:
	//   - source_scene_id  = player's current scene_id (drives co-location;
	//     the new mirror lands on the same node when feasible)
	//   - mirror_config_id = caller-provided template id (>0; SceneInfoComp
	//     downstream filters mirrors via mirror_config_id() > 0)
	//   - creator_ids      = [player_id]; SceneManager echoes these in
	//     CreateSceneResponse so the response handler can dispatch the
	//     follow-up EnterScene without per-call request state
	//   - scene_conf_id    = the source's scene_config_id (template) so the
	//     mirror inherits the same map definition
	//   - scene_type       = INSTANCE (mirrors are not main-world channels)
	//
	// Returns true if the request was dispatched (CreateScene fired). False
	// when the prerequisites failed (no current scene, no SceneManager node,
	// missing player session). Failure to dispatch is logged at WARN; an
	// async failure on the SceneManager side is logged at ERROR by the
	// AsyncSceneManagerCreateScene handler.
	//
	// mirrorConfigId must be > 0. Pass 0 only if the intent is "clone
	// everything from source" (no template), which goes through the
	// source-clone refusal path on SceneManager and is currently NOT
	// supported by this helper (callers always pick a mirror template).
	static bool RequestEnterMirrorScene(entt::entity player, uint32_t mirrorConfigId);
};
