package logic

import (
	"context"
	"strconv"
	"time"

	"proto/scene_manager"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/playerredis"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 成员集合对账(docs/design/redis-player-store-cluster.md §5.10)。
//
// scene_manager 在预占之后、落点之前崩溃,或落点之后、摘旧场景之前崩溃,会留下一个 location 不指向这里的成员。请求路径上
// 没有人会再摘它,实例永远不销毁、选线一直躲着它。这里周期性地逐个核 location,把这种成员摘掉。
//
// 只有领导者跑。每轮有两个预算:读 location 的成员数(MembersPerRound)与 SSCAN 的条数(ScansPerRound),用完就停,下一轮接着
// 上一轮的位置走。遍历位置只在本进程内存里,换领导者从头开始,只是多走一遍。
//
// 集合的来源:登记过的 zone ∪ 有活节点的 zone → 负载集里的节点 → 节点的成员集合,以及它名下场景的成员集合。全部是点查,不 SCAN
// 键空间(理由同 orphan_cleanup.go)。死节点的集合不归这里管,它们由死节点收尾删掉。
//
// 在途预占(还没落点)的 location 同样不指向这里,不能一看到就摘:它是实例里唯一的成员时,摘掉会让空闲回收把实例销毁,玩家落到
// 一个已经没了的场景。所以成员要连续 sceneMemberStaleGrace 不指向这里才摘,这比任何一次进场请求都长。记录只在本进程内存里,
// 换领导者重新计时。
//
// 摘除走 removeSceneMember,即 §5.10 第 3 条:SREM 之后再读一次 location,这时指向这里就加回去。和在途请求的交错都是安全的:
//   - 超过宽限才落点的预占被摘掉:落点提交之后会再 SADD 一次(第 2 条)。这段时间人数少算一个,并发选线可能多放一个人进来,无害;
//   - 第一次读 location 之后、SREM 之前玩家落到了这里:第二次读会把成员加回去。

const (
	// sceneMemberScanCount 是 SSCAN 的 COUNT 提示。小集合(intset / listpack 编码)会一次回完,不受它约束,所以一批的大小
	// 上界是 set-max-listpack-entries 与它的较大者。
	sceneMemberScanCount = 64
	// sceneMemberLocationBatch 是一次 MGET 读 location 的成员数上限。按 slot 拆组后是一条 pipeline,太大会占住连接。
	sceneMemberLocationBatch = 64
	// sceneMemberStaleGrace 是成员连续不指向这里多久才算残留,要长过一次进场请求从预占到落点的全部耗时。
	sceneMemberStaleGrace = 2 * time.Minute
	// sceneMemberSuspectLimit 是嫌疑记录的上限,记不下的成员这一遍不动,只是晚一点摘。
	sceneMemberSuspectLimit = 1 << 16
)

// sceneMemberSet 是一个要核对的成员集合。场景集合 sceneID 非 0、nodeID 为空,节点集合反过来。
type sceneMemberSet struct {
	key     string
	sceneID uint64
	zoneID  uint32
	nodeID  string
}

// sceneMemberSuspect 是一个 location 不指向这里的成员。同一个玩家在场景集合和节点集合里各算各的。
type sceneMemberSuspect struct {
	key    string
	player uint64
}

// sceneMemberSuspectRecord 是嫌疑成员的记录。
type sceneMemberSuspectRecord struct {
	firstSeen time.Time
	// lastPass 是最后一次看到它的那一遍,一遍结束时用来丢掉已经不在集合里的记录。
	lastPass uint64
}

// sceneMemberReconciler 是遍历位置和嫌疑记录。只由一个后台循环使用,不需要锁。
type sceneMemberReconciler struct {
	// pending 是这一遍还没走完的集合,pending[0] 是正在走的那个。
	pending []sceneMemberSet
	// cursor / scanned:pending[0] 的 SSCAN 游标,scanned 区分「还没开始」与「游标回到 0、扫完了」。
	cursor  uint64
	scanned bool
	// batch 是 pending[0] 扫出来、还没核对的成员。SSCAN 一批可能超过预算,余下的留到下一轮;丢掉的话小集合永远走不到后面的成员。
	batch []string

	// grace 为 0 时不读时钟、看到就摘,只有单测这么用。
	grace time.Duration
	now   func() time.Time
	// suspects 是看到过、还没到宽限的成员。pass 是当前这一遍的编号。
	suspects map[sceneMemberSuspect]sceneMemberSuspectRecord
	pass     uint64
}

// StartSceneMemberReconciler 起对账循环。立即返回。
func StartSceneMemberReconciler(ctx context.Context, svcCtx *svc.ServiceContext) {
	cfg := svcCtx.Config.SceneMemberReconcile
	if cfg.Disabled {
		logx.Info("[SceneMemberReconcile] Not started: SceneMemberReconcile.Disabled=true; members left behind by a crash stay until the instance is force-destroyed")
		return
	}
	interval := cfg.EffectiveInterval()
	members, scans := cfg.EffectiveMembersPerRound(), cfg.EffectiveScansPerRound()
	logx.Infof("[SceneMemberReconcile] Started: interval=%v members_per_round=%d scans_per_round=%d stale_grace=%v", interval, members, scans, sceneMemberStaleGrace)
	reconciler := &sceneMemberReconciler{grace: sceneMemberStaleGrace, now: time.Now}
	safego.Loop(ctx, SafePointSceneMemberReconcile, interval, func(ctx context.Context) {
		reconciler.runRound(ctx, svcCtx, members, scans)
	})
}

// reset 丢掉遍历位置和嫌疑记录,下一轮从头开始。
func (r *sceneMemberReconciler) reset() {
	*r = sceneMemberReconciler{grace: r.grace, now: r.now}
}

// dropHead 结束 pending[0]。
func (r *sceneMemberReconciler) dropHead() {
	r.pending = r.pending[1:]
	r.cursor, r.scanned, r.batch = 0, false, nil
}

// finishPass 结束一遍:这一遍没再看到的记录作废(成员已被别的路径摘掉),否则记录只增不减。
func (r *sceneMemberReconciler) finishPass() {
	for suspect, record := range r.suspects {
		if record.lastPass != r.pass {
			delete(r.suspects, suspect)
		}
	}
	r.pass++
	metrics.ObserveSceneMemberReconcilePass()
}

// graceElapsed 记下这次看到的嫌疑成员,报告它是否已经连续不指向这里超过宽限。
func (r *sceneMemberReconciler) graceElapsed(suspect sceneMemberSuspect) bool {
	if r.grace <= 0 {
		return true
	}
	now := r.now()
	record, seen := r.suspects[suspect]
	if !seen {
		if len(r.suspects) >= sceneMemberSuspectLimit {
			return false
		}
		if r.suspects == nil {
			r.suspects = make(map[sceneMemberSuspect]sceneMemberSuspectRecord)
		}
		record.firstSeen = now
	}
	record.lastPass = r.pass
	r.suspects[suspect] = record
	return now.Sub(record.firstSeen) >= r.grace
}

// runRound 走一轮:预算用完、或这一轮已经走完一遍就返回。
func (r *sceneMemberReconciler) runRound(ctx context.Context, svcCtx *svc.ServiceContext, memberBudget, scanBudget int) {
	listed := false
	for memberBudget > 0 {
		if ctx.Err() != nil {
			return
		}
		// 降级即停:位置不留给以后,再当选时集合可能已经变了很多。
		if !isLeader() {
			r.reset()
			return
		}
		if len(r.batch) > 0 {
			n := min(len(r.batch), memberBudget, sceneMemberLocationBatch)
			r.checkMembers(ctx, svcCtx, r.pending[0], r.batch[:n])
			r.batch = r.batch[n:]
			memberBudget -= n
			continue
		}
		if len(r.pending) > 0 && r.scanned && r.cursor == 0 {
			r.dropHead()
			if len(r.pending) == 0 {
				r.finishPass()
			}
		}
		if len(r.pending) == 0 {
			// 一轮最多列一次集合:列集合本身也是一串 Redis 读,集合很少时不要在一轮里空转。
			if listed {
				return
			}
			listed = true
			r.pending = listSceneMemberSets(svcCtx)
			r.cursor, r.scanned = 0, false
			if len(r.pending) == 0 {
				return
			}
		}
		if scanBudget <= 0 {
			return
		}
		scanBudget--
		set := r.pending[0]
		members, next, err := svcCtx.Redis.SscanCtx(ctx, set.key, r.cursor, "", sceneMemberScanCount)
		if err != nil {
			// 不卡在这个集合上,下一遍再来。
			logx.Errorf("[SceneMemberReconcile] sscan(%s) failed, skipping this set until the next pass: %v", set.key, err)
			metrics.ObserveSceneMemberReconcile(metrics.SceneMemberReconcileError, 1)
			r.dropHead()
			continue
		}
		r.cursor, r.scanned, r.batch = next, true, members
	}
}

// listSceneMemberSets 列出要核对的集合。读失败的 zone 或节点这一遍跳过,打 Error。
func listSceneMemberSets(svcCtx *svc.ServiceContext) []sceneMemberSet {
	registered, err := listRegisteredZones(svcCtx)
	if err != nil {
		logx.Errorf("[SceneMemberReconcile] read zone registry failed; checking zones with live nodes only: %v", err)
	}
	sets := make([]sceneMemberSet, 0)
	for _, zone := range unionZones(registered, GetActiveZones()) {
		nodes, err := svcCtx.Redis.ZrangeWithScores(nodeLoadKey(zone), 0, -1)
		if err != nil {
			logx.Errorf("[SceneMemberReconcile] read load set of zone %d failed; skipping it this pass: %v", zone, err)
			continue
		}
		for _, node := range nodes {
			sets = append(sets, sceneMemberSet{key: nodeMembersKey(zone, node.Key), zoneID: zone, nodeID: node.Key})
			scenes, err := svcCtx.Redis.Smembers(nodeScenesKey(zone, node.Key))
			if err != nil {
				logx.Errorf("[SceneMemberReconcile] read scenes of zone %d node %s failed; skipping them this pass: %v", zone, node.Key, err)
				continue
			}
			for _, member := range scenes {
				sceneID, err := strconv.ParseUint(member, 10, 64)
				if err != nil || sceneID == 0 {
					continue
				}
				sets = append(sets, sceneMemberSet{key: instanceMembersKey(sceneID), sceneID: sceneID, zoneID: zone})
			}
		}
	}
	return sets
}

// checkMembers 读这批成员的 location,摘掉连续超过宽限不指向这里的成员。
func (r *sceneMemberReconciler) checkMembers(ctx context.Context, svcCtx *svc.ServiceContext, set sceneMemberSet, members []string) {
	players := make([]uint64, 0, len(members))
	keys := make([]string, 0, len(members))
	for _, member := range members {
		player, err := strconv.ParseUint(member, 10, 64)
		if err != nil || player == 0 {
			// 不是合法的 player_id:没有 location 可核,留着它实例就永远不销毁。
			removeMalformedSceneMember(svcCtx, set, member)
			continue
		}
		players = append(players, player)
		keys = append(keys, getPlayerLocationKey(player))
	}
	if len(keys) == 0 {
		return
	}
	values, err := playerredis.MGetBySlot(ctx, svcCtx.PlayerRedis, keys)
	if err != nil {
		// 读不到 location 不能判它是残留:这批留到下一遍。
		logx.Errorf("[SceneMemberReconcile] read %d player locations for %s failed: %v", len(keys), set.key, err)
		metrics.ObserveSceneMemberReconcile(metrics.SceneMemberReconcileError, len(keys))
		return
	}
	for i, value := range values {
		raw, _ := value.(string)
		loc, parseErr := parseSceneMemberLocation(raw)
		if parseErr != nil {
			// location 写坏是另一个事故,这里不替它做决定。removeSceneMember 读到坏值同样会报错。
			logx.Errorf("[SceneMemberReconcile] location of player %d is corrupt, keeping member of %s: %v", players[i], set.key, parseErr)
			metrics.ObserveSceneMemberReconcile(metrics.SceneMemberReconcileSkipped, 1)
			continue
		}
		suspect := sceneMemberSuspect{key: set.key, player: players[i]}
		if sceneMemberPointsHere(loc, set) {
			delete(r.suspects, suspect)
			metrics.ObserveSceneMemberReconcile(metrics.SceneMemberReconcileOK, 1)
			continue
		}
		if !r.graceElapsed(suspect) {
			metrics.ObserveSceneMemberReconcile(metrics.SceneMemberReconcileSkipped, 1)
			continue
		}
		if err := removeSceneMember(svcCtx, players[i], set.sceneID, set.zoneID, set.nodeID); err != nil {
			logx.Errorf("[SceneMemberReconcile] remove player %d from %s failed: %v", players[i], set.key, err)
			metrics.ObserveSceneMemberReconcile(metrics.SceneMemberReconcileError, 1)
			continue
		}
		delete(r.suspects, suspect)
		logx.Infof("[SceneMemberReconcile] removed stale member: player=%d set=%s", players[i], set.key)
		metrics.ObserveSceneMemberReconcile(metrics.SceneMemberReconcileRemoved, 1)
	}
}

func removeMalformedSceneMember(svcCtx *svc.ServiceContext, set sceneMemberSet, member string) {
	if _, err := svcCtx.Redis.Srem(set.key, member); err != nil {
		logx.Errorf("[SceneMemberReconcile] remove malformed member %q from %s failed: %v", member, set.key, err)
		metrics.ObserveSceneMemberReconcile(metrics.SceneMemberReconcileError, 1)
		return
	}
	logx.Errorf("[SceneMemberReconcile] removed malformed member %q from %s", member, set.key)
	metrics.ObserveSceneMemberReconcile(metrics.SceneMemberReconcileMalformed, 1)
}

// parseSceneMemberLocation 解 location 原文。键不存在(空串)返回 (nil, nil)。
func parseSceneMemberLocation(raw string) (*scene_manager.PlayerLocation, error) {
	if raw == "" {
		return nil, nil
	}
	loc := &scene_manager.PlayerLocation{}
	if err := proto.Unmarshal([]byte(raw), loc); err != nil {
		return nil, err
	}
	return loc, nil
}

// sceneMemberPointsHere 报告 location 是否指向这个集合。判据与 removeSceneMember 同一个(locationOverlap)。
func sceneMemberPointsHere(loc *scene_manager.PlayerLocation, set sceneMemberSet) bool {
	scene, node := locationOverlap(loc, set.sceneID, set.zoneID, set.nodeID)
	return scene != 0 || node != ""
}
