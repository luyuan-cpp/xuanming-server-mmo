package logic

import (
	"fmt"

	"proto/scene_manager"
	"scene_manager/internal/metrics"
)

// 疏散请求的落点解析(EnterSceneRequest.evacuation,docs/design/scene-node-planned-drain.md §4.4)。
//
// 源 scene 节点把玩家从一个要没了的场景里搬走:计划排空(节点下线)、整节点疏散(身份冲突)、频道缩容、归属对账与
// 镜像级联的单场景排空。请求带两条去向提示 —— scene_id 是原频道(副本、镜像为 0),scene_conf_id 是原地图(副本为 0)。
// 依次解析:
//  1. 原频道已归别的节点(排空迁移、归属改派之后):派过去 —— 同一个频道,坐标保留;
//  2. 原地图是世界地图:同图最空的频道;
//  3. 默认大世界最空的频道;
//  4. 都没有:回错误,scene 侧的待确认表按「改派没生效」核实后踢线(现有行为)。
//
// 2、3 不选属主排空中的频道,不在外出构建上的优先(tierWorldChannelCandidates)。
//
// 解析不需要知道源节点是不是在下线:
//   - 节点计划下线就是排空(自报 CLOSED),它名下的频道在 2、3 被排除;
//   - 频道缩容时源节点照常可选,被缩的那个频道已经不在路由集合里;
//   - 原频道还在源节点上(还没迁走)时第 1 步跳过,不把人派回将要拆掉的那份实例;
//   - 身份冲突的老进程:按节点号路由到的已是接手这个号的新进程(或节点已不在负载集),不会派回老进程。
//
// 落点之后的步骤(换手门、铸 epoch、写 location、推路由)与普通跨节点交接完全相同:同一个 scene_id 换了节点不算同落点。

// evacuationSourceNode 是疏散的源节点:location 记着的、与落点同一个 zone 的节点号;拿不到时为空串(不排除任何节点)。
func evacuationSourceNode(currentLoc *scene_manager.PlayerLocation, currentZoneID, targetZoneID uint32) string {
	if currentLoc == nil || currentZoneID == 0 || currentZoneID != targetZoneID {
		return ""
	}
	return currentLoc.GetNodeId()
}

// resolveEvacuationTarget 按上面的顺序解析疏散落点。返回值同 resolveSceneForEnter:reserved = 是否已按选频道预占人数
// (第 1 步不预占,与按 scene_id 进场一致;落点提交后统一确认成员身份)。
func (l *EnterSceneLogic) resolveEvacuationTarget(in *scene_manager.EnterSceneRequest, sourceNode string, zoneID uint32) (uint64, string, bool, error) {
	admission := snapshotNodeAdmission(zoneID)

	if sceneID := in.GetSceneId(); sceneID != 0 {
		if nodeID, ok := l.evacuationOwnerOf(sceneID, sourceNode, zoneID, admission); ok {
			metrics.ObserveEvacuationResolve(zoneID, metrics.EvacuationResolvedSameChannel)
			return sceneID, nodeID, false, nil
		}
	}

	pick := worldChannelPick{evacuation: true}
	confID := in.GetSceneConfId()
	if confID != 0 && isWorldMap(confID) {
		sid, nid, err := reserveWorldChannelForEnter(l.ctx, l.svcCtx, confID, zoneID, in.GetPlayerId(), pick)
		if err == nil && sid != 0 {
			metrics.ObserveEvacuationResolve(zoneID, metrics.EvacuationResolvedSameMap)
			return sid, nid, true, nil
		}
		l.Logger.Infof("[Evacuation] no channel of the same map; trying the default world: player=%d zone=%d conf=%d err=%v",
			in.GetPlayerId(), zoneID, confID, err)
	}

	if defaultConf, ok := defaultWorldConfID(); ok && defaultConf != confID {
		sid, nid, err := reserveWorldChannelForEnter(l.ctx, l.svcCtx, defaultConf, zoneID, in.GetPlayerId(), pick)
		if err == nil && sid != 0 {
			metrics.ObserveEvacuationResolve(zoneID, metrics.EvacuationResolvedDefaultWorld)
			return sid, nid, true, nil
		}
		l.Logger.Infof("[Evacuation] no channel of the default world: player=%d zone=%d conf=%d err=%v",
			in.GetPlayerId(), zoneID, defaultConf, err)
	}

	metrics.ObserveEvacuationResolve(zoneID, metrics.EvacuationResolvedNone)
	return 0, "", false, fmt.Errorf("no evacuation destination in zone %d (scene %d, conf %d)", zoneID, in.GetSceneId(), confID)
}

// isWorldMap 报告 confID 是不是一张世界地图。与 defaultWorldConfID 同一个口径(worldConfIds:World 表,单测可覆盖),
// 两步解析看的是同一份地图清单。
func isWorldMap(confID uint64) bool {
	for _, id := range worldConfIds() {
		if id == confID {
			return true
		}
	}
	return false
}

// evacuationOwnerOf 判断原频道能不能接住疏散的玩家:频道属于本 zone,属主存在、不是源节点、活着、没在排空、身份不歧义。
// 任何一次读失败都按「不能」处理,交给下一级(同图别的频道);这里不改写任何归属。
func (l *EnterSceneLogic) evacuationOwnerOf(sceneID uint64, sourceNode string, zoneID uint32, admission nodeAdmissionView) (string, bool) {
	// 只读 zoneID 的属主键(docs/design/redis-cluster-all-domains-20261110.md §3.3):原频道不在这个 zone 时读不到属主,
	// 与「属主不存在」同一个结局,交给下一级。原来先读 scene:<id>:zone 比 zone,那把键已删。
	owner, err := l.svcCtx.Redis.Get(sceneNodeKey(zoneID, sceneID))
	if err != nil {
		l.Logger.Errorf("[Evacuation] owner of scene %d unreadable; skipping the original channel: %v", sceneID, err)
		return "", false
	}
	if owner == "" || owner == sourceNode {
		return "", false
	}
	if isKnownNodeIdentityAmbiguous(zoneID, owner) || admission.isDraining(owner) || !IsNodeAlive(l.svcCtx, zoneID, owner) {
		return "", false
	}
	return owner, true
}
