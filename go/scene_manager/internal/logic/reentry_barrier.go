package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"scene_manager/internal/constants"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---------------------------------------------------------------------------
// 再入屏障(scene owner re-entry barrier)—— Go 侧落点
//
// 不变量:**老节点最晚可能写入的时刻 < 新节点最早被允许接管的时刻**。
// 推导、跨语言依赖与常数见 internal/constants/reentry_barrier.go 与
// docs/design/scene-owner-reentry-barrier.md §3.2。
//
// 本文件提供三样东西:
//  1. 死亡时刻打点(markNodeDeath / clearNodeDeath)——只在 etcd 事件处调用;
//  2. 判定入口(reentryBarrierBlocks)——**所有**会改派 / 销毁 / 清理场景归属
//     的分支在动手前都要过它;
//  3. 被推迟的死节点收尾队列(死节点孤儿场景强制销毁 + 计数清理)。
//
// 判定的默认方向是 fail-closed:拿不准就**不动** scene ownership。少改一次派
// 只是本轮不自愈(下一轮还会再来),改错一次是玩家数据回档。
// ---------------------------------------------------------------------------

// NodeDeathAtKeyFmt 是节点死亡时刻标记的 Redis 键格式,值是 Unix 毫秒。
// 与 NodeSceneCountKey / NodePlayerCountKey 同一命名族,便于按节点前缀清理。
const NodeDeathAtKeyFmt = "node:zone:%d:%s:death_at"

// 再入屏障判定点位名。**必须是常量**:它们会直接变成 Prometheus label,
// 拼进运行期值会把指标基数打爆(仓库 CLAUDE.md §9)。
const (
	barrierSiteResolveScene     = "resolve_scene"
	barrierSiteRebalance        = "rebalance"
	barrierSiteReassign         = "reassign"
	barrierSiteWorldChannelLazy = "world_channel_lazy"
	barrierSiteOrphanCleanup    = "orphan_cleanup"
	barrierSiteDeadNodeCleanup  = "dead_node_cleanup"
)

// ErrReentryBarrierPending 表示老属主节点刚判死,但再入屏障还没走完。
//
// 它是**可重试**的瞬时拒绝,不是故障:上游带着同样的参数退避重试,屏障过后
// 自然成功。调用方用 errors.Is 识别它,翻成 constants.ErrSceneReentryBarrier。
var ErrReentryBarrierPending = errors.New("scene owner reentry barrier not elapsed")

func nodeDeathAtKey(zoneID uint32, nodeID string) string {
	return fmt.Sprintf(NodeDeathAtKeyFmt, zoneID, nodeID)
}

// sceneReentryBarrier 返回本进程生效的屏障时长(配置可调高,不可调低)。
// 配置非法时 ResolveSceneReentryBarrier 已经把值钳回安全下限,这里不再重复
// 打日志 —— 启动时 main 已经把那条 error 记过一次了。
func sceneReentryBarrier(svcCtx *svc.ServiceContext) time.Duration {
	d, _ := constants.ResolveSceneReentryBarrier(svcCtx.Config.SceneReentryBarrierSeconds)
	return d
}

// markNodeDeath 记录「本进程观察到该节点从 etcd 消失」的时刻。
//
// 用覆盖写(SET)而不是 SETNX:更晚的观察时刻意味着老节点可能停笔得更晚,
// 取**最新**观察值才满足不变量。重复观察把屏障往后推的代价只是改派再多等一个
// 屏障时长,可自愈;而保留一个过早的旧时刻会让屏障提前失效,那是数据损坏。
func markNodeDeath(svcCtx *svc.ServiceContext, zoneID uint32, nodeID string) {
	if nodeID == "" {
		return
	}
	key := nodeDeathAtKey(zoneID, nodeID)
	ttl := int(constants.NodeDeathMarkTTL / time.Second)
	if err := svcCtx.Redis.Setex(key, strconv.FormatInt(time.Now().UnixMilli(), 10), ttl); err != nil {
		// 写不进去 = 后续判定读不到标记 = 会被当成「没死过」直接放行改派。
		// 这里只能大声报出来:Redis 都写不了的时候,判定侧的 fail-closed
		// (读失败按屏障未到处理)才是真正兜底的那一层。
		logx.Errorf("[ReentryBarrier] 写 death_at 失败,屏障对该节点可能失效: zone=%d node=%s err=%v",
			zoneID, nodeID, err)
		return
	}
	logx.Infof("[ReentryBarrier] 记录节点死亡时刻: zone=%d node=%s barrier=%v",
		zoneID, nodeID, sceneReentryBarrier(svcCtx))
}

// clearNodeDeath 在节点重新注册(etcd PUT)时抹掉死亡标记。
//
// 不清的话,一个「死了又活过来」的 node_id 会被上一次死亡时刻继续压着 ——
// 虽然屏障总会走完,但那段时间里它作为**老属主**的场景无法被改派,而它此刻
// 其实是活的,IsNodeAlive 也说活,判定结果自相矛盾。
func clearNodeDeath(svcCtx *svc.ServiceContext, zoneID uint32, nodeID string) {
	if nodeID == "" {
		return
	}
	if _, err := svcCtx.Redis.Del(nodeDeathAtKey(zoneID, nodeID)); err != nil {
		logx.Errorf("[ReentryBarrier] 清除 death_at 失败: zone=%d node=%s err=%v", zoneID, nodeID, err)
	}
}

// CanReclaimDeadNode 判断「老属主 nodeID 名下的场景现在能不能被接管/清理」。
//
// 返回 (allowed, remaining):allowed=false 时 remaining 是还需要等待的时长,
// 仅供日志使用,调用方不应据此 sleep —— 正确做法是本轮跳过 / 返回可重试错误。
//
// 三种输入对应三种判定:
//
//	Redis 读失败        状态未知 → 不允许(fail-closed;拿不到证据不动 ownership)
//	没有 death_at 标记  没观察到它近期死亡 → 允许(它要么早就死了,要么没死过)
//	有 death_at 标记    now - death_at >= 屏障 才允许
//
// nodeID 为空表示这条场景根本没有老属主,没有任何进程可能在写它 → 允许。
func CanReclaimDeadNode(svcCtx *svc.ServiceContext, zoneID uint32, nodeID string) (bool, time.Duration) {
	barrier := sceneReentryBarrier(svcCtx)
	if nodeID == "" {
		return true, 0
	}

	raw, err := svcCtx.Redis.Get(nodeDeathAtKey(zoneID, nodeID))
	if err != nil {
		logx.Errorf("[ReentryBarrier] 读 death_at 失败,按屏障未到处理: zone=%d node=%s err=%v",
			zoneID, nodeID, err)
		return false, barrier
	}
	// go-zero 的 Get 把 redis.Nil 吞成 ("", nil):键不存在就是这一支。
	if raw == "" {
		return true, 0
	}

	deathMs, parseErr := strconv.ParseInt(raw, 10, 64)
	if parseErr != nil || deathMs <= 0 {
		// 标记被写坏了,读不出死亡时刻就等于没有证据。按屏障未到处理,
		// 最坏情况是这个节点的场景要等到标记 TTL 过期才会被自愈 —— 有界。
		logx.Errorf("[ReentryBarrier] death_at 值非法,按屏障未到处理: zone=%d node=%s raw=%q err=%v",
			zoneID, nodeID, raw, parseErr)
		return false, barrier
	}

	elapsed := time.Since(time.UnixMilli(deathMs))
	if elapsed < 0 {
		// death_at 在未来:写标记的进程与本进程墙钟劈叉。按「屏障刚开始」处理,
		// 绝不能因为算出负数就当作已过。
		logx.Errorf("[ReentryBarrier] death_at 在未来(墙钟劈叉),按屏障刚开始处理: zone=%d node=%s skew=%v",
			zoneID, nodeID, -elapsed)
		return false, barrier
	}
	if elapsed >= barrier {
		return true, 0
	}
	return false, barrier - elapsed
}

// reentryBarrierBlocks 是各改派 / 销毁 / 清理点的统一判定入口:被挡下时
// 统一打日志 + 记指标,调用方只需要看 bool。
//
// site 取本文件顶部的常量点位名。
func reentryBarrierBlocks(svcCtx *svc.ServiceContext, zoneID uint32, nodeID, site string) bool {
	allowed, remaining := CanReclaimDeadNode(svcCtx, zoneID, nodeID)
	if allowed {
		return false
	}
	metrics.ObserveReentryBarrierBlocked(zoneID, site)
	logx.Errorf("[ReentryBarrier] %s: 老属主刚判死,再入屏障还差 %v,本轮不改写 scene ownership: zone=%d node=%s",
		site, remaining, zoneID, nodeID)
	return true
}

// ---------------------------------------------------------------------------
// 被屏障推迟的死节点收尾
// ---------------------------------------------------------------------------

// deadNodeReconcile 是一次「等屏障走完再做」的死节点收尾任务。
//
// scenes 是**判死那一刻**该节点名下场景的快照,而不是收尾时再读一次:
// 屏障窗口里完全可能有新场景被建到同一个 node_id 上(进程换代但 id 复用),
// 收尾时现读会把这些新场景一起强制销毁。快照把收尾范围钉死在「它死时确实
// 属于它的那些场景」。
type deadNodeReconcile struct {
	entry  nodeEntry
	scenes []string
}

// pendingDeadNodes 保存已判死、但收尾动作还被屏障压着的节点。
//
// 只在 LoadReporter 的 watch / ticker 这条单 goroutine 链路上读写,加锁是为了
// 让测试能从别的 goroutine 观察。进程重启会丢掉队列 —— 代价是那批孤儿实例
// 场景的映射会残留到下一次节点死亡事件,不是数据损坏。
var (
	pendingDeadNodesMu sync.Mutex
	pendingDeadNodes   = make(map[string]deadNodeReconcile)
)

func pendingDeadNodeKey(zoneID uint32, nodeID string) string {
	return fmt.Sprintf("%d/%s", zoneID, nodeID)
}

// enqueueDeadNodeReconcile 记下一个待收尾的死节点,并立刻抄下它名下的场景快照。
func enqueueDeadNodeReconcile(svcCtx *svc.ServiceContext, entry nodeEntry) {
	setKey := nodeScenesKey(entry.reg.ZoneId, entry.nodeID)
	scenes, err := svcCtx.Redis.Smembers(setKey)
	if err != nil {
		// 快照读不到就别猜:留着反向索引,等下一次机会(fullSync 的陈旧清理或
		// 节点再次死亡)重新抄。这里若退化成「收尾时现读」,就把上面说的
		// 「误杀新场景」风险放了回来。
		logx.Errorf("[ReentryBarrier] 抄死节点场景快照失败,跳过本次收尾: zone=%d node=%s err=%v",
			entry.reg.ZoneId, entry.nodeID, err)
		return
	}

	pendingDeadNodesMu.Lock()
	pendingDeadNodes[pendingDeadNodeKey(entry.reg.ZoneId, entry.nodeID)] = deadNodeReconcile{
		entry:  entry,
		scenes: scenes,
	}
	pending := len(pendingDeadNodes)
	pendingDeadNodesMu.Unlock()

	logx.Infof("[ReentryBarrier] 死节点收尾入队(等屏障): zone=%d node=%s scenes=%d pending=%d",
		entry.reg.ZoneId, entry.nodeID, len(scenes), pending)
	publishDeadNodeReconcilePending()
}

// drainPendingDeadNodeReconciles 把屏障已过的死节点收尾做掉,返回本轮完成数。
//
// 由 LoadReporter 的 5s ticker 与每次 fullSync 驱动:屏障(默认 20s)远大于
// 一个 tick,所以「等屏障」天然就是「多等几拍」,不需要额外的定时器或
// 每个死节点一条 goroutine。
func drainPendingDeadNodeReconciles(ctx context.Context, svcCtx *svc.ServiceContext) int {
	pendingDeadNodesMu.Lock()
	snapshot := make([]deadNodeReconcile, 0, len(pendingDeadNodes))
	for _, task := range pendingDeadNodes {
		snapshot = append(snapshot, task)
	}
	pendingDeadNodesMu.Unlock()

	done := 0
	for _, task := range snapshot {
		zoneID := task.entry.reg.ZoneId
		if reentryBarrierBlocks(svcCtx, zoneID, task.entry.nodeID, barrierSiteDeadNodeCleanup) {
			continue
		}

		reconcileDeadNodeScenes(ctx, svcCtx, task.entry, task.scenes)
		// scene_count / player_count 必须在 reconcile **之后**清:
		// reconcile 里的 destroyInstanceForce 自己还会对这两个键做减法,
		// 先删会被它们重新建出来。(理由详见 removeNodeFromRedis)
		deleteNodeCounters(svcCtx, zoneID, task.entry.nodeID)

		pendingDeadNodesMu.Lock()
		delete(pendingDeadNodes, pendingDeadNodeKey(zoneID, task.entry.nodeID))
		pendingDeadNodesMu.Unlock()
		done++
	}

	if done > 0 {
		publishDeadNodeReconcilePending()
	}
	return done
}

// publishDeadNodeReconcilePending 把队列深度按 zone 发到 Prometheus。
// 已经归零的 zone 也要显式写 0,否则仪表盘会一直停在最后一个非零值上。
func publishDeadNodeReconcilePending() {
	pendingDeadNodesMu.Lock()
	counts := make(map[uint32]int, len(pendingDeadNodes))
	for _, task := range pendingDeadNodes {
		counts[task.entry.reg.ZoneId]++
	}
	pendingDeadNodesMu.Unlock()

	for _, zoneID := range GetActiveZones() {
		metrics.SetDeadNodeReconcilePending(zoneID, counts[zoneID])
	}
	for zoneID, n := range counts {
		metrics.SetDeadNodeReconcilePending(zoneID, n)
	}
}
