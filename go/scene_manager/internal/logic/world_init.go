package logic

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"scene_manager/internal/constants"
	"scene_manager/internal/svc"

	"shared/generated/table"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
)

// Redis keys for world scene management.
const (
	// SET per (zone, confId): holds sceneId strings for each channel.
	WorldChannelsKeyFmt = "world_channels:zone:%d:%d"

	// worldInitLockKeyFmt: zone 级补建互斥锁。initWorldScenesForZone 的
	// "数集合缺多少补多少"是无原子性的读改写,而它有两类并发入口:
	// 领导者的后台循环(fullSync / watch PUT / autoscaler 扩容)与**任意副本**
	// 上 CreateScene 的 on-demand 兜底(createscenelogic.go)。不加锁时
	// 两个实例同时看到 existing=0 就会把同一批频道位建两份。
	worldInitLockKeyFmt = "world_init:lock:zone:%d"
	// worldInitLockTTLSeconds: 锁 TTL。补建正常几百 ms 内完成,TTL 只是
	// 持有者进程死亡时的自愈上限。
	worldInitLockTTLSeconds = 60
	// worldInitLockWait / worldInitLockPoll: 锁忙时的等待预算与轮询间隔。
	// 等待预算按调用方拆分(initWorldScenesForZone 的 waitForLock 参数):
	//   - CreateScene 的 on-demand 兜底(任意副本)必须在返回前看到频道
	//     存在,用 worldInitLockWait 忙等;
	//   - 后台路径(fullSync / watch PUT / autoscaler)"锁忙"就意味着别人
	//     正在做同一件事,直接跳过、靠下一拍收敛 —— 尤其 watch PUT 跑在
	//     etcd 事件 goroutine 上,忙等 10s 会把 DELETE 事件和定时器全堵住。
	worldInitLockWait = 10 * time.Second
	worldInitLockPoll = 200 * time.Millisecond

	// worldInitCreateRPCTimeout: 锁内每个 CreateScene RPC 的时限。后台
	// 调用方传进来的是 main 的永不取消 ctx,黑洞节点(进程死了、etcd 租约
	// 还没过期)会让无时限 RPC 挂 ~20s(gRPC minConnectTimeout),几个僵尸
	// 节点就能把持锁时长顶过 60s TTL,互斥悄悄失效。CreateScene 按
	// scene_id 幂等,超时安全;DeadlineExceeded 只把节点记为「本轮不可达」(markNodeUnreachable),不据此判死。
	worldInitCreateRPCTimeout = 5 * time.Second
)

// luaReleaseWorldInitLock: 属主校验后释放,与仓内其它 Redis 锁的释放脚本一致。
const luaReleaseWorldInitLock = `
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
else
	return 0
end
`

// acquireWorldInitLock 串行化同一 zone 的世界频道补建。返回属主 token。
// maxWait=0 表示只试一次(后台路径:锁忙即跳过)。ok=false 的各种情况都
// 选择"跳过本轮补建":Redis 出错时无锁裸跑会复活双建竞态;等待超时说明
// 持有者异常(正常几百 ms 内放锁),下一拍会收敛。
func acquireWorldInitLock(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, maxWait time.Duration) (string, bool) {
	key := fmt.Sprintf(worldInitLockKeyFmt, zoneId)
	token := uuid.NewString()
	deadline := time.Now().Add(maxWait)
	for {
		ok, err := svcCtx.Redis.SetnxEx(key, token, worldInitLockTTLSeconds)
		if err != nil {
			logx.Errorf("[World] Zone %d: acquire init lock failed: %v", zoneId, err)
			return "", false
		}
		if ok {
			return token, true
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return "", false
		}
		time.Sleep(worldInitLockPoll)
	}
}

func releaseWorldInitLock(svcCtx *svc.ServiceContext, zoneId uint32, token string) {
	key := fmt.Sprintf(worldInitLockKeyFmt, zoneId)
	res, err := svcCtx.Redis.Eval(luaReleaseWorldInitLock, []string{key}, token)
	if err != nil {
		// 释放失败无碍正确性:锁到 TTL 自然过期,只是这段时间别的补建要等。
		logx.Errorf("[World] Zone %d: release init lock failed: %v", zoneId, err)
		return
	}
	if toInt64(res) == 0 {
		// 属主校验没通过 = 本次补建超过了 60s TTL,互斥在中途已经失效。
		// 这必须可见:说明有节点把 CreateScene 拖得极慢(黑洞节点超时不够
		// 用?),需要人查,而不是悄悄接受可能的双建。
		logx.Errorf("[World] Zone %d: init lock expired mid-run (TTL %ds) — mutual exclusion lapsed, investigate slow CreateScene RPCs",
			zoneId, worldInitLockTTLSeconds)
	}
}

type WorldChannelCandidate struct {
	SceneID uint64
	NodeID  string
	ZoneID  uint32
}

func worldChannelsKey(zoneID uint32, confId uint64) string {
	return fmt.Sprintf(WorldChannelsKeyFmt, zoneID, confId)
}

// initWorldScenesForZone ensures world scenes (with N channels each) exist for a single zone.
// Called by fullSync (on startup / re-sync) and handleWatchEvent (on node PUT).
// Two-layer idempotency:
//   - Redis layer: only creates missing channels (if existing < ChannelCount).
//   - C++ layer: CreateScene is idempotent by scene_id (returns existing entity).
//
// Always sends CreateScene RPC for all channels to handle C++ node restarts.
//
// Node selection uses the world-hosting pool (scene_node_type ∈
// {kMainSceneNode, kMainSceneCrossNode}) so instance-only nodes are never
// handed a persistent world channel. When StrictNodeTypeSeparation=false and
// the pool is empty, selection falls back to any available node in the zone
// (dev / single-process convenience).
// waitForLock:锁忙时是否忙等(预算 worldInitLockWait)。RPC 兜底传 true
// (必须在返回前看到频道存在),后台路径传 false(跳过,下一拍收敛)。
func initWorldScenesForZone(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, confIds []uint64, waitForLock bool) {
	// zone 级互斥:见 worldInitLockKeyFmt 的注释。拿不到锁就跳过本轮 ——
	// 等到锁的一方会重查集合、只补真正缺的;拿不到的调用方靠自身重试
	// (RPC 兜底会重查 GetBestWorldChannel)或下一拍后台循环收敛。
	maxWait := time.Duration(0)
	if waitForLock {
		maxWait = worldInitLockWait
	}
	lockToken, locked := acquireWorldInitLock(ctx, svcCtx, zoneId, maxWait)
	if !locked {
		logx.Infof("[World] Zone %d: init lock busy/unavailable, skipping this init round", zoneId)
		return
	}
	defer releaseWorldInitLock(svcCtx, zoneId, lockToken)

	nodes := getNodesForPurpose(svcCtx, zoneId, constants.NodePurposeWorld)
	if len(nodes) == 0 {
		logx.Errorf("[World] Zone %d: no world-hosting nodes available (strict=%v), skipping",
			zoneId, svcCtx.Config.StrictNodeTypeSeparation)
		return
	}
	// assignNodeByHash needs a stable ordering independent of live load
	// scores, otherwise the same (confId, channel) could land on different
	// nodes across two scheduling runs whenever a node's score changed.
	sort.Strings(nodes)

	logx.Infof("[World] Zone %d: ensuring %d world scenes across %d world-hosting nodes",
		zoneId, len(confIds), len(nodes))

	// 本轮里 RPC 不可达(拒连 / 超时)的节点:后续场景不再对它白等一次超时 ——
	// 本段在 zone 锁内,黑洞节点每个场景等 5s 会把持锁时长顶过 TTL。
	// **不可达 ≠ 已死**:高负载下一个只是慢了的活节点同样会超时,所以这张表只用来
	// 「本轮跳过」,改写场景归属还要另看 etcd 注册表与负载集(见下面两处 isNodeGoneFromRegistry
	// 与 !IsNodeAlive)。
	unreachableNodes := make(map[string]struct{})
	liveNodes := make([]string, len(nodes))
	copy(liveNodes, nodes)

	created, ensured := 0, 0

	for _, confId := range confIds {
		channelSetKey := worldChannelsKey(zoneId, confId)
		// 期望频道数的权威在 Redis,不是配置 —— 配置只是首次播种的种子。
		// 直接读 ChannelCountFor 的话,自动缩容刚摘掉的频道会在这里被
		// 重新补出来,缩容根本不成立。见 world_autoscale.go 顶部注释。
		// 期望数与现有集合任一读不到,就不知道缺几个:期望数读错当种子值会把缩掉的频道补回来,
		// 集合读错当 0 会把整组频道再建一遍(多铸 scene_id、多占 Agones 名额)。
		// 本 conf 本轮跳过,下一拍或 RPC 兜底的重查再补。
		channelCount, err := DesiredWorldChannelCount(svcCtx, zoneId, confId)
		if err != nil {
			noteReadUnknown(zoneId, readUnknownSiteWorldInit, fmt.Sprintf("desired channel count of conf %d", confId), err)
			continue
		}

		// Get existing channels for this confId.
		existingMembers, err := svcCtx.Redis.Smembers(channelSetKey)
		if err != nil {
			noteReadUnknown(zoneId, readUnknownSiteWorldInit, fmt.Sprintf("channel set of conf %d", confId), err)
			continue
		}
		existingCount := len(existingMembers)

		// Create missing channels — only assign to live nodes.
		for i := existingCount; i < channelCount; i++ {
			if len(liveNodes) == 0 {
				logx.Errorf("[World] No live nodes remaining for zone %d, stopping allocation", zoneId)
				break
			}

			// 这条路径由后台 goroutine(load_reporter / world_autoscale)周期驱动,
			// 不在 gRPC 请求链上。失租后发号器被 fence,这里必须停止铺频道 ——
			// 否则会用一个已被别人接管的 worker id 继续铸 scene_id。
			sceneId, genErr := svcCtx.SceneIDGen.Generate()
			if genErr != nil {
				logx.Errorf("[World] scene id generator unavailable for zone %d: %v; stopping allocation", zoneId, genErr)
				break
			}

			// Vary hash input per channel so channels may land on different nodes.
			targetNode := assignNodeByHash(confId*1000+uint64(i), liveNodes)

			// Agones 高密度模式:世界频道也要占一个 rooms 名额。
			//
			// 漏掉这一步的话 rooms Counter 只统计副本,而 FleetAutoscaler 用的
			// 正是 Counter=rooms —— 大世界人数涨到要开新频道时,Pod 完全不会
			// 跟着扩。优先按哈希目标节点占,占不到才回落自由分配(落点可能偏离
			// 哈希目标,rebalance 之后会把它搬回去)。
			//
			// 占不到任何容量时 fail-closed:不建这个频道。建了也没有进程承载,
			// 只会得到一个 Agones 不认账的房间。
			actualNode, placement, ok := ReserveAgonesRoomForWorldChannel(ctx, svcCtx, sceneId, targetNode, zoneId)
			if !ok {
				logx.Errorf("[World] zone=%d conf=%d: no Agones room capacity for a new channel, skipping",
					zoneId, confId)
				break
			}
			targetNode = actualNode

			// 频道的全部 Redis 状态(映射、zone、频道集合、节点计数、节点场景集合、agones_gs)由一段脚本登记,
			// 见 scene_registration.go。节点场景集合让死节点收尾也知道这个频道:孤儿清理不销毁世界频道
			// (它们走 rebalance),但集合要准,诊断靠它。
			reg := sceneRegistration{SceneID: sceneId, ZoneID: zoneId, NodeID: targetNode}
			if placement != nil {
				reg.AgonesGs = placement.GameServerName
			}
			if outcome := registerWorldChannel(svcCtx, reg, confId); outcome != registrationRegistered {
				if placement != nil && outcome.releasesReservation() {
					ReleaseAgonesRoom(ctx, svcCtx, placement.Namespace, placement.GameServerName, "scene_registration_"+outcome.String())
				}
				logx.Errorf("[World] channel %d (scene=%d, conf=%d) on node %s not registered: %s",
					i, sceneId, confId, targetNode, outcome)
				continue
			}

			created++
			logx.Infof("[World] Allocated channel %d (scene=%d, conf=%d) on node %s in zone %d",
				i, sceneId, confId, targetNode, zoneId)
		}

		// Always send CreateScene RPC for all channels — C++ deduplicates by scene_id.
		// If a channel's target node is dead, reassign to a live node.
		allMembers, err := svcCtx.Redis.Smembers(channelSetKey)
		if err != nil {
			noteReadUnknown(zoneId, readUnknownSiteWorldInit, fmt.Sprintf("channel set of conf %d", confId), err)
			continue
		}
		for _, sceneIdStr := range allMembers {
			sceneId, _ := strconv.ParseUint(sceneIdStr, 10, 64)
			nodeKey := fmt.Sprintf("scene:%d:node", sceneId)
			targetNode, err := svcCtx.Redis.Get(nodeKey)
			if err != nil {
				noteReadUnknown(zoneId, readUnknownSiteWorldInit, fmt.Sprintf("owner of scene %d", sceneId), err)
				continue
			}
			if targetNode == "" {
				logx.Errorf("[World] Missing node for scene %d, skipping RPC", sceneId)
				continue
			}

			// 目标节点本轮已知不可达:不再对它发 RPC。
			if _, unreachable := unreachableNodes[targetNode]; unreachable {
				// 仍在 etcd 注册表里 = 拿不到它已死的证据(可能只是慢,也可能注册表尚未完成
				// 首次同步)。此时改写归属并在别的节点上建同一个 scene_id,会让同一个世界频道
				// 在两个节点上各有一份活副本:老节点上的玩家与新进来的玩家互相不可见,
				// 路由与 PlayerLocation 也对不上。归属不动,本轮跳过;节点若真死了,etcd 注销
				// 路径会先写 death_at 再摘负载集(写不进时推迟摘除,GO-6;改派同时要求已摘出
				// 负载集,见下一段),之后的 rebalance / 下一轮 init 再按屏障改派。
				if !isNodeGoneFromRegistry(zoneId, targetNode) {
					logx.Infof("[World] Scene %d stays on node %s: unreachable this round but still registered in etcd", sceneId, targetNode)
					continue
				}
				// 注册表里没了,但还在负载集里:领导者还没处理这次注销,或 death_at 写不进、摘除被推迟(GO-6),
				// 或本进程不是持有推迟队列的那个领导者(CreateScene 慢路径会在任意副本上跑到这里)。
				// 负载集成员资格是「death_at 已落地或已满一个屏障」唯一跨进程可见的证据,没摘就不改派。
				if IsNodeAlive(svcCtx, zoneId, targetNode) {
					logx.Infof("[World] Scene %d stays on node %s: gone from etcd but still in the load set (death_at not landed yet)", sceneId, targetNode)
					continue
				}
				if len(liveNodes) == 0 {
					logx.Errorf("[World] No live nodes remaining, skipping scene %d", sceneId)
					continue
				}
				newNode := assignNodeByHash(confId*1000+uint64(ensured), liveNodes)
				logx.Infof("[World] Reassigning scene %d from dead node %s to live node %s", sceneId, targetNode, newNode)
				// 建实体、改归属、转名额都在 moveWorldChannelOffDeadNode 里做完(先建后改),
				// 下面对属主的 CreateScene 就不再发了。
				rpcSent, createErr := moveWorldChannelOffDeadNode(ctx, svcCtx, zoneId, confId, sceneId, targetNode, newNode)
				if createErr != nil {
					logx.Errorf("[World] Failed to call CreateScene for conf %d scene %d on node %s: %v", confId, sceneId, newNode, createErr)
					// 与下面对属主发 RPC 失败同口径:记为本轮不可达,剩下的场景不再对它白等。
					if isNodeUnreachableError(createErr) {
						markNodeUnreachable(zoneId, newNode, unreachableNodes, &liveNodes)
					}
				}
				if rpcSent {
					ensured++
				}
				continue
			}

			// 每个 RPC 限时:本段在 zone 锁内,黑洞节点的无时限 RPC 会把
			// 持锁时长顶过 TTL(见 worldInitCreateRPCTimeout 注释)。
			// CreateScene 按 scene_id 幂等,超时安全。
			rpcCtx, rpcCancel := context.WithTimeout(ctx, worldInitCreateRPCTimeout)
			_, rpcErr := RequestNodeCreateScene(rpcCtx, svcCtx, zoneId, targetNode, uint32(confId), sceneId)
			rpcCancel()
			if rpcErr != nil {
				logx.Errorf("[World] Failed to call CreateScene for conf %d scene %d: %v", confId, sceneId, rpcErr)
				// 记为本轮不可达,剩下的场景不再对它白等。
				if isNodeUnreachableError(rpcErr) {
					markNodeUnreachable(zoneId, targetNode, unreachableNodes, &liveNodes)
					// 只有节点已确认从 etcd 注册表消失、且领导者已把它摘出负载集,才把这个场景
					// 改派到活节点并重试一次;仅仅超时/拒连不是死亡证据,「注册表没了但还在负载集」
					// 说明 death_at 可能还没落地(GO-6)。理由同上面 RPC 之前的那两段。
					if isNodeGoneFromRegistry(zoneId, targetNode) && !IsNodeAlive(svcCtx, zoneId, targetNode) && len(liveNodes) > 0 {
						newNode := assignNodeByHash(confId*1000+uint64(ensured), liveNodes)
						logx.Infof("[World] Retrying scene %d on live node %s after dead node %s", sceneId, newNode, targetNode)
						// 同上一段:先建后改,屏障未到或归属已被别的写者改走时连 CreateScene 都不发。
						if _, retryErr := moveWorldChannelOffDeadNode(ctx, svcCtx, zoneId, confId, sceneId, targetNode, newNode); retryErr != nil {
							logx.Errorf("[World] Retry also failed for conf %d scene %d on node %s: %v", confId, sceneId, newNode, retryErr)
						}
					}
				}
			}
			ensured++
		}
	}

	logx.Infof("[World] Zone %d done: created=%d channels, ensured=%d RPCs, unreachable_nodes=%d",
		zoneId, created, ensured, len(unreachableNodes))
}

// moveWorldChannelOffDeadNode 是 initWorldScenesForZone 的死节点改派(发 RPC 之前已知不可达、对属主的 RPC 失败后
// 重试,两处共用):把频道 sceneId 从已判死的 deadNode 搬到 newNode。顺序与 rebalance(migrateWorldChannel)、
// resolveScene 的死节点自愈、懒改派相同,先建后改(2026-11-05):
//  1. 重读属主,仍是 deadNode 才往下做,再过再入屏障。读错、属主已被别的写者改走、屏障未到,都本轮跳过:一个字节
//     不改,也不发 RPC。这一步只为不白建白拆(屏障窗口里每一轮 init 都会走到这里),不代替 CAS —— 重读到提交之间
//     仍可能被改,由第 3 步的 CAS 兜底。屏障在 CAS 之前还会再判一次(casReassignSceneNode),两次都记在 reassign 点位。
//  2. 在 newNode 上 CreateScene(按 scene_id 幂等;本函数跑在 zone 锁内,限时 worldInitCreateRPCTimeout)。
//     失败即返回错误,归属不变,下一轮 init 从头再来。
//  3. 改归属、转名额,落空回收预建的实体:commitPreCreatedWorldChannel。
//
// 以前是「CAS → 转名额 → CreateScene」。CAS 落地之后、CreateScene 到达 newNode 之前,频道若被按新属主拆掉(排空
// 收尾先向属主发 DestroyScene 再原子删映射,显式 DestroyScene、孤儿清理同样按属主发),或被 rebalance 再次搬走,
// 晚到的 CreateScene 会在 newNode 上留下一个没有任何 Redis 状态的幽灵实体,不会有任何路径来收它;CreateScene
// 失败时归属也已经指了过去,要等下一轮 init 补建。先建后改之后,归属落地时实体已经在,之后的拆除按新属主拆得到它。
//
// 返回 rpcSent:本次是否向 newNode 发过 CreateScene(调用方据此累加本轮 ensured);createErr:CreateScene 的错误。
func moveWorldChannelOffDeadNode(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, confId, sceneId uint64, deadNode, newNode string) (bool, error) {
	owner, err := svcCtx.Redis.Get(sceneNodeKey(sceneId))
	if err != nil {
		noteReadUnknown(zoneId, readUnknownSiteWorldInit,
			fmt.Sprintf("owner of scene %d before moving it off dead node %s", sceneId, deadNode), err)
		return false, nil
	}
	if owner != deadNode {
		logx.Infof("[World] Scene %d is no longer owned by dead node %s (owner %q); not moving it to %s this round",
			sceneId, deadNode, owner, newNode)
		return false, nil
	}
	if reentryBarrierBlocks(svcCtx, zoneId, deadNode, barrierSiteReassign) {
		return false, nil
	}
	rpcCtx, rpcCancel := context.WithTimeout(ctx, worldInitCreateRPCTimeout)
	_, err = RequestNodeCreateScene(rpcCtx, svcCtx, zoneId, newNode, uint32(confId), sceneId)
	rpcCancel()
	if err != nil {
		return true, err
	}
	commitPreCreatedWorldChannel(ctx, svcCtx, zoneId, confId, sceneId, deadNode, newNode)
	return true, nil
}

// GetBestWorldChannel returns the (sceneId, nodeId) of the least-loaded channel
// for the given world confId in the zone. Returns (0, "", nil) if no channels exist.
//
// Self-healing: if all channels are mapped to dead nodes (e.g. the old scene node
// crashed and a new one just registered), it lazily reassigns stale channels to
// live nodes instead of returning "no channel". This covers the race window between
// a new node's etcd PUT event and initWorldScenesForZone completing reassignment.
//
// 属主(scene:{id}:node)读不到的频道既不当候选也不当陈旧,本轮跳过;一个频道都没选出来、
// 且有被跳过的,回可重试的 error(见 worldChannelOwnersUnknownError)。
func GetBestWorldChannel(ctx context.Context, svcCtx *svc.ServiceContext, confId uint64, zoneId uint32) (uint64, string, error) {
	channelSetKey := worldChannelsKey(zoneId, confId)
	members, err := svcCtx.Redis.Smembers(channelSetKey)
	if err != nil || len(members) == 0 {
		return 0, "", err
	}

	var bestSceneId uint64
	var bestNodeId string
	bestCount := int64(math.MaxInt64)

	var staleSceneIds []uint64
	// 属主读不到的频道数。它们既不是候选也不算陈旧;本次一个都没选出来时据此回错误(可重试),
	// 而不是「没有频道」。
	unknownOwners := 0

	for _, m := range members {
		sceneId, _ := strconv.ParseUint(m, 10, 64)
		if sceneId == 0 {
			continue
		}

		nodeKey := fmt.Sprintf("scene:%d:node", sceneId)
		nid, getErr := svcCtx.Redis.Get(nodeKey)
		if getErr != nil {
			// 读错不等于映射不存在:属主可能活着。当成陈旧会进懒改派,下一次读若成功就会拿着
			// 活属主做 CAS,把频道从活节点上搬走、在新节点上再建一份同 scene_id 的实体。
			noteReadUnknown(zoneId, readUnknownSiteWorldPick, fmt.Sprintf("owner of scene %d", sceneId), getErr)
			unknownOwners++
			continue
		}
		if nid != "" && isKnownNodeIdentityAmbiguous(zoneId, nid) {
			logx.Errorf("[World] scene %d maps to duplicate identity zone=%d node=%s; refusing stale-channel reassignment",
				sceneId, zoneId, nid)
			continue
		}
		if nid == "" || !IsNodeAlive(svcCtx, zoneId, nid) {
			staleSceneIds = append(staleSceneIds, sceneId)
			continue
		}

		count := readScenePlayerCount(svcCtx, sceneId)

		if count < bestCount {
			bestCount = count
			bestSceneId = sceneId
			bestNodeId = nid
		}
	}

	if bestSceneId > 0 {
		return bestSceneId, bestNodeId, nil
	}

	// All channels mapped to dead nodes — lazy-reassign to live world-hosting
	// nodes. Mixing purposes here would put a persistent world onto an
	// instance-only node and future EnterScene requests would be rejected by
	// the C++ handler (see player_scene_handler.cpp).
	liveNodes := getNodesForPurpose(svcCtx, zoneId, constants.NodePurposeWorld)
	sort.Strings(liveNodes)
	if len(liveNodes) == 0 {
		logx.Errorf("[World] All %d channels for conf %d (zone %d) are stale and no live nodes available",
			len(staleSceneIds), confId, zoneId)
		return 0, "", worldChannelOwnersUnknownError(confId, zoneId, unknownOwners)
	}

	logx.Infof("[World] All %d channels for conf %d (zone %d) are stale, lazy-reassigning to %d live nodes",
		len(staleSceneIds), confId, zoneId, len(liveNodes))

	for i, sceneId := range staleSceneIds {
		targetNode := assignNodeByHash(confId*1000+uint64(i), liveNodes)
		if !lazyReassignStaleWorldChannel(ctx, svcCtx, zoneId, confId, sceneId, targetNode) {
			continue
		}
		logx.Infof("[World] Lazy-reassigned stale channel (scene=%d, conf=%d) to live node %s", sceneId, confId, targetNode)
		return sceneId, targetNode, nil
	}

	return 0, "", worldChannelOwnersUnknownError(confId, zoneId, unknownOwners)
}

// lazyReassignStaleWorldChannel 把一个已判陈旧的世界频道懒改派到 targetNode,返回 true 表示频道已经由
// targetNode 承载(实体已建、归属已改)。两个懒改派调用方(GetBestWorldChannel / ReserveBestWorldChannelForEnter)
// 共用它,顺序与 rebalance 的 migrateWorldChannel、resolveScene 的死节点自愈一致:先建后改(2026-10-23)。
//  1. staleWorldChannelOwner:重读属主并复核存活、身份歧义与再入屏障,廉价的判断都在发 RPC 之前做完 ——
//     刚判死的节点在屏障窗口里会挡住每一个进场请求,不能让每个请求都白建白拆一次。
//  2. CreateScene(按 scene_id 幂等):归属一旦落地就有实体可进,不会出现「映射指向一个还没建出实体的节点」。
//  3. reassignWorldChannelNode:属主 CAS 与在役成员资格在同一个脚本里判定。在役核对是正确性前提:调用方读到
//     频道列表之后,排空收尾或孤儿清理可能已经原子地拆掉了这个频道(删映射并摘出集合,见 atomicTeardownScene)。
//     此时映射不存在,只比属主的 CAS(期望空串)会把它复活成一个哪个集合都不列的频道 —— 实体、记账与
//     Agones 名额从此没有任何路径能收回。落空(属主已变或已不在役)就回收第 2 步预建的实体。
//  4. 转 Agones 名额:reassignSceneNode 的契约把它交给调用方(以前懒改派漏了 —— agones_gs 仍指向老 GameServer,
//     新节点的 rooms Counter 不记账)。TransferAgonesRoomForScene 要求调用时归属已是 targetNode,第 3 步成功后正好满足。
//
// 第 3、4 步与 init 的死节点改派共用 commitPreCreatedWorldChannel(2026-11-05 抽出),落空回收与转名额的 ctx 见该函数。
func lazyReassignStaleWorldChannel(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, confId, sceneId uint64, targetNode string) bool {
	oldNode, ok := staleWorldChannelOwner(svcCtx, zoneId, sceneId)
	if !ok || oldNode == targetNode {
		return false
	}
	if _, err := RequestNodeCreateScene(ctx, svcCtx, zoneId, targetNode, uint32(confId), sceneId); err != nil {
		logx.Errorf("[World] Lazy-reassign: CreateScene failed for scene %d on node %s: %v", sceneId, targetNode, err)
		return false
	}
	return commitPreCreatedWorldChannel(ctx, svcCtx, zoneId, confId, sceneId, oldNode, targetNode)
}

// commitPreCreatedWorldChannel 是世界频道换节点「先建后改」的提交一半:调用方已经在 newNode 上 CreateScene 成功
// (按 scene_id 幂等),这里改归属并转名额。懒改派与 init 的死节点改派共用;rebalance(迁移后还要拆老实体、级联镜像)
// 与 resolveScene 的死节点自愈(落空后要重读属主定应答)各自另写。
//  1. reassignWorldChannelNode:属主 CAS 与在役成员资格同一个脚本判定(在役核对为什么必要见该函数注释)。
//     落空(属主已变、已不在役、屏障未到,或 Redis 出错、结论未知)就回收预建的实体:destroyUnownedMigrationTarget
//     先重读属主,恰是 newNode(别的写者派到了同一个节点,或脚本其实已执行)或重读失败时保留。
//  2. 转 Agones 名额。TransferAgonesRoomForScene 要求调用时归属已是 newNode,第 1 步成功后正好满足。
//
// 回收与转名额都是 K8s / RPC 写入,不挂调用方 ctx:取消或超时时写入可能已经生效、本端却拿到错误(实体没回收,
// 或名额白占一份)。用 WithoutCancel(ctx) 加 deadNodeReassignFollowUpTimeout,与 resolveScene 的死节点自愈同一时限。
// 返回 true 表示归属已落到 newNode。
func commitPreCreatedWorldChannel(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, confId, sceneId uint64, oldNode, newNode string) bool {
	followCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deadNodeReassignFollowUpTimeout)
	defer cancel()
	if !reassignWorldChannelNode(svcCtx, zoneId, sceneId, oldNode, newNode, worldChannelsKey(zoneId, confId)) {
		destroyUnownedMigrationTarget(followCtx, svcCtx, zoneId, confId, sceneId, newNode)
		return false
	}
	TransferAgonesRoomForScene(followCtx, svcCtx, sceneId, newNode, zoneId)
	return true
}

// worldChannelOwnersUnknownError:本次一个频道都没选出来时,若有频道是因为属主读不到而被跳过,
// 回一个可重试的错误,而不是「没有频道」。EnterScene 把它翻成「暂无可用频道」,CreateScene 回 ErrRedis,
// 都由上游退避重试,不会触发按需补建。
// 没有读不到的频道时回 nil,保持原来「确实没有可用频道」的语义。
func worldChannelOwnersUnknownError(confId uint64, zoneId uint32, unknownOwners int) error {
	if unknownOwners == 0 {
		return nil
	}
	return fmt.Errorf("world channel owners unreadable: conf=%d zone=%d channels=%d", confId, zoneId, unknownOwners)
}

// staleWorldChannelOwner 在懒改派动手之前重读一个已判陈旧的世界频道的属主,判断它此刻是否仍可被接管。
// ok=true 时 oldNode 是重读到的属主(空串 = 映射不存在),调用方拿它做 reassignWorldChannelNode 的期望值;
// ok=false 表示本轮不接管,什么都没写。
//
// 判陈旧那次读与这里隔着别的频道的读与 RPC,所以重读,按读到的结果分开处理:
//   - 读错:不知道当前属主,本轮放弃。以前读错被当成「没有属主」,随后的 CAS(期望值为空串)
//     恰好挡住了写,但这是巧合,不是契约;
//   - 有值且属主活着(或身份有歧义):频道已经不陈旧了(判陈旧时映射还不存在、之后被补建,或被
//     rebalance 改派到活节点)。CAS 只比属主、不看存活,放过去就会把它从活节点上搬走,同一 scene_id
//     两份实体;
//   - 有值且属主已死:老属主刚判死时仍可能在 SavePlayer,再入屏障没走完就不抢;
//   - 不存在(Nil):没有活着的属主在写它,交给 CAS。映射不存在也可能是因为频道刚被排空收尾或孤儿清理
//     原子拆掉,所以调用方的 CAS 必须同时核对在役成员资格(reassignWorldChannelNode),不能只比属主。
//
// 归属的写入在调用方走 reassignWorldChannelNode 的 CAS:这条路径在任意副本的 EnterScene 上跑,与领导者的
// rebalance、initWorldScenesForZone、排空收尾、孤儿清理并发,归属已被别人改走或频道已不在役就落空。存活判定
// 没法放进 CAS 脚本 —— 同一脚本也服务 rebalance 从活节点主动迁出 —— 所以「重读 + 存活复核」与 CAS 之间仍有
// 一条缝(中间隔着一次 CreateScene),比原来的「跨多个频道与 RPC」窄得多。
func staleWorldChannelOwner(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64) (string, bool) {
	oldNode, err := svcCtx.Redis.Get(sceneNodeKey(sceneId))
	if err != nil {
		noteReadUnknown(zoneId, readUnknownSiteWorldLazy, fmt.Sprintf("owner of scene %d before lazy reassign", sceneId), err)
		return "", false
	}
	if oldNode != "" && (isKnownNodeIdentityAmbiguous(zoneId, oldNode) || IsNodeAlive(svcCtx, zoneId, oldNode)) {
		logx.Infof("[World] scene %d is no longer stale (owner %s alive or ambiguous in zone %d); skipping lazy reassign",
			sceneId, oldNode, zoneId)
		return "", false
	}
	// 再入屏障:老属主刚判死时仍可能在 SavePlayer,懒改派同样不能抢。
	// 换下一个陈旧频道试,全都被挡住就返回"没有可用频道",上游退避重试。
	if oldNode != "" && reentryBarrierBlocks(svcCtx, zoneId, oldNode, barrierSiteWorldChannelLazy) {
		return "", false
	}
	return oldNode, true
}

// ReserveBestWorldChannelForEnter selects and reserves a world channel for an
// EnterScene request. It preserves GetBestWorldChannel's stale-channel healing,
// then performs the final least-loaded choice and member reservation in
// one Redis Lua script.
func ReserveBestWorldChannelForEnter(ctx context.Context, svcCtx *svc.ServiceContext, confId uint64, zoneId uint32, playerId uint64) (uint64, string, error) {
	channelSetKey := worldChannelsKey(zoneId, confId)
	members, err := svcCtx.Redis.Smembers(channelSetKey)
	if err != nil || len(members) == 0 {
		return 0, "", err
	}

	var candidates []WorldChannelCandidate
	var staleSceneIds []uint64
	// 同 GetBestWorldChannel:属主读不到的频道既不是候选也不算陈旧。
	unknownOwners := 0

	for _, m := range members {
		sceneId, _ := strconv.ParseUint(m, 10, 64)
		if sceneId == 0 {
			continue
		}

		nodeKey := fmt.Sprintf("scene:%d:node", sceneId)
		nid, getErr := svcCtx.Redis.Get(nodeKey)
		if getErr != nil {
			// 同 GetBestWorldChannel:读错不当陈旧,否则可能把活属主的频道懒改派走。
			noteReadUnknown(zoneId, readUnknownSiteWorldPick, fmt.Sprintf("owner of scene %d", sceneId), getErr)
			unknownOwners++
			continue
		}
		if nid != "" && isKnownNodeIdentityAmbiguous(zoneId, nid) {
			logx.Errorf("[World] scene %d maps to duplicate identity zone=%d node=%s; refusing reservation-time reassignment",
				sceneId, zoneId, nid)
			continue
		}
		if nid == "" || !IsNodeAlive(svcCtx, zoneId, nid) {
			staleSceneIds = append(staleSceneIds, sceneId)
			continue
		}
		candidates = append(candidates, WorldChannelCandidate{SceneID: sceneId, NodeID: nid, ZoneID: zoneId})
	}

	if len(candidates) == 0 {
		liveNodes := getNodesForPurpose(svcCtx, zoneId, constants.NodePurposeWorld)
		sort.Strings(liveNodes)
		if len(liveNodes) == 0 {
			logx.Errorf("[World] All %d channels for conf %d (zone %d) are stale and no live nodes available",
				len(staleSceneIds), confId, zoneId)
			return 0, "", worldChannelOwnersUnknownError(confId, zoneId, unknownOwners)
		}

		logx.Infof("[World] All %d channels for conf %d (zone %d) are stale, lazy-reassigning to %d live nodes",
			len(staleSceneIds), confId, zoneId, len(liveNodes))

		for i, sceneId := range staleSceneIds {
			targetNode := assignNodeByHash(confId*1000+uint64(i), liveNodes)
			// 同 GetBestWorldChannel:重读属主并复核、CreateScene、CAS(落空回收预建实体)、转名额。
			if !lazyReassignStaleWorldChannel(ctx, svcCtx, zoneId, confId, sceneId, targetNode) {
				continue
			}
			candidates = append(candidates, WorldChannelCandidate{SceneID: sceneId, NodeID: targetNode, ZoneID: zoneId})
		}
	}

	if len(candidates) == 0 {
		if unknownErr := worldChannelOwnersUnknownError(confId, zoneId, unknownOwners); unknownErr != nil {
			return 0, "", unknownErr
		}
	}

	sceneId, nodeId, _, err := ReserveBestWorldChannel(svcCtx, candidates, playerId)
	if err != nil {
		return 0, "", err
	}
	return sceneId, nodeId, nil
}

// luaReassignSceneNode 是 scene:{id}:node 的比较并交换:只有当前属主仍是调用方
// 规划时看到的 oldNode,才把归属改到 newNode,并在同一个脚本里同步两侧的反向索引
// 与 scene_count。比较和写入之间没有缝,并发的两个改派者只有一个能落地。
//
// KEYS[1] = scene:{id}:node
// KEYS[2] = node:zone:{zoneId}:{oldNode}:scenes
// KEYS[3] = node:zone:{zoneId}:{newNode}:scenes
// KEYS[4] = node:zone:{zoneId}:{oldNode}:scene_count
// KEYS[5] = node:zone:{zoneId}:{newNode}:scene_count
// KEYS[6] = scene:{id}:reassign_receipt:{token},本次调用的回执(见下)
// KEYS[7] = (可选)world_channels:zone:{zoneId}:{confId};传了就要求 sceneId 仍是它的成员
// ARGV[1] = oldNode;空串表示期望映射不存在(懒改派里没有属主的频道)
// ARGV[2] = newNode
// ARGV[3] = sceneId
// ARGV[4] = 回执存活秒数
// 返回 1 = 已改派;0 = 当前属主不是 oldNode,或这次调用已被作废;2 = 频道已不在役(KEYS[7] 里没有它)。
// 0 和 2 都一个键没动。
//
// 回执(2026-11-05,与销毁认领的 luaSceneClaimPrelude、新场景登记的 luaAbortSceneRegistration 同一思路)。
// go-redis 对读超时 / EOF 会原样重发 EVAL;第一次已经改派、回包丢了,重发时属主已是 newNode、不等于 oldNode,
// 没有回执会回 0:调用方按「抢输」走,转名额、rebalance 拆老实体与级联镜像都被跳过,老节点上的活实体与映射分家。
//   - 改派成功时写回执 'applied',同一 token 再来直接回 1、什么都不动。落空(0 / 2)不写:那两种什么都没改,
//     重发时重新判定就是对的。
//   - Eval 最终回错时调用方用 luaResolveCasReceipt 问清结局:还没执行就写下 'aborted',这个 token 的 EVAL
//     之后即使晚到也回 0、一个键不动。
// 回执键按 token 分开,不与同一场景的其它改派共用一把键:共用的话,并发的另一次改派会覆盖掉本次的回执。
//
// 新节点的 INCR 放在所有其它写之前:Lua 脚本中途报错时已执行的写不会回滚,计数键里
// 若是非整数(INCR 唯一会失败的情形),要在动归属之前就失败,不留半截改派。
// 老节点的 scene_count 只在 > 0 时才减:判死的节点由死节点收尾(finishDeadNodeReconcile)删掉计数键,
// 这里无条件 DECR 会把键重建成 -1,再被复用同 id 的新节点继承。
const luaReassignSceneNode = `
local receipt = redis.call('GET', KEYS[6])
if receipt == 'applied' then
    return 1
end
if receipt == 'aborted' then
    return 0
end
local cur = redis.call('GET', KEYS[1])
if not cur then
    cur = ''
end
if cur ~= ARGV[1] then
    return 0
end
if #KEYS >= 7 and redis.call('SISMEMBER', KEYS[7], ARGV[3]) == 0 then
    return 2
end
redis.call('INCR', KEYS[5])
redis.call('SET', KEYS[1], ARGV[2])
redis.call('SADD', KEYS[3], ARGV[3])
if ARGV[1] ~= '' then
    redis.call('SREM', KEYS[2], ARGV[3])
    local n = tonumber(redis.call('GET', KEYS[4]) or '0') or 0
    if n > 0 then
        redis.call('DECR', KEYS[4])
    end
end
redis.call('SET', KEYS[6], 'applied', 'EX', ARGV[4])
return 1
`

// sceneReassignReceiptKey 是 luaReassignSceneNode 一次调用的回执键,存活 sceneClaimRecordTTLSeconds(与认领记录
// 同一个口径:长过 go-redis 的重发,也长过一个卡在旧连接里的 EVAL)。
func sceneReassignReceiptKey(sceneID uint64, token string) string {
	return fmt.Sprintf("scene:%d:reassign_receipt:%s", sceneID, token)
}

// casReceiptApplied / casReceiptAborted 是两种 CAS 回执(改派、名额转移)里的状态字面量,要与脚本里的一致。
// 名额转移成功时回执里是 cjson,只有作废标记是这个字面量。
const (
	casReceiptApplied = "applied"
	casReceiptAborted = "aborted"
)

// luaResolveCasReceipt 在改派 / 名额转移的 Eval 结论未知时问清结局,并保证它之后不会再落地:
// 本次 token 的回执已在,原样返回(已经落地,或上一次问的时候已经作废);不在就写下作废标记再返回它。
// 这个 token 的 EVAL 之后即使才到,读到作废标记也一个键不动。只读一下回执判断不了「没落地」:
// 原来那次 EVAL 可能还在路上,读完之后才执行(与 luaAbortSceneRegistration 同一理由)。
//
// KEYS[1] = 本次 token 的回执键,ARGV[1] = 存活秒数。
const luaResolveCasReceipt = `
local receipt = redis.call('GET', KEYS[1])
if receipt then
    return receipt
end
redis.call('SET', KEYS[1], 'aborted', 'EX', ARGV[1])
return 'aborted'
`

// resolveCasReceipt 跑 luaResolveCasReceipt,返回回执内容:作废时是 casReceiptAborted,改派落地时是
// casReceiptApplied,名额转移落地时是它的 cjson 回执。出错时结局仍未知。
func resolveCasReceipt(svcCtx *svc.ServiceContext, receiptKey string) (string, error) {
	raw, err := svcCtx.Redis.Eval(luaResolveCasReceipt, []string{receiptKey}, sceneClaimRecordTTLSeconds)
	if err != nil {
		return "", err
	}
	return toString(raw), nil
}

// reassignSceneNode 把场景归属从 oldNode 原子地改到 newNode:scene:{id}:node、两侧的
// node:zone:{zoneId}:{nodeId}:scenes 反向索引、两侧的 scene_count 在一个 Lua 脚本里一起改。
//
// 这是**所有权改写的收口点**:全模块改写已有场景的 scene:{id}:node 的路径都经过它
// (建场景时的首次写入不算改写,见 scene_registration.go)。
// 再入屏障在这里再判一次 —— 调用方在计划阶段判过的那次可能已经隔了几个
// RPC,期间老节点的死亡时刻才刚写进 Redis。
//
// 写入是比较并交换。改派者不止一个,彼此也不共用锁:领导者的 rebalance
// (migrateWorldChannel,不持 zone 锁)、任意副本上的 initWorldScenesForZone(持 zone 锁)、
// EnterScene 路径上的懒改派(GetBestWorldChannel / ReserveBestWorldChannelForEnter),以及
// EnterScene 指定场景时的死节点自愈(enterscenelogic.go reassignSceneOffDeadNode,2026-10-22 起,
// 2026-10-23 起经带在役核对的 reassignWorldChannelNode)。
// 它们选新节点的依据不同(前三者按各自的哈希输入,死节点自愈按最低负载),同时判定老节点
// 已死时会把同一个场景派给两个不同的节点。
// 以前无条件 SET 的结果是两边都以为自己成功:各建一份实体、各转一次 Agones 名额、
// scene_count 双减。现在只有 scene:{id}:node 仍等于 oldNode 时才写,后到的一方拿到 false。
//
// 返回 true 表示归属、反向索引与 scene_count 都已改到 newNode,调用方负责其余副作用
// (Agones 名额、CreateScene 等),而且只有它负责。
// 返回 false 表示**本次调用什么都没写**:屏障未到、归属已不是 oldNode(别的写者先改了),
// 或参数无效。调用方必须跳过 Agones 名额转移、CreateScene 与计数调整,否则会在没有
// 归属的节点上建实体。例外是 Redis 出错:脚本可能已经执行,结论未知,所以调用方
// 想回收自己预建的实体前,必须先重读归属(见 migrateWorldChannel)。2026-11-05 起 Redis 出错时先用同一个 token
// 问清结局(见 casReassignSceneNode):落地了返回 true;没落地就同时作废,以后也不会落地。false 里剩下的
// 「结论未知」只有问清结局也出错的情形。
func reassignSceneNode(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, oldNode, newNode string) bool {
	return casReassignSceneNode(svcCtx, zoneId, sceneId, oldNode, newNode, "")
}

// reassignWorldChannelNode 与 reassignSceneNode 相同,另在同一个脚本里要求 sceneId 仍是在役频道
// (channelSetKey = world_channels:zone:{z}:{conf} 的成员),不在就什么都不写、返回 false。
//
// 缩容排空(world_autoscale.go)先把频道从在役集合里 SREM,之后才读属主、再由 atomicTeardownScene 按读到的属主
// 原子删映射;孤儿清理(orphan_cleanup.go)同样用 atomicTeardownScene,在删映射的同一步把频道摘出集合。
// 调用方在改派前单独读一次成员资格的话,读与提交之间排空 / 拆除恰好发生,CAS 仍会落地:
//   - 改派落在拆除之前:拆除按读到的属主比对,落空、一个键不动,排空下一拍按新属主收尾(这一侧不依赖本核对);
//   - 改派落在拆除之后:映射已删,只比属主的 CAS(期望空串)会落地,把频道复活成一个哪个集合都不列的频道,
//     新节点上的实体、记账与 Agones 名额从此没有路径能收回 —— 这一侧只有本核对挡得住。
//
// 成员资格与改归属在同一个脚本里判定,CAS 落地就说明频道此刻仍在役。
func reassignWorldChannelNode(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, oldNode, newNode, channelSetKey string) bool {
	return casReassignSceneNode(svcCtx, zoneId, sceneId, oldNode, newNode, channelSetKey)
}

// luaReassignSceneNode 的返回值。
const (
	reassignOwnerChanged        = 0
	reassignApplied             = 1
	reassignChannelOutOfService = 2
)

// casReassignSceneNode 是 reassignSceneNode / reassignWorldChannelNode 的共同实现;channelSetKey 为空
// 表示不核对在役成员资格。
func casReassignSceneNode(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, oldNode, newNode, channelSetKey string) bool {
	if newNode == "" || newNode == oldNode {
		return false
	}
	if oldNode != "" && reentryBarrierBlocks(svcCtx, zoneId, oldNode, barrierSiteReassign) {
		return false
	}
	// 每次调用一个新 token、一把自己的回执键:go-redis 内部重发带的是同一个,脚本据此认出重发(见 luaReassignSceneNode)。
	receiptKey := sceneReassignReceiptKey(sceneId, uuid.NewString())
	keys := []string{
		sceneNodeKey(sceneId),
		nodeScenesKey(zoneId, oldNode),
		nodeScenesKey(zoneId, newNode),
		nodeSceneCountKey(zoneId, oldNode),
		nodeSceneCountKey(zoneId, newNode),
		receiptKey,
	}
	if channelSetKey != "" {
		keys = append(keys, channelSetKey)
	}
	res, err := svcCtx.Redis.Eval(luaReassignSceneNode, keys, oldNode, newNode, strconv.FormatUint(sceneId, 10),
		sceneClaimRecordTTLSeconds)
	if err != nil {
		// go-redis 的重发也都没拿到应答,脚本可能已经执行。用同一个 token 问清结局:落地了就按已改派返回,调用方照常
		// 做后续(不然 rebalance 会把带着玩家的老实体留在老节点上);没落地的话问的同时作废了它,以后也不会落地,
		// 按落空返回。问也出错才是结论未知,按落空返回(调用方怎么处理见 reassignSceneNode)。
		verdict, resolveErr := resolveCasReceipt(svcCtx, receiptKey)
		switch {
		case resolveErr != nil:
			logx.Errorf("[reassign] zone=%d scene=%d: CAS %q -> %q failed and its outcome could not be resolved, outcome unknown: %v / %v",
				zoneId, sceneId, oldNode, newNode, err, resolveErr)
		case verdict == casReceiptApplied:
			logx.Infof("[reassign] zone=%d scene=%d: CAS %q -> %q lost its reply; resolved as applied: %v",
				zoneId, sceneId, oldNode, newNode, err)
			return true
		default:
			// 用 Errorf:脚本自己报错(例如计数键不是整数,INCR 失败)也走到这里,不能降成 Info 埋掉。
			logx.Errorf("[reassign] zone=%d scene=%d: CAS %q -> %q failed; resolved as not applied (now fenced off): %v",
				zoneId, sceneId, oldNode, newNode, err)
		}
		return false
	}
	switch toInt64(res) {
	case reassignApplied:
		return true
	case reassignChannelOutOfService:
		logx.Infof("[reassign] zone=%d scene=%d: channel is no longer in service (%s); move %q -> %q not applied",
			zoneId, sceneId, channelSetKey, oldNode, newNode)
	default:
		logx.Infof("[reassign] zone=%d scene=%d: owner is no longer %q (another writer reassigned it first); move to %q not applied",
			zoneId, sceneId, oldNode, newNode)
	}
	return false
}

// GetAllWorldChannels returns all channel sceneIds for a confId in a zone.
func GetAllWorldChannels(ctx context.Context, svcCtx *svc.ServiceContext, confId uint64, zoneId uint32) ([]uint64, error) {
	channelSetKey := worldChannelsKey(zoneId, confId)
	members, err := svcCtx.Redis.Smembers(channelSetKey)
	if err != nil {
		return nil, err
	}
	ids := make([]uint64, 0, len(members))
	for _, m := range members {
		id, _ := strconv.ParseUint(m, 10, 64)
		if id > 0 {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// worldConfIds extracts base scene config IDs from the loaded World table.
// worldConfIdsOverride, when non-nil, replaces the World-table lookup.
// Same test-seam pattern as nodeDialer / nodeEndpointOverride: the World
// table is empty in unit tests, so anything that iterates world maps
// (autoscaler, orphan cleanup) would be a no-op without this.
// Production never sets it.
var worldConfIdsOverride []uint64

// SetWorldConfIdsForTest installs a fake World-table id list and returns a
// restore func. MUST be paired with t.Cleanup.
func SetWorldConfIdsForTest(ids []uint64) (restore func()) {
	prev := worldConfIdsOverride
	worldConfIdsOverride = ids
	return func() { worldConfIdsOverride = prev }
}

func worldConfIds() []uint64 {
	if worldConfIdsOverride != nil {
		return worldConfIdsOverride
	}
	rows := table.WorldTableManagerInstance.FindAll()
	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, uint64(row.SceneId))
	}
	return ids
}

// IsWorldConf checks if a scene_conf_id is a world scene.
func IsWorldConf(confId uint64) bool {
	for _, row := range table.WorldTableManagerInstance.FindAll() {
		if uint64(row.SceneId) == confId {
			return true
		}
	}
	return false
}

// assignNodeByHash uses FNV-1a to consistently map a key to a node.
// Deterministic: same key + same node list -> same node.
func assignNodeByHash(key uint64, sortedNodes []string) string {
	h := fnv.New32a()
	h.Write([]byte(fmt.Sprintf("%d", key)))
	idx := int(h.Sum32()) % len(sortedNodes)
	return sortedNodes[idx]
}

// isNodeUnreachableError returns true if the error indicates the node process is
// not reachable (connection refused, unavailable). This identifies zombie nodes
// whose etcd lease hasn't expired yet but whose process is dead.
func isNodeUnreachableError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "Unavailable") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "connectex") ||
		strings.Contains(s, "not found in etcd") ||
		// 锁内 CreateScene 加了 5s 时限后,黑洞节点(进程死、租约未过期)
		// 的失败形态从 Unavailable 变成超时 —— 同样按"不可达"处理,
		// 否则僵尸节点每轮都要白等一次超时。
		strings.Contains(s, "DeadlineExceeded") ||
		strings.Contains(s, "context deadline exceeded")
}

// markNodeUnreachable 把节点记入本轮的不可达集合,并从本轮的候选列表里拿掉,
// 让后续场景不再对它白等一次 RPC 超时;同时丢掉它的 gRPC 连接缓存(端点可能已陈旧,
// 下次用到时重新解析重拨,代价可忽略)。
//
// **刻意不碰 Redis 负载集。** IsNodeAlive 把「不在负载集」当作断言节点已死的唯一证据,
// 而再入屏障对没有 death_at 的节点一律放行;旧实现(markNodeDead)在一次 5s 超时后就
// ZREM,于是一个只是慢了的活节点会在负载上报把它加回来之前(最长一个 LoadReportInterval)
// 被 rebalance / 孤儿清理当成死节点,名下场景被改派或销毁。节点真死时,负载集由 etcd
// 注销路径(removeNodeFromRedis / 周期巡检)负责摘除,那条路径会先写 death_at;写不进时
// 推迟摘除(GO-6),本文件的改派同时要求节点已摘出负载集。
func markNodeUnreachable(zoneId uint32, nodeId string, unreachableNodes map[string]struct{}, liveNodes *[]string) {
	if _, already := unreachableNodes[nodeId]; already {
		return
	}
	unreachableNodes[nodeId] = struct{}{}

	// 从本轮候选列表里拿掉:后面给新频道挑节点时不再选它。
	filtered := (*liveNodes)[:0]
	for _, n := range *liveNodes {
		if n != nodeId {
			filtered = append(filtered, n)
		}
	}
	*liveNodes = filtered

	RemoveNodeConn(zoneId, nodeId)

	logx.Infof("[World] Node %s unreachable this round (zone %d); load set and scene ownership left untouched", nodeId, zoneId)
}
