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
// 本文件提供四样东西:
//  1. 死亡时刻打点(markNodeDeath / clearNodeDeath)——只在 etcd 事件、fullSync 陈旧清扫与
//     推迟摘除重试(retryDeferredNodeDetaches)里调用;
//  2. 判定入口(reentryBarrierBlocks)——**所有**会改派 / 销毁 / 清理场景归属
//     的分支在动手前都要过它;
//  3. 被推迟的死节点收尾队列(死节点孤儿场景强制销毁 + 计数清理);
//  4. death_at 写不进 / 摘负载集失败时推迟的摘除(GO-6,见文件末尾「推迟的摘除」一段)。
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
	// barrierSiteStaleLocation:EnterScene 把已下线 zone 的陈旧 player location
	// 当作不存在之前的判定点(见 enterscenelogic.go playerLocationOwnerGone)。
	barrierSiteStaleLocation = "stale_location"
	// 单节点判死后的玩家接管(enterscenelogic.go playerLocationOwnerDead)。
	barrierSiteDeadOwnerTakeover = "dead_owner_takeover"
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
//
// 返回 error 而不是自己打日志吞掉:写不进去 = 后续判定读不到标记 = 会被当成「没死过」
// 直接放行改派。**death_at 落地之前,调用方不得把节点摘出负载集**(GO-6),而是带着上下文
// 记日志并进推迟队列(见 removeNodeFromRedis)。节点留在负载集里的整个推迟期间,改派 / 接管 /
// 清理点靠三层保证不动它:IsNodeAlive 门、world_init 两处改派要求 !IsNodeAlive、本领导者的
// CanReclaimDeadNode 对推迟态恒拒(见本文件末尾「推迟的摘除」一段)。单靠 IsNodeAlive 挡不住:
// reassignSceneNode 只经 reentryBarrierBlocks 查 death_at,孤儿清理对存活节点反而直接进删除循环。
func markNodeDeath(svcCtx *svc.ServiceContext, zoneID uint32, nodeID string) error {
	if nodeID == "" {
		return nil
	}
	key := nodeDeathAtKey(zoneID, nodeID)
	ttl := int(constants.NodeDeathMarkTTL / time.Second)
	if err := svcCtx.Redis.Setex(key, strconv.FormatInt(time.Now().UnixMilli(), 10), ttl); err != nil {
		return fmt.Errorf("写 death_at 失败: zone=%d node=%s: %w", zoneID, nodeID, err)
	}
	logx.Infof("[ReentryBarrier] 记录节点死亡时刻: zone=%d node=%s barrier=%v",
		zoneID, nodeID, sceneReentryBarrier(svcCtx))
	return nil
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
// 在它们之前还有一道进程内判定:本进程(领导者)的推迟摘除队列里有它 → 不允许(GO-6)。
//
// nodeID 为空表示这条场景根本没有老属主,没有任何进程可能在写它 → 允许。
func CanReclaimDeadNode(svcCtx *svc.ServiceContext, zoneID uint32, nodeID string) (bool, time.Duration) {
	barrier := sceneReentryBarrier(svcCtx)
	if nodeID == "" {
		return true, 0
	}

	// 推迟态 = 已观察到死亡、但 death_at 还没落地或还没摘出负载集(GO-6)。
	// world_init / reassignSceneNode / rebalance 机会迁移 / 孤儿清理只看 death_at,
	// 「没有 death_at ⇒ 放行」会让它们在老节点 15s drain 里改派。队列只在领导者上有内容,
	// 跨副本的那一层由负载集成员资格与 world_init 的 !IsNodeAlive 保证(见文件内 GO-6 段)。
	if isNodeDetachDeferred(zoneID, nodeID) {
		return false, barrier
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
	enqueueDeadNodeReconcileSnapshot(entry, scenes)
}

// enqueueDeadNodeReconcileSnapshot 用调用方已经抄好的场景快照入收尾队列。
// 推迟摘除(GO-6)用它交出**首次推迟时**抄的快照:推迟期间新建到同一 node_id 上的场景
// 不属于这次死亡的收尾范围,现读会把它们一起强制销毁(理由同 deadNodeReconcile 的注释)。
func enqueueDeadNodeReconcileSnapshot(entry nodeEntry, scenes []string) {
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

// ---------------------------------------------------------------------------
// 推迟的摘除(GO-6):death_at 没落地,就不把节点摘出负载集
// ---------------------------------------------------------------------------
//
// 以前 removeNodeFromRedis / fullSync 陈旧清扫在 markNodeDeath 的 Setex 失败后照样 ZREM,
// 读者于是看到「不在负载集(IsNodeAlive=false)+ 没有 death_at(屏障放行)」,在老节点
// 15s emergency drain 还在 SavePlayerToRedis 时就改派 / 接管 —— 正是再入屏障要防的双写回档。
//
// 不变量:
//
//	G6-I1 节点被摘出负载集时,必须满足两条之一:它的 death_at 已写进 Redis;或者本进程从第一次
//	      尝试摘它开始(单调时钟)已经满一个再入屏障。等价说法:任何读者看到「不在负载集 + 没有
//	      death_at」时,这个节点的屏障都已经走完。
//	G6-I2 节点「已判死、但还留在负载集里」(推迟态)的整个期间,任何路径都不改写它名下的
//	      scene ownership。三层保证:
//	      (1) IsNodeAlive 门:节点在负载集里 IsNodeAlive 就为真。playerLocationOwnerDead、
//	          playerLocationOwnerGone、resolveScene、world_init 的两处懒改派、rebalance 紧急迁移
//	          都因此拒绝。负载集是 Redis 里的共享状态,所有副本上都成立。
//	      (2) world_init 两处改派在「已从注册表消失」之外还要求 !IsNodeAlive:CreateScene 慢路径
//	          会在任意副本上跑 initWorldScenesForZone,新领导者在 resync fullSync 之前也可能先处理
//	          一个 PUT,这些地方手里都没有本队列。负载集成员资格是 G6-I1 唯一跨进程可见的证据。
//	      (3) 在持有本队列的领导者上,CanReclaimDeadNode 对推迟态恒返回 false,补住只看 death_at 的
//	          reassignSceneNode(含 rebalance 机会迁移走进来的那一路)、孤儿清理与 drain。
//
// 为什么满一个屏障后可以不带 death_at 摘除:death_at 唯一的作用是在观察到死亡之后的一个屏障
// 时长内让 CanReclaimDeadNode 拒绝;推迟期间上面三层已经让所有改派点拒绝,屏障由推迟态兜满。
// 首次观察时刻不早于 etcd 删除(也就不早于租约到期),用本进程单调时钟量的屏障不含跨进程
// 墙钟偏差,比 death_at 的墙钟口径更保守。
//
// 为什么不让 fullSync 返回 error、走外层 3s 重试:
//   - fullSync 出错时外层不进 watch,5s ticker(负载分、drain)、rebalance ticker、PUT/DELETE
//     处理会一起停住;首轮出错时 knownNodesSynced 也不会置位。
//   - go-redis 已按 MaxRetries 重试过,还能漏到应用层的单命令失败大概率是持续性故障,那个 3s
//     循环会无界地转下去(AGENTS §11 要求重试有上限 / 截止时间)。前瞻理由:redis.yaml 写明将来
//     要定 maxmemory 且必须 noeviction,写满时会持续拒 SET(denyoom)而 ZREM / DEL 照常。
//   - 写 death_at 失败与 ZREM 失败是同一个动作的两半,用同一个队列、同一个截止时间。
//
// 有界性(AGENTS §11):节拍是现成的 5s ticker 加每次 fullSync,不是紧循环。
//   - 写 death_at:截止 = 一个屏障(默认 20s),每个节点约 4~5 次,每次 1 条 SET;
//   - ZREM:放弃期限 deferredNodeDetachGiveUpAfter(10 分钟),最多约 120 次,每次 1 条 SET + 1 条
//     ZREM。过了它,任何写成的 death_at 都已过期、屏障早已走完,节点继续留在负载集只剩可用性代价
//     (按存活处理,方向 fail-closed),放弃后由下一次 fullSync(watch 重连 / 领导权变化 / 进程
//     重启)在负载集里重扫到它。
//   - 不加退避与抖动:单写者(领导者)对每个死节点一两条命令,节拍固定;退避和抖动是为打散大量
//     并发客户端,这里只有一个领导者。
//   - 所有时间判断都用 time.Since(单调读数)。
//
// 并发:队列只在 LoadReporter goroutine 上写(watch 事件、5s ticker、fullSync 同一条链路);
// CanReclaimDeadNode / 孤儿清理可能在 gRPC goroutine 上读,所以加锁。进程重启会丢队列:节点仍在
// 负载集里,新进程首次 fullSync 会重扫到它(首次观察时刻偏晚,方向安全)。

// deferredNodeDetachGiveUpAfter:摘负载集一直失败时放弃推迟的期限(取值理由见上面「有界性」)。
const deferredNodeDetachGiveUpAfter = constants.NodeDeathMarkTTL

type deferredNodeDetach struct {
	entry nodeEntry
	// 这一次死亡里本进程第一次尝试摘它的时刻(time.Now(),带单调读数)。重复失败不刷新:
	// 截止时间必须从最早一次算,否则一直失败的写会把摘除无限推后。
	firstObservedAt time.Time
	// 判死时刻的场景快照(与 deadNodeReconcile 同一口径)。推迟结束或节点重新注册时交给收尾队列;
	// 没抄到时 scenesCaptured=false,摘除时退回 enqueueDeadNodeReconcile 现读。
	scenes         []string
	scenesCaptured bool
}

var (
	deferredNodeDetachMu sync.Mutex
	deferredNodeDetaches = make(map[string]deferredNodeDetach) // 键 = pendingDeadNodeKey(zone,node)
)

// deferNodeDetach 把一个已判死、但这次没能摘出负载集的节点放进推迟队列;cause 是 markNodeDeath
// 或 detachDeadNode 的错误,只用于日志。节点**留在负载集里**,由 retryDeferredNodeDetaches 每拍重试。
//
// 首次入队时先取 time.Now() 再 Smembers(Smembers 可能阻塞到超时,不能让它把观察时刻往后推);
// 已有同键任务就只打日志,不刷新时间、不重抄快照、不重复计数。
func deferNodeDetach(svcCtx *svc.ServiceContext, entry nodeEntry, cause error) {
	zoneID, nodeID := entry.reg.ZoneId, entry.nodeID
	if existing, ok := lookupDeferredNodeDetach(zoneID, nodeID); ok {
		logx.Errorf("[ReentryBarrier][DeferDetach] 仍未摘出负载集,沿用首次推迟的时刻与快照(已等 %v): zone=%d node=%s err=%v",
			time.Since(existing.firstObservedAt), zoneID, nodeID, cause)
		return
	}

	task := deferredNodeDetach{entry: entry, firstObservedAt: time.Now()}
	scenes, snapErr := svcCtx.Redis.Smembers(nodeScenesKey(zoneID, nodeID))
	if snapErr != nil {
		// 快照没抄到不影响推迟本身;摘除时退回 enqueueDeadNodeReconcile 现读(那边读失败同样跳过收尾)。
		logx.Errorf("[ReentryBarrier][DeferDetach] 抄死节点场景快照失败,摘除时再现读: zone=%d node=%s err=%v",
			zoneID, nodeID, snapErr)
	} else {
		task.scenes = scenes
		task.scenesCaptured = true
	}

	key := pendingDeadNodeKey(zoneID, nodeID)
	deferredNodeDetachMu.Lock()
	if _, dup := deferredNodeDetaches[key]; dup {
		// 单写者下不会发生;真发生了也保留先入队那份(时刻更早)。
		deferredNodeDetachMu.Unlock()
		return
	}
	deferredNodeDetaches[key] = task
	pending := len(deferredNodeDetaches)
	deferredNodeDetachMu.Unlock()

	metrics.ObserveNodeDetachDeferred(zoneID, metrics.NodeDetachOutcomeDeferred)
	logx.Errorf("[ReentryBarrier][DeferDetach] 已判死但 death_at 未落地或摘负载集失败,节点暂留负载集(按存活处理),每拍重试: zone=%d node=%s scenes=%d captured=%v pending=%d err=%v",
		zoneID, nodeID, len(task.scenes), task.scenesCaptured, pending, cause)
}

func lookupDeferredNodeDetach(zoneID uint32, nodeID string) (deferredNodeDetach, bool) {
	deferredNodeDetachMu.Lock()
	defer deferredNodeDetachMu.Unlock()
	task, ok := deferredNodeDetaches[pendingDeadNodeKey(zoneID, nodeID)]
	return task, ok
}

// forgetDeferredNodeDetach 出队,返回任务原先是否存在(调用方据此决定记不记 outcome,避免重复计数)。
func forgetDeferredNodeDetach(zoneID uint32, nodeID string) bool {
	key := pendingDeadNodeKey(zoneID, nodeID)
	deferredNodeDetachMu.Lock()
	defer deferredNodeDetachMu.Unlock()
	if _, ok := deferredNodeDetaches[key]; !ok {
		return false
	}
	delete(deferredNodeDetaches, key)
	return true
}

// isNodeDetachDeferred:该节点是否处于推迟态(已判死、仍留在负载集)。只有领导者上会返回 true。
func isNodeDetachDeferred(zoneID uint32, nodeID string) bool {
	_, ok := lookupDeferredNodeDetach(zoneID, nodeID)
	return ok
}

// cancelDeferredNodeDetachOnReregister:节点重新注册(watch PUT,或重试时发现它已回到注册表)。
// 取消摘除 —— 不写 death_at(不能给活节点打死亡标记)、不摘负载集,记 dropped。
//
// 快照抄到了就交给收尾队列,与旧代码「DELETE 入队 → PUT 清 death_at → 下一拍 drain 收尾旧化身的
// 孤儿实例并删计数」时序一致(含其既有取舍:drain 按「没有 death_at」立即收尾,会删掉新化身已开始
// 累计的计数,有界)。快照没抄到时只打 Errorf:旧化身的孤儿实例映射与计数留到该节点下一次死亡时收,
// 与 enqueueDeadNodeReconcile 抄快照失败同口径。
func cancelDeferredNodeDetachOnReregister(zoneID uint32, nodeID string) {
	key := pendingDeadNodeKey(zoneID, nodeID)
	deferredNodeDetachMu.Lock()
	task, ok := deferredNodeDetaches[key]
	delete(deferredNodeDetaches, key)
	deferredNodeDetachMu.Unlock()
	if !ok {
		return
	}

	metrics.ObserveNodeDetachDeferred(zoneID, metrics.NodeDetachOutcomeDropped)
	if task.scenesCaptured {
		logx.Infof("[ReentryBarrier][DeferDetach] 节点在推迟摘除期间重新注册,放弃推迟的摘除;判死时刻快照交给收尾队列: zone=%d node=%s scenes=%d",
			zoneID, nodeID, len(task.scenes))
		enqueueDeadNodeReconcileSnapshot(task.entry, task.scenes)
		return
	}
	logx.Errorf("[ReentryBarrier][DeferDetach] 节点在推迟摘除期间重新注册,放弃推迟的摘除;判死时刻快照没抄到,旧化身的孤儿实例与计数留到该节点下一次死亡时收: zone=%d node=%s",
		zoneID, nodeID)
}

// retryDeferredNodeDetaches 重试推迟队列里的摘除,返回本轮完成摘除的个数。
// 只在 LoadReporter goroutine 上调用(5s ticker 与每次 fullSync)。
func retryDeferredNodeDetaches(svcCtx *svc.ServiceContext) int {
	deferredNodeDetachMu.Lock()
	if len(deferredNodeDetaches) == 0 {
		// 跟随者与健康领导者的常态:每拍只付一次加锁。
		deferredNodeDetachMu.Unlock()
		return 0
	}
	snapshot := make([]deferredNodeDetach, 0, len(deferredNodeDetaches))
	for _, task := range deferredNodeDetaches {
		snapshot = append(snapshot, task)
	}
	deferredNodeDetachMu.Unlock()

	leading := isLeader()
	barrier := sceneReentryBarrier(svcCtx)
	done := 0
	for _, task := range snapshot {
		z, n := task.entry.reg.ZoneId, task.entry.nodeID
		if !leading {
			// 不碰 Redis:节点仍在负载集(按存活处理),新领导者的 fullSync 会在负载集里重扫到它,
			// 用它自己的(更晚的)首次观察时刻重新推迟,方向保守。
			if forgetDeferredNodeDetach(z, n) {
				metrics.ObserveNodeDetachDeferred(z, metrics.NodeDetachOutcomeDropped)
				logx.Infof("[ReentryBarrier][DeferDetach] 本副本已不是领导者,放弃推迟的摘除(不碰 Redis,节点留在负载集由新领导者重扫): zone=%d node=%s",
					z, n)
			}
			continue
		}
		// 漏了 PUT 也不能给活节点打死亡标记:它已经回到注册表,按重新注册处理。
		if knownNodeIdentityMatchCount(z, n) > 0 {
			cancelDeferredNodeDetachOnReregister(z, n)
			continue
		}

		waited := time.Since(task.firstObservedAt)
		// ZREM 一直失败时每拍都会重写 death_at:屏障只会往后推(保守),不会提前失效。
		markErr := markNodeDeath(svcCtx, z, n)
		if markErr != nil && waited < barrier {
			logx.Errorf("[ReentryBarrier][DeferDetach] 仍写不进 death_at,已等 %v / 屏障 %v,下一拍再试: zone=%d node=%s err=%v",
				waited, barrier, z, n, markErr)
			continue
		}
		if err := detachDeadNode(svcCtx, task.entry, task.scenes, task.scenesCaptured); err != nil {
			if waited >= deferredNodeDetachGiveUpAfter {
				if forgetDeferredNodeDetach(z, n) {
					metrics.ObserveNodeDetachDeferred(z, metrics.NodeDetachOutcomeAbandoned)
				}
				logx.Errorf("[ReentryBarrier][DeferDetach] 自首次尝试已 %v 仍摘不出负载集,放弃;节点留在负载集按存活处理,由下一次 fullSync 重扫: zone=%d node=%s err=%v",
					waited, z, n, err)
				continue
			}
			logx.Errorf("[ReentryBarrier][DeferDetach] 摘负载集失败,下一拍再试(已等 %v / 放弃期限 %v): zone=%d node=%s err=%v",
				waited, deferredNodeDetachGiveUpAfter, z, n, err)
			continue
		}

		forgetDeferredNodeDetach(z, n)
		if markErr == nil {
			metrics.ObserveNodeDetachDeferred(z, metrics.NodeDetachOutcomeRecovered)
			logx.Infof("[ReentryBarrier][DeferDetach] 补写 death_at 成功,已摘出负载集(已等 %v): zone=%d node=%s",
				waited, z, n)
		} else {
			metrics.ObserveNodeDetachDeferred(z, metrics.NodeDetachOutcomeExpired)
			logx.Errorf("[ReentryBarrier][DeferDetach] 自首次尝试已 %v ≥ 屏障 %v 仍写不进 death_at,不带标记摘除(推迟期间 IsNodeAlive 为真、本 leader 的 CanReclaimDeadNode 恒拒,屏障已兜满): zone=%d node=%s err=%v",
				waited, barrier, z, n, markErr)
		}
		done++
	}
	return done
}
