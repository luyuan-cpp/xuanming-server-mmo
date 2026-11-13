package logic

import (
	"context"
	"strconv"
	"time"

	"scene_manager/internal/agones"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
)

// Agones rooms 名额的对账(docs/design/agones-scene-node-high-density.md §13.5)。
//
// 名额是 GameServer 上 rooms 列表里按身份的条目,Redis 里记着每个场景占的是哪一条、哪些条目还在途
// (agones_room_ledger.go)。两边都是按条目的,对账就能逐条判谁对谁错,两个方向都改得动:
//   - 条目 → 记录:列表里没有记录、拆除任务、在途占用指向的条目是孤儿,删掉。来历:占了名额之后进程崩溃、
//     放弃名额时删条目失败、改派转移之后删旧条目失败、登记结局未知而其实没落地。
//   - 记录 → 条目:节点上的场景,记录指向这台却不在列表里的条目补加;记录在别的 GameServer 上(改派转移时新
//     GameServer 满了)的转过来。
//
// 只碰本 zone 的 GameServer 与本 zone 的场景键(control-plane-zone-scoping-20261105.md §4.3)。只有本 zone 的领导者做。
// 列不出 GameServer 时整轮跳过,不改任何一方;单个条目、场景读写失败只跳过它,计入本轮未收敛数。

// agonesRoomsReadBatch 是「记录 → 条目」一次 MGET 读的场景数(每个场景两把键)。
const agonesRoomsReadBatch = 200

// agonesRoomHoldSweepBatch 是一轮最多处理的过期在途占用数,剩下的下一轮做。
const agonesRoomHoldSweepBatch = 500

// StartAgonesReconcile 周期性对账 Agones rooms 列表与 Redis 名额记录,见文件头。
// ReconcileIntervalSeconds<=0 或未启用 Agones 时直接返回,不起 goroutine。
func StartAgonesReconcile(ctx context.Context, svcCtx *svc.ServiceContext) {
	interval := svcCtx.Config.Agones.ReconcileIntervalSeconds
	if interval <= 0 {
		logx.Info("[AgonesReconcile] disabled (ReconcileIntervalSeconds <= 0)")
		return
	}
	if !AgonesEnabled(svcCtx) {
		logx.Info("[AgonesReconcile] disabled (Agones not enabled)")
		return
	}

	logx.Infof("[AgonesReconcile] started, interval=%ds", interval)

	// safego.Loop:单轮 panic 只丢那一轮(对账要读 K8s API,反序列化出意外
	// 结构是现实风险),循环继续;点位名进 safego_panic_total。
	safego.Loop(ctx, SafePointAgonesReconcile, time.Duration(interval)*time.Second,
		func(ctx context.Context) {
			if !isLeader() {
				// 降级的副本把自己发过的 zone 写回 0,告警取 max 时不会被旧值拖住。
				for _, zoneID := range orchestratedZones(svcCtx) {
					metrics.SetAgonesCounterDrift(agonesZoneLabel(zoneID), 0)
				}
				return
			}
			for _, zoneID := range orchestratedZones(svcCtx) {
				ReconcileAgonesRoomsForZone(ctx, svcCtx, zoneID)
			}
		})
}

// ReconcileAgonesRoomsForZone 跑一轮对账,回本轮结束时仍没收敛的条目与场景数,并写进
// scene_manager_agones_counter_drift。导出是为了让单测能直接驱动一轮,不用等 ticker。
func ReconcileAgonesRoomsForZone(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32) int {
	allocator := GetAgonesAllocator()
	if allocator == nil {
		return 0
	}
	zoneLabel := agonesZoneLabel(zoneID)
	if !svcCtx.Config.Agones.HighDensityEnabled {
		// 非高密度模式不记名额,没有可对的账。
		metrics.SetAgonesCounterDrift(zoneLabel, 0)
		return 0
	}

	round := agonesRoomsRound{
		ctx:       ctx,
		svcCtx:    svcCtx,
		allocator: allocator,
		zoneID:    zoneID,
		zoneLabel: zoneLabel,
		namespace: agonesNamespace(svcCtx),
		now:       time.Now(),
	}
	servers, err := allocator.ListGameServerRooms(ctx, round.namespace, zoneLabel)
	if err != nil {
		logx.Errorf("[AgonesReconcile] zone %d: list gameservers failed, skipping this round: %v", zoneID, err)
		return 0
	}

	listed := make(map[string]struct{})
	for _, gs := range servers {
		for _, entry := range gs.Entries {
			listed[entry] = struct{}{}
		}
	}

	unconverged := 0
	for _, gs := range servers {
		unconverged += round.reclaimOrphanEntries(gs)
	}
	unconverged += round.cancelExpiredHolds(listed)
	for _, gs := range servers {
		unconverged += round.restoreRecordedRooms(gs)
	}

	metrics.SetAgonesCounterDrift(zoneLabel, unconverged)
	if unconverged > 0 {
		logx.Errorf("[AgonesReconcile] zone %d: %d room entries / scenes still disagree after this round", zoneID, unconverged)
	}
	return unconverged
}

// agonesRoomsRound 是一轮对账的上下文。now 在一轮里固定:判在途占用是否过期都按同一时刻。
type agonesRoomsRound struct {
	ctx       context.Context
	svcCtx    *svc.ServiceContext
	allocator agones.Allocator
	zoneID    uint32
	zoneLabel string
	namespace string
	now       time.Time
}

// reclaimOrphanEntries 是「条目 → 记录」:逐条判 GameServer 上的条目(luaJudgeAgonesRoomEntry),删掉孤儿。
// 回这台没收敛的条目数。
func (r agonesRoomsRound) reclaimOrphanEntries(gs agones.GameServerRooms) int {
	unconverged := 0
	for _, entry := range gs.Entries {
		if r.ctx.Err() != nil {
			return unconverged + 1
		}
		sceneID, ok := agonesRoomEntrySceneID(entry)
		if !ok {
			// 不是本服务写的格式(人工改过 GameServer,或别的程序在用这个列表):不猜,留着,让告警把人叫来。
			logx.Errorf("[AgonesReconcile] zone %d gs=%s: unrecognized room entry %q, leaving it", r.zoneID, gs.Name, entry)
			unconverged++
			continue
		}
		room := agonesRoom{GameServer: gs.Name, Entry: entry}
		verdict, err := judgeAgonesRoomEntry(r.svcCtx, r.zoneID, sceneID, room, r.now)
		if err != nil {
			logx.Errorf("[AgonesReconcile] zone %d gs=%s: judging room entry %s failed, leaving it this round: %v",
				r.zoneID, gs.Name, entry, err)
			unconverged++
			continue
		}
		switch {
		case verdict.reclaimable():
			if err := releaseAgonesRoom(r.ctx, r.svcCtx, r.namespace, room, "reconcile_"+string(verdict)); err != nil {
				unconverged++
				continue
			}
			action := metrics.AgonesRoomsOrphanRemoved
			if verdict == agonesRoomHoldExpired {
				action = metrics.AgonesRoomsHoldExpiredRemoved
			}
			metrics.ObserveAgonesRoomsReconciled(r.zoneLabel, action)
			logx.Infof("[AgonesReconcile] zone %d gs=%s: reclaimed room entry %s of scene %d (%s)",
				r.zoneID, gs.Name, entry, sceneID, verdict)
		}
	}
	return unconverged
}

// cancelExpiredHolds 取消过期、条目又不在任何一台 GameServer 上的在途占用:分配根本没发出去,或者条目已经删了、
// 在途占用没删掉。条目在列表里的,上一步已经按条目判过(在途占用过期的在那里同一步取消)。
// 列表快照之后才加上的条目,在途占用照样在这里取消:之后它的提交被挡住,条目下一轮按孤儿删。
func (r agonesRoomsRound) cancelExpiredHolds(listed map[string]struct{}) int {
	holdsKey := agonesRoomHoldsKey(r.zoneID)
	due, err := r.svcCtx.Redis.ZrangebyscoreWithScoresAndLimit(holdsKey, 0, r.now.Unix(), 0, agonesRoomHoldSweepBatch)
	if err != nil {
		logx.Errorf("[AgonesReconcile] zone %d: reading expired room holds failed: %v", r.zoneID, err)
		return 1
	}
	unconverged := 0
	for _, hold := range due {
		if _, onGameServer := listed[hold.Key]; onGameServer {
			continue
		}
		canceled, err := cancelExpiredAgonesRoomHold(r.svcCtx, r.zoneID, hold.Key, r.now)
		if err != nil {
			logx.Errorf("[AgonesReconcile] zone %d: canceling expired room hold %s failed: %v", r.zoneID, hold.Key, err)
			unconverged++
			continue
		}
		if canceled {
			metrics.ObserveAgonesRoomsReconciled(r.zoneLabel, metrics.AgonesRoomsHoldCanceled)
			logx.Infof("[AgonesReconcile] zone %d: canceled expired room hold %s (no gameserver holds it)", r.zoneID, hold.Key)
		}
	}
	return unconverged
}

// restoreRecordedRooms 是「记录 → 条目」:这台 GameServer 对应的节点上、属主仍是这个节点的场景,记录指向这台却不在
// 列表里的条目补加;记录在别的 GameServer 上(或没有记录)的,按改派转移的做法转过来。回这台没收敛的场景数。
//
// 补加不写在途占用:记录已经指向这一条,判条目时就是「在用」。补加之后场景恰好被销毁或改派,补上的这条会成为孤儿,
// 下一轮删掉;只会短暂多记,不会少记。
func (r agonesRoomsRound) restoreRecordedRooms(gs agones.GameServerRooms) int {
	if gs.PodIP == "" {
		// 没有 Pod 地址对不上节点。它的条目已在上一步按条目判过,这里只是少做这一个方向。
		return 0
	}
	nodeID, entryZone, _, ok := FindNodeByPodIP(gs.PodIP)
	if !ok || entryZone != r.zoneID {
		return 0
	}
	sceneIDs, err := r.svcCtx.Redis.Smembers(nodeScenesKey(r.zoneID, nodeID))
	if err != nil {
		logx.Errorf("[AgonesReconcile] zone %d gs=%s node=%s: reading the node's scenes failed: %v", r.zoneID, gs.Name, nodeID, err)
		return 1
	}
	present := make(map[string]struct{}, len(gs.Entries))
	for _, entry := range gs.Entries {
		present[entry] = struct{}{}
	}

	unconverged := 0
	for start := 0; start < len(sceneIDs); start += agonesRoomsReadBatch {
		if r.ctx.Err() != nil {
			return unconverged + 1
		}
		batch := sceneIDs[start:min(start+agonesRoomsReadBatch, len(sceneIDs))]
		keys := make([]string, 0, 2*len(batch))
		ids := make([]uint64, 0, len(batch))
		for _, member := range batch {
			sceneID, err := strconv.ParseUint(member, 10, 64)
			if err != nil || sceneID == 0 {
				continue
			}
			ids = append(ids, sceneID)
			keys = append(keys, sceneNodeKey(r.zoneID, sceneID), sceneAgonesGsKey(r.zoneID, sceneID))
		}
		if len(ids) == 0 {
			continue
		}
		values, err := r.svcCtx.Redis.Mget(keys...)
		if err != nil || len(values) != len(keys) {
			logx.Errorf("[AgonesReconcile] zone %d node=%s: reading %d scenes' owners and rooms failed: %v", r.zoneID, nodeID, len(ids), err)
			unconverged += len(ids)
			continue
		}
		for i, sceneID := range ids {
			owner, record := values[2*i], values[2*i+1]
			if owner != nodeID {
				// 场景已不在这个节点上(集合里的旧成员,或刚被改派、销毁):归新属主那一台对。
				continue
			}
			unconverged += r.restoreSceneRoom(gs, nodeID, sceneID, parseAgonesRoom(record), present)
		}
	}
	return unconverged
}

// restoreSceneRoom 对一个属主是 nodeID 的场景做「记录 → 条目」,回 0 或 1(没收敛)。
func (r agonesRoomsRound) restoreSceneRoom(gs agones.GameServerRooms, nodeID string, sceneID uint64, room agonesRoom,
	present map[string]struct{}) int {

	if room.GameServer == gs.Name && room.Entry != "" {
		if _, ok := present[room.Entry]; ok {
			return 0
		}
		if err := r.allocator.AddRoomEntry(r.ctx, r.namespace, gs.Name, room.Entry); err != nil {
			if isNoCapacity(err) {
				metrics.ObserveAgonesRoomsReconciled(r.zoneLabel, metrics.AgonesRoomsMissingNoCapacity)
			}
			logx.Errorf("[AgonesReconcile] zone %d gs=%s: scene %d's recorded room entry %s is missing and could not be added back: %v",
				r.zoneID, gs.Name, sceneID, room.Entry, err)
			return 1
		}
		metrics.ObserveAgonesRoomsReconciled(r.zoneLabel, metrics.AgonesRoomsMissingAdded)
		logx.Infof("[AgonesReconcile] zone %d gs=%s: added back scene %d's missing room entry %s", r.zoneID, gs.Name, sceneID, room.Entry)
		return 0
	}

	// 场景在这个节点上,名额却记在别的 GameServer 上(改派转移时这台满了),或者根本没记:转过来。
	// 不经 AcquireAgonesRoomOnNode,直接用已知的这台 GameServer,省得每个场景 LIST 一次。
	placement, err := acquireAgonesRoomOnGameServer(r.ctx, r.svcCtx, r.zoneID, sceneID, nodeID, gs.Name)
	if err != nil {
		if failedAgonesTransfer(sceneID, nodeID, err) == agonesTransferNoCapacity {
			metrics.ObserveAgonesRoomsReconciled(r.zoneLabel, metrics.AgonesRoomsRehomeNoCapacity)
		}
		return 1
	}
	switch commitAgonesRoomTransfer(r.ctx, r.svcCtx, sceneID, nodeID, r.zoneID, placement) {
	case agonesTransferMoved:
		metrics.ObserveAgonesRoomsReconciled(r.zoneLabel, metrics.AgonesRoomsRehomed)
		logx.Infof("[AgonesReconcile] zone %d: moved scene %d's room %q onto gs=%s (node %s)",
			r.zoneID, sceneID, room.record(), gs.Name, nodeID)
		return 0
	case agonesTransferNotNeeded:
		// 读完之后别的写者已经转过来,或场景已被改派、销毁:没什么可做的。
		return 0
	default:
		return 1
	}
}
