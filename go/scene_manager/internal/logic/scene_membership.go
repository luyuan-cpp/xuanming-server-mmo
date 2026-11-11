package logic

import (
	"context"
	"fmt"
	"strconv"

	"proto/scene_manager"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// 场景与节点的成员集合(docs/design/redis-player-store-cluster.md §5.10)。
//
//	instance:<scene>:members         SET,成员是十进制 player_id;场景人数 = SCARD
//	node:{ctl:<zone>}:<node>:members SET,同上;节点人数 = SCARD,只进负载分与每节点指标
//
// 这两把键在控制域(svcCtx.Redis),不带玩家域的 tag;位置等玩家域键在 svcCtx.PlayerRedis,一段 Lua 不能同时碰两边,
// 所以「删位置」与「摘成员」是两步。计数器在两步之间失败时人数永久多计(实例永不销毁),集合没有这个问题:
// SADD / SREM 天然幂等,原样重发、调用方重试、并发重进场都不会多减或漏减。
//
// 成员身份跟着 player:{id}:location 走,不变量是「location 指向 (scene,node) ⇔ 玩家是这两个集合的成员」,
// 由下面三条约定收敛,任意交错都成立(推导见设计文档):
//   - 预占(进场时为了让并发选线看到在途请求):落点之前 SADD;
//   - 落点提交之后再 SADD 一次(addSceneMember):预占之后、落点之前,别的请求可能已经把它摘掉;
//   - 摘成员时,location 此刻指向这里的部分不摘;其余 SREM 之后再读一次 location,这时指向这里就加回去
//     (removeSceneMember):SREM 之前并发进场可能已经落点。
//
// 所以调用方不必判断「旧场景与新场景是否同一个」「旧节点与新节点是否同一个」,也不必区分预占是不是本请求加的。
// 重叠的部分不摘,人数不下探:排空收尾、机会迁移只读一次人数就做决定,看到 0 就会动一个有人的频道。只有「第一次读之后、
// SREM 之前恰好有并发进场落到这里」时才有一次往返的下探,代价是每次摘成员多读一次 location。
//
// scene_manager 在预占之后、落点之前崩溃,或落点之后、摘旧场景之前崩溃,会留下一个 location 不指向这里的成员,
// 请求路径上没有人会再摘它。这类残留由后台对账收掉(scene_member_reconcile.go),在那之前的后果与计数器时代多计一次相同
// (实例不销毁、选线躲着它)。

const (
	// InstanceMembersKeyFmt 是场景的成员集合,见文件头。
	InstanceMembersKeyFmt = "instance:%d:members"
	// NodeMembersKeyFmt 是节点的成员集合,见文件头。第一个 %s 是 sceneControlTag(zone)。
	NodeMembersKeyFmt = "node:%s:%s:members"
)

func instanceMembersKey(sceneID uint64) string {
	return fmt.Sprintf(InstanceMembersKeyFmt, sceneID)
}

func nodeMembersKey(zoneID uint32, nodeID string) string {
	return fmt.Sprintf(NodeMembersKeyFmt, sceneControlTag(zoneID), nodeID)
}

// luaAddSceneMember / luaRemoveSceneMember:KEYS 是要动的成员集合(1~2 把,都在控制域),ARGV[1] 是十进制 player_id。
// 用脚本只为一次往返动两把键。
const luaAddSceneMember = `
for i = 1, #KEYS do
    redis.call('SADD', KEYS[i], ARGV[1])
end
return 1
`

const luaRemoveSceneMember = `
for i = 1, #KEYS do
    redis.call('SREM', KEYS[i], ARGV[1])
end
return 1
`

// sceneMemberKeys 组一次增减涉及的集合。sceneID 为 0 不动场景集合;zoneID 为 0 或 nodeID 为空不动节点集合。
func sceneMemberKeys(sceneID uint64, zoneID uint32, nodeID string) []string {
	keys := make([]string, 0, 2)
	if sceneID != 0 {
		keys = append(keys, instanceMembersKey(sceneID))
	}
	if zoneID != 0 && nodeID != "" {
		keys = append(keys, nodeMembersKey(zoneID, nodeID))
	}
	return keys
}

// skipForeignZoneMembership:zoneID 不是本进程的 zone(或未知)时跳过成员集合的写入(docs/design/control-plane-zone-scoping-20261105.md
// §4.3)。成员集合是派生索引,权威是玩家域的 location;别的 zone 的陈旧成员由那个 zone 的成员对账在宽限后摘掉
// (scene_member_reconcile.go),本进程没摘过的成员回滚时也不需要加回。同组的键在同一个实例上读得到也不写:行为不随组的划分变。
func skipForeignZoneMembership(svcCtx *svc.ServiceContext, zoneID uint32, site string) bool {
	if ownsControlZone(svcCtx, zoneID) {
		return false
	}
	metrics.ObserveForeignZoneSkip(site)
	return true
}

// addSceneMember 把玩家加进场景与节点的成员集合。幂等。只写本 zone(skipForeignZoneMembership)。
func addSceneMember(svcCtx *svc.ServiceContext, playerID, sceneID uint64, zoneID uint32, nodeID string) error {
	if skipForeignZoneMembership(svcCtx, zoneID, metrics.ForeignZoneSkipSiteMemberAdd) {
		return nil
	}
	keys := sceneMemberKeys(sceneID, zoneID, nodeID)
	if len(keys) == 0 {
		return nil
	}
	_, err := svcCtx.Redis.Eval(luaAddSceneMember, keys, strconv.FormatUint(playerID, 10))
	return err
}

// removeSceneMember 把玩家移出场景与节点的成员集合,location 指向这里的部分保留。幂等。推导见文件头。
// 读 location 走玩家域句柄(GetPlayerLocation),写成员走控制域句柄。
//
// 返回错误时成员身份处于未知状态(SREM 可能已生效)。需要收敛的调用方把错误交给上游重试,见 LeaveScene。
// 只写本 zone(skipForeignZoneMembership)。
func removeSceneMember(svcCtx *svc.ServiceContext, playerID, sceneID uint64, zoneID uint32, nodeID string) error {
	if skipForeignZoneMembership(svcCtx, zoneID, metrics.ForeignZoneSkipSiteMemberRemove) {
		return nil
	}
	loc, err := GetPlayerLocation(context.Background(), svcCtx, playerID)
	if err != nil {
		return err
	}
	keepScene, keepNode := locationOverlap(loc, sceneID, zoneID, nodeID)
	if keepScene != 0 {
		sceneID = 0
	}
	if keepNode != "" {
		nodeID = ""
	}
	keys := sceneMemberKeys(sceneID, zoneID, nodeID)
	if len(keys) == 0 {
		return nil
	}
	if _, err := svcCtx.Redis.Eval(luaRemoveSceneMember, keys, strconv.FormatUint(playerID, 10)); err != nil {
		return err
	}
	loc, err = GetPlayerLocation(context.Background(), svcCtx, playerID)
	if err != nil {
		return err
	}
	sceneBack, nodeBack := locationOverlap(loc, sceneID, zoneID, nodeID)
	return addSceneMember(svcCtx, playerID, sceneBack, zoneID, nodeBack)
}

// locationOverlap 返回 location 指向 (sceneID, zoneID, nodeID) 的部分:场景号(不指向时为 0)与节点号(不指向时为空)。
func locationOverlap(loc *scene_manager.PlayerLocation, sceneID uint64, zoneID uint32, nodeID string) (uint64, string) {
	if loc == nil {
		return 0, ""
	}
	var scene uint64
	if loc.GetSceneId() == sceneID {
		scene = sceneID
	}
	node := ""
	// 历史记录的 location 可能没有 zone_id(0),此时只比节点号。
	if loc.GetNodeId() == nodeID && (loc.GetZoneId() == zoneID || loc.GetZoneId() == 0) {
		node = nodeID
	}
	return scene, node
}

// removeScenePlayer 按场景此刻的属主摘成员,给手里只有场景号的路径用(离场、撤销)。zoneID 为 0 时按 scene:{id}:zone 补。
// 场景被改派过时玩家当初进的是旧节点的集合,而改派只发生在旧节点已死之后,它的集合由死节点收尾删掉。
// scene:{id}:zone 只在本组的控制域实例里查:读不到(场景在别的组,或已销毁)或不是本 zone,就不动(§4.3)。
func removeScenePlayer(svcCtx *svc.ServiceContext, playerID, sceneID uint64, zoneID uint32) error {
	if sceneID == 0 {
		return nil
	}
	if zoneID == 0 {
		zoneID = GetSceneZone(svcCtx, sceneID)
	}
	if skipForeignZoneMembership(svcCtx, zoneID, metrics.ForeignZoneSkipSiteScenePlayer) {
		return nil
	}
	return removeSceneMember(svcCtx, playerID, sceneID, zoneID, lookupSceneNode(svcCtx, sceneID))
}

// removeResidualSceneMembers 把被销毁场景 sceneID 剩下的成员从节点集合里摘掉,members 来自销毁脚本的返回值。
// location 还指向这个场景的要摘:场景已经没了,留着就是多算人数。location 指向同节点别的场景的不能摘。
// 不是合法 player_id 的成员没有 location 可核,直接摘。单个成员失败只打日志,由对账收掉(scene_member_reconcile.go)。
func removeResidualSceneMembers(svcCtx *svc.ServiceContext, sceneID uint64, zoneID uint32, nodeID string, members []string) {
	keys := sceneMemberKeys(0, zoneID, nodeID)
	if len(keys) == 0 {
		return
	}
	for _, member := range members {
		playerID, err := strconv.ParseUint(member, 10, 64)
		if err != nil {
			if _, err := svcCtx.Redis.Srem(keys[0], member); err != nil {
				logx.Errorf("[SceneMembership] remove malformed member %q from node %s members failed: %v", member, nodeID, err)
			}
			continue
		}
		if err := removeResidualNodeMember(svcCtx, keys, playerID, sceneID, zoneID, nodeID); err != nil {
			logx.Errorf("[SceneMembership] remove player %d from node %s members failed: %v", playerID, nodeID, err)
		}
	}
}

// removeResidualNodeMember 是 removeSceneMember 在场景已销毁时的版本:只动节点集合(keys),同样 SREM 之后再读一次
// location,这时落到了同节点别的场景就加回去(文件头第 3 条)。
func removeResidualNodeMember(svcCtx *svc.ServiceContext, keys []string, playerID, sceneID uint64, zoneID uint32, nodeID string) error {
	onOtherScene := func() (bool, error) {
		loc, err := GetPlayerLocation(context.Background(), svcCtx, playerID)
		if err != nil {
			return false, err
		}
		_, node := locationOverlap(loc, 0, zoneID, nodeID)
		return node != "" && loc.GetSceneId() != sceneID, nil
	}
	keep, err := onOtherScene()
	if err != nil || keep {
		return err
	}
	if _, err := svcCtx.Redis.Eval(luaRemoveSceneMember, keys, strconv.FormatUint(playerID, 10)); err != nil {
		return err
	}
	back, err := onOtherScene()
	if err != nil || !back {
		return err
	}
	return addSceneMember(svcCtx, playerID, 0, zoneID, nodeID)
}

// readScenePlayerCount 读一个场景的在线人数。读不到当 0。
// 只给读错无害的地方用:分线目录的展示、选线(预占由 Lua 原子完成)、空闲扫描(销毁脚本里再判一次)。
// 伸缩决策、排空推进和迁移规划用 readScenePlayerCountChecked:读错不能当 0。
func readScenePlayerCount(svcCtx *svc.ServiceContext, sceneID uint64) int64 {
	n, err := svcCtx.Redis.Scard(instanceMembersKey(sceneID))
	if err != nil {
		return 0
	}
	return n
}
