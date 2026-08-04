package logic

import (
	"context"
	"fmt"
	"strconv"

	"proto/common/base"
	"proto/scene_manager"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DestroySceneLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDestroySceneLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DestroySceneLogic {
	return &DestroySceneLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// DestroyScene is the explicit RPC path operators / gameplay code use to
// force-destroy a scene. Unlike the lifecycle auto-destroy it does NOT
// respect player_count — an explicit DestroyScene means "tear it down now".
// It still:
//   - Cascades into any mirrors whose source is this scene (destroying an
//     active world would otherwise leave dangling mirrors).
//   - Removes the scene from both reverse indexes (node:zone:{zoneId}:{nodeId}:scenes and
//     scene:{src}:mirrors when this is itself a mirror).
//   - Drains the per-node player counter by the residual, to keep
//     GetBestNode's composite score accurate.
func (l *DestroySceneLogic) DestroyScene(in *scene_manager.DestroySceneRequest) (*base.Empty, error) {
	sceneIdStr := fmt.Sprintf("%d", in.SceneId)
	key := fmt.Sprintf("scene:%d:node", in.SceneId)

	// Snapshot everything we need before we wipe state.
	nodeId, _ := l.svcCtx.Redis.Get(key)
	residualStr, _ := l.svcCtx.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, in.SceneId))
	residual, _ := strconv.ParseInt(residualStr, 10, 64)
	sourceStr, _ := l.svcCtx.Redis.Get(sceneSourceKey(in.SceneId))
	sourceId, _ := strconv.ParseUint(sourceStr, 10, 64)
	mirrorFlag, _ := l.svcCtx.Redis.Get(sceneMirrorFlagKey(in.SceneId))
	kind := "instance"
	if mirrorFlag == "1" {
		kind = "mirror"
	}

	// Cascade: destroy every mirror whose source is this scene first.
	// Use the lifecycle force path so "mirror has players" doesn't rescue
	// children from a dying parent.
	mirrorChildren, _ := l.svcCtx.Redis.Smembers(sceneMirrorsKey(in.SceneId))
	for _, mid := range mirrorChildren {
		childId, err := strconv.ParseUint(mid, 10, 64)
		if err != nil || childId == in.SceneId {
			continue
		}
		childZoneStr, _ := l.svcCtx.Redis.Get(sceneZoneKey(childId))
		childZone, _ := strconv.ParseUint(childZoneStr, 10, 32)
		if childZone == 0 {
			childZone = uint64(in.ZoneId)
		}
		l.Logger.Infof("DestroyScene: cascading to mirror %d (source %d)", childId, in.SceneId)
		destroyInstanceForce(l.ctx, l.svcCtx, uint32(childZone), childId, "cascade")
	}
	l.svcCtx.Redis.Del(sceneMirrorsKey(in.SceneId))

	// Notify C++ node to destroy the ECS scene entity (skip if node is already dead).
	if nodeId != "" && IsNodeAlive(l.svcCtx, in.ZoneId, nodeId) {
		if err := RequestNodeDestroyScene(l.ctx, l.svcCtx, in.ZoneId, nodeId, in.SceneId); err != nil {
			l.Logger.Errorf("Failed to call DestroyScene on node %s for scene %d: %v", nodeId, in.SceneId, err)
		}
	}

	if _, err := l.svcCtx.Redis.Del(key); err != nil {
		l.Logger.Errorf("Failed to destroy scene %d: %v", in.SceneId, err)
	} else {
		l.Logger.Infof("Destroyed scene %d", in.SceneId)
	}

	// Clean up instance tracking (no-op if this was a main scene).
	instKey := activeInstancesKey(in.ZoneId)
	l.svcCtx.Redis.Zrem(instKey, sceneIdStr)
	l.svcCtx.Redis.Del(fmt.Sprintf(InstancePlayerCountKey, in.SceneId))
	l.svcCtx.Redis.Del(sceneMirrorFlagKey(in.SceneId))
	l.svcCtx.Redis.Del(sceneSourceKey(in.SceneId))
	l.svcCtx.Redis.Del(sceneZoneKey(in.SceneId))

	// Unlink reverse indexes.
	if nodeId != "" {
		l.svcCtx.Redis.Srem(nodeScenesKey(in.ZoneId, nodeId), sceneIdStr)
	}
	if sourceId > 0 {
		l.svcCtx.Redis.Srem(sceneMirrorsKey(sourceId), sceneIdStr)
	}

	// Drain node counters.
	if nodeId != "" {
		sceneCountKey := nodeSceneCountKey(in.ZoneId, nodeId)
		if _, countErr := l.svcCtx.Redis.Incrby(sceneCountKey, -1); countErr != nil {
			l.Logger.Errorf("Failed to decrement scene count for node %s: %v", nodeId, countErr)
		}
		if residual > 0 {
			playerCountKey := nodePlayerCountKey(in.ZoneId, nodeId)
			if newVal, err := l.svcCtx.Redis.Incrby(playerCountKey, -residual); err == nil && newVal < 0 {
				l.svcCtx.Redis.Set(playerCountKey, "0")
			}
		}
	}

	metrics.ObserveInstanceDestroyed(in.ZoneId, kind, "explicit")
	return &base.Empty{}, nil
}
