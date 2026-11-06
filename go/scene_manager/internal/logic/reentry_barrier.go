package logic

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
//  3. 被推迟的死节点收尾(死节点孤儿场景强制销毁 + 计数清理),任务落 Redis,任何领导者都能接手;
//  4. death_at 写不进 / 摘负载集失败时推迟的摘除(GO-6,见文件末尾「推迟的摘除」一段)。
//
// 判定的默认方向是 fail-closed:拿不准就**不动** scene ownership。少改一次派
// 只是本轮不自愈(下一轮还会再来),改错一次是老进程疏散中的最后一笔进度被写权行拒掉而丢失。
// ---------------------------------------------------------------------------

// NodeDeathAtKeyFmt 是节点死亡时刻标记的 Redis 键格式,值是 Unix 毫秒。
// 与 NodeSceneCountKey / NodeMembersKeyFmt 同一命名族,便于按节点前缀清理。
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
	// 缩容排空收尾时属主已死(world_autoscale.go drainOrDestroyChannel 的死属主分支,2026-11-05)。
	barrierSiteAutoscaleDrain = "autoscale_drain"
	// barrierSiteStaleLocation:EnterScene 把已下线 zone 的陈旧 player location
	// 当作不存在之前的判定点(见 enterscenelogic.go playerLocationOwnerGone)。
	barrierSiteStaleLocation = "stale_location"
	// 单节点判死后的玩家接管(enterscenelogic.go playerLocationOwnerDead)。
	barrierSiteDeadOwnerTakeover = "dead_owner_takeover"
	// 节点号被同号新进程复用后的玩家接管(owner_replaced.go playerLocationOwnerReplaced,GO-3):EnterScene 一侧。
	barrierSiteReplacedOwnerTakeover = "replaced_owner_takeover"
	// 同一判定在 LeaveScene 释放判定里的那一侧(清理任务按自己的节奏重试,分开记才不会把重登次数抬高)。
	barrierSiteReplacedOwnerLeaveRelease = "replaced_owner_leave_release"
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
	logx.Infof("[ReentryBarrier] Recording node death time: zone=%d node=%s barrier=%v",
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
		logx.Errorf("[ReentryBarrier] Failed to clear death_at: zone=%d node=%s err=%v", zoneID, nodeID, err)
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
	// 「没有 death_at ⇒ 放行」会让它们在老节点冲突关停的有界疏散(15s)里改派,
	// 老节点的最后一笔进度被写权行拒掉而丢失。队列只在领导者上有内容,
	// 跨副本的那一层由负载集成员资格与 world_init 的 !IsNodeAlive 保证(见文件内 GO-6 段)。
	if isNodeDetachDeferred(zoneID, nodeID) {
		return false, barrier
	}

	raw, err := svcCtx.Redis.Get(nodeDeathAtKey(zoneID, nodeID))
	if err != nil {
		logx.Errorf("[ReentryBarrier] Failed to read death_at; treating barrier as not elapsed: zone=%d node=%s err=%v",
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
		logx.Errorf("[ReentryBarrier] Invalid death_at value; treating barrier as not elapsed: zone=%d node=%s raw=%q err=%v",
			zoneID, nodeID, raw, parseErr)
		return false, barrier
	}

	elapsed := time.Since(time.UnixMilli(deathMs))
	if elapsed < 0 {
		// death_at 在未来:写标记的进程与本进程墙钟劈叉。按「屏障刚开始」处理,
		// 绝不能因为算出负数就当作已过。
		logx.Errorf("[ReentryBarrier] death_at in future (wall-clock divergence); treating barrier as just started: zone=%d node=%s skew=%v",
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
	logx.Errorf("[ReentryBarrier] %s: previous owner just declared dead, reentry barrier needs %v more; scene ownership unchanged this round: zone=%d node=%s",
		site, remaining, zoneID, nodeID)
	return true
}

// ---------------------------------------------------------------------------
// 被屏障推迟的死节点收尾:任务落 Redis,任何新当选的领导者都能接手
// ---------------------------------------------------------------------------
//
// 一条收尾任务由三个键组成,都在控制域共享 Redis 里(与 scene_manager 其它多键 Lua 脚本同一前提):
//
//	scene_manager:dead_node_reconcile:zone:{z}  ZSET,成员 "{zone}/{node}",分数 = 首次入队时刻(Unix 毫秒,只用于排序与日志)
//	node:zone:{z}:{n}:reconcile_scenes   SET,这次死亡还没收完的场景(收尾范围):入队时从反向索引
//	                                     node:zone:{z}:{n}:scenes 抄出判死时刻的快照,此后每处理完一个摘掉一个
//	node:zone:{z}:{n}:reconcile_instance STRING,判死的那一代进程的 node_uuid;不知道时没有这个键
//
// 判死那一代若又回来了,任务整条撤掉(R1-I5,见 cancelDeadNodeReconcileIfInstanceBack):C++ 节点 keepalive 回 TTL=0
// 或本地租约超时后只是重新拿租约、按原 node_id 重新注册(etcd_service.cpp OnKeepAliveResponse、node.cpp),进程和
// 它名下的实例都还在;只有分配键 CAS 失败才走冲突关停。以前重新注册的 PUT 只清 death_at,CanReclaimDeadNode 随即
// 放行,下一拍就按判死时刻的快照把活节点上的实例拆掉,还向它发 DestroyScene(缺口清单第 32 条)。
//
// 2026-10-30 之前任务与快照只在领导者进程内存里:drain 没有领导者闸门,旧领导者降级后照样删计数并出队;新领导者的
// fullSync 只扫负载集,看不到已摘出负载集的死节点;进程重启丢队列;failed 成员没有重试(docs/PROGRESS.md 2026-10-23
// 「死节点收尾与显式 DestroyScene 的销毁改为按认领时属主」条目的残余 R1)。
//
// 不变量:
//
//	R1-I1 节点被摘出负载集的那一刻,它的收尾任务已经在 Redis 里:摘负载集与写任务是同一次 Lua
//	      (luaEnqueueDeadNodeReconcile)。所以任何领导者都能发现每个死节点 —— 还在负载集里的由 fullSync 的
//	      陈旧清扫重扫,已摘出去的由任务表接手。
//	R1-I2 收尾范围是判死时刻的快照,入队之后不再从反向索引补抄:屏障窗口里新化身建到同一 node_id 上的场景不属于
//	      这次死亡,补抄会把它们一起强制销毁(属主比对拦不住,它们的属主就是这个 node_id)。同一 node_id 在上一条
//	      任务做完之前再次判死时,两次的快照取并集。
//	R1-I3 死节点的计数键(scene_count / members)只在收尾范围已空时、与出队在同一次 Lua 里删掉(放弃时强制删),
//	      理由见 finishDeadNodeReconcile。
//	R1-I4 只有领导者读写任务;降级即停,一个字节不写回。旧领导者没做完的,新领导者照 Redis 里剩下的接着做。每个
//	      场景的销毁是 Lua 原子认领(destroyOrphanOfDeadNode),新旧领导者短暂重叠时副作用也只执行一次;重做已处理
//	      过的成员只会落到 stale / world_skipped。
//	R1-I5 判死那一代的 node_uuid 又在注册表里(同一个进程失租后重新注册)时,任务在销毁任何场景之前撤掉,计数不删:
//	      那些场景与计数都属于活着的它。记的是**最近一次**判死的那一代 —— 同一 node_id 上更早的一代不可能再回来
//	      (新一代占了分配键,旧一代重注册的 CAS 必然失败、冲突关停)。两次判死之间换过代、而回来的是后一代时,
//	      前一代留下的孤儿实例随任务一起撤掉不拆,方向是宁可留孤儿也不拆活实例;它们由空闲回收或该 node_id 下次判死时处理。
//	      不知道判死那一代是谁(注册里没有 node_uuid 的旧版本节点;fullSync 清扫到本副本镜像里没有的节点)时照旧收尾。
//
// 有界重试(AGENTS §11.3):一轮里有成员没结论(Redis 出错)时任务留着,下一拍(5s ticker / fullSync)只重试收尾
// 范围里剩下的。本进程从第一次没做完起(单调时钟)满 deadNodeReconcileGiveUpAfter 仍做不完就放弃:删计数、删任务,
// 没结论的成员留在反向索引里,等同一 node_id 再次判死或空闲回收(与 10-30 之前「跑一次就出队」的结局相同,只是晚了
// 一个放弃期限)。计时只在进程内:换领导者时新领导者从头计时,放弃更晚,方向是多重试;每个领导任期内仍有界。不把
// 计时写进 Redis,是因为跨进程的截止时间只能用墙钟。不加退避与抖动,理由同「推迟的摘除」一段:单写者、节拍固定。

const (
	// deadNodeReconcileTasksKeyFmt:死节点收尾任务表,每个 zone 一张(docs/design/control-plane-zone-scoping-20261105.md §4.2)。
	// 选主按 zone(LeaderLockKey),一个 zone 的表只由那个 zone 的领导者读写;同组几个 zone 共用一个控制域实例,
	// 原来全服一张的表会让一个 zone 的领导者去收同组别的 zone 死节点的尾。成员的 zone 段与表的 zone 不一致的
	// 按坏成员摘掉。键名不以 node:zone: 开头,合服按 node:zone:{S}:* 前缀清源 zone 热状态时不会命中它:源 zone 下线后
	// 没有领导者再处理它的表,要由合服流程显式删掉(残余见设计稿)。
	deadNodeReconcileTasksKeyFmt = "scene_manager:dead_node_reconcile:zone:%d"

	// NodeReconcileScenesKeyFmt:一个死节点还没收完的场景(收尾范围),与 NodeScenesKeyFmt 同一命名族。
	NodeReconcileScenesKeyFmt = "node:zone:%d:%s:reconcile_scenes"

	// NodeReconcileInstanceKeyFmt:判死的那一代进程的 node_uuid(R1-I5)。与收尾范围同生同灭:入队脚本写、
	// 结束 / 撤销脚本删;同一 node_id 再次判死时改成最近这一代,不知道是哪一代时删掉。
	NodeReconcileInstanceKeyFmt = "node:zone:%d:%s:reconcile_instance"

	// deadNodeReconcileGiveUpAfter:收尾一直做不完时放弃的期限。Redis 主从切换 / 重启是分钟级,10 分钟约 120 拍,
	// 足够跨过一次;过了它还做不完,多半是持续性故障或个别成员的脏数据,再等也不会好。
	deadNodeReconcileGiveUpAfter = 10 * time.Minute
)

// luaEnqueueDeadNodeReconcile 写一条收尾任务;ARGV[4] 非空时在同一次脚本里先把节点摘出负载集(R1-I1)。
//
// KEYS[1] = 任务表 scene_manager:dead_node_reconcile:zone:{z}(与其余三把键同一个 zone,同一个控制域实例)
// KEYS[2] = 收尾范围 node:zone:{z}:{n}:reconcile_scenes
// KEYS[3] = 反向索引 node:zone:{z}:{n}:scenes(只在 ARGV[3] == 'index' 时读)
// KEYS[4] = 负载集 scene_nodes:zone:{z}:load
// KEYS[5] = 判死那一代的实例号 node:zone:{z}:{n}:reconcile_instance
// ARGV[1] = 任务成员 "{z}/{n}"
// ARGV[2] = 入队时刻(Unix 毫秒);成员已在表里时保留更早的分数(ZADD NX)
// ARGV[3] = 'index':收尾范围从反向索引抄;'given':用 ARGV[6..]
// ARGV[4] = 要摘出负载集的节点 id;空串 = 不碰负载集(节点重新注册时只交出快照)
// ARGV[5] = 判死那一代的 node_uuid;空串 = 不知道(删掉旧记录:旧记录说的是更早的一代,拿它判「回来了」会认错人)
// ARGV[6..] = 'given' 时的场景成员
// 返回收尾范围的成员数。
//
// ZREM 放在最前:脚本中途报错时已执行的写不会回滚(同 world_init.go luaReassignSceneNode 的写法),ZREM 失败时什么都
// 没写,节点照旧留在负载集、进推迟队列。之后的写只会因键类型错(数据损坏)失败;内存满时 Redis 只在脚本第一次写之前
// 拒绝 denyoom 命令,ZREM 不是 denyoom,真摘掉了成员之后的 SUNIONSTORE / ZADD 不会被拒。
// 收尾范围用 SUNIONSTORE / SADD 与已有内容取并集(R1-I2)。
const luaEnqueueDeadNodeReconcile = `
if ARGV[4] ~= '' then
    redis.call('ZREM', KEYS[4], ARGV[4])
end
if ARGV[3] == 'index' then
    redis.call('SUNIONSTORE', KEYS[2], KEYS[2], KEYS[3])
else
    for i = 6, #ARGV do
        redis.call('SADD', KEYS[2], ARGV[i])
    end
end
if ARGV[5] ~= '' then
    redis.call('SET', KEYS[5], ARGV[5])
else
    redis.call('DEL', KEYS[5])
end
redis.call('ZADD', KEYS[1], 'NX', ARGV[2], ARGV[1])
return redis.call('SCARD', KEYS[2])
`

// luaFinishDeadNodeReconcile 结束一条收尾任务:收尾范围已空时删死节点的计数键并出队(R1-I3);
// ARGV[2] == '1'(放弃)时不看收尾范围,连同它一起删。
//
// KEYS[1] = 收尾范围  KEYS[2] = scene_count  KEYS[3] = members  KEYS[4] = 任务表  KEYS[5] = 判死那一代的实例号
// ARGV[1] = 任务成员  ARGV[2] = '1' 表示放弃
// 返回 1 = 已出队;0 = 收尾范围不空,任务原样留着。
//
// 任务已不在表里时直接回 1、一个键都不动:它已被别的领导者做完,或因判死那一代回来而撤掉(R1-I5)。领导者短暂重叠时,
// 手里还拿着旧列表的一方若照删计数,删的就是回来的那一代(或复用 node_id 的新一代)正在累计的 scene_count / members。
const luaFinishDeadNodeReconcile = `
if not redis.call('ZSCORE', KEYS[4], ARGV[1]) then
    return 1
end
if ARGV[2] ~= '1' and redis.call('SCARD', KEYS[1]) > 0 then
    return 0
end
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3], KEYS[5])
redis.call('ZREM', KEYS[4], ARGV[1])
return 1
`

// luaCancelDeadNodeReconcileForLiveInstance 撤掉一条收尾任务(R1-I5):记下的判死那一代就是 ARGV[2] 时,删任务、收尾范围
// 与实例号记录;不碰场景、不碰计数(scene_count / members 属于回来的这一代),也不碰反向索引。
//
// KEYS[1] = 任务表  KEYS[2] = 收尾范围  KEYS[3] = 判死那一代的实例号
// ARGV[1] = 任务成员  ARGV[2] = 注册表里现在的 node_uuid
// 返回 1 = 已撤掉;0 = 记录不是这一代(或没有记录、或任务已被别的领导者做完),一个键都不动。
// 先读后写,只会在第一次写之前因键类型错报错,报错时什么都没改。
const luaCancelDeadNodeReconcileForLiveInstance = `
if ARGV[2] == '' or redis.call('GET', KEYS[3]) ~= ARGV[2] then
    return 0
end
redis.call('ZREM', KEYS[1], ARGV[1])
redis.call('DEL', KEYS[2], KEYS[3])
return 1
`

// deadNodeReconcileTasksKey 返回 zone 的死节点收尾任务表。
func deadNodeReconcileTasksKey(zoneID uint32) string {
	return fmt.Sprintf(deadNodeReconcileTasksKeyFmt, zoneID)
}

// pendingDeadNodeKey 是 (zone,node) 的队列键:推迟摘除表的键,也是收尾任务表的成员。
func pendingDeadNodeKey(zoneID uint32, nodeID string) string {
	return fmt.Sprintf("%d/%s", zoneID, nodeID)
}

// parseDeadNodeReconcileMember 是 pendingDeadNodeKey 的逆。
func parseDeadNodeReconcileMember(member string) (uint32, string, bool) {
	zonePart, nodeID, ok := strings.Cut(member, "/")
	if !ok || nodeID == "" {
		return 0, "", false
	}
	zoneID, err := strconv.ParseUint(zonePart, 10, 32)
	if err != nil {
		return 0, "", false
	}
	return uint32(zoneID), nodeID, true
}

func nodeReconcileScenesKey(zoneID uint32, nodeID string) string {
	return fmt.Sprintf(NodeReconcileScenesKeyFmt, zoneID, nodeID)
}

func nodeReconcileInstanceKey(zoneID uint32, nodeID string) string {
	return fmt.Sprintf(NodeReconcileInstanceKeyFmt, zoneID, nodeID)
}

// enqueueDeadNodeReconcile 把一个已判死的节点写进收尾任务表,返回收尾范围的成员数。
//
// detach=true 时在同一次脚本里先把节点摘出负载集(detachDeadNode 用,R1-I1);detach=false 只交出快照
// (节点已重新注册,不能碰负载集)。scenesCaptured=true 时收尾范围用调用方抄好的 scenes —— 推迟摘除(GO-6)交出的
// **首次推迟时**的快照:推迟期间节点留在负载集里、还能被派新场景,那些不属于这次死亡;否则在脚本里从反向索引现抄
// (此刻就是判死时刻)。脚本出错时什么都没写(见脚本注释),调用方按失败处理。
//
// entry.reg.NodeUuid 是判死的那一代(R1-I5):watch DELETE 交来的条目带着它;fullSync 清扫从旧镜像补上;
// 推迟摘除沿用首次推迟时的条目。为空时记为「不知道」,这一代回来也认不出,照旧收尾。
func enqueueDeadNodeReconcile(svcCtx *svc.ServiceContext, entry nodeEntry, detach bool, scenes []string, scenesCaptured bool) (int64, error) {
	zoneID, nodeID := entry.reg.ZoneId, entry.nodeID
	detachMember := ""
	if detach {
		detachMember = nodeID
	}
	mode := "index"
	if scenesCaptured {
		mode = "given"
	}
	args := []any{pendingDeadNodeKey(zoneID, nodeID), strconv.FormatInt(time.Now().UnixMilli(), 10), mode, detachMember, entry.reg.NodeUuid}
	if scenesCaptured {
		args = append(args, stringsAsArgs(scenes)...)
	}
	keys := []string{deadNodeReconcileTasksKey(zoneID), nodeReconcileScenesKey(zoneID, nodeID), nodeScenesKey(zoneID, nodeID), nodeLoadKey(zoneID),
		nodeReconcileInstanceKey(zoneID, nodeID)}
	raw, err := svcCtx.Redis.Eval(luaEnqueueDeadNodeReconcile, keys, args...)
	if err != nil {
		return 0, fmt.Errorf("写死节点收尾任务失败: zone=%d node=%s: %w", zoneID, nodeID, err)
	}
	scope := toInt64(raw)
	logx.Infof("[ReentryBarrier] Dead node cleanup queued in Redis (awaiting barrier): zone=%d node=%s instance=%q scenes=%d captured=%v detached=%v",
		zoneID, nodeID, entry.reg.NodeUuid, scope, scenesCaptured, detach)
	return scope, nil
}

// deadNodeReconcileTask 是从任务表里读出的一条任务。
type deadNodeReconcileTask struct {
	member   string // 任务表成员 "{zone}/{node}"
	zoneID   uint32
	nodeID   string
	queuedAt time.Time // 首次入队时刻(墙钟),只用于日志
}

func (t deadNodeReconcileTask) entry() nodeEntry {
	e := nodeEntry{nodeID: t.nodeID}
	e.reg.ZoneId = t.zoneID
	return e
}

// listDeadNodeReconcileTasks 读本 zone 的任务表,按入队先后返回。解析不了的成员(不是本代码写的,对不上任何节点),
// 以及 zone 段不是本 zone 的成员(一个 zone 的表只可能被那个 zone 的领导者推进)单独返回。
func listDeadNodeReconcileTasks(svcCtx *svc.ServiceContext) ([]deadNodeReconcileTask, []string, error) {
	ownZone := svcCtx.Config.ZoneId
	tasksKey := deadNodeReconcileTasksKey(ownZone)
	pairs, err := svcCtx.Redis.ZrangeWithScores(tasksKey, 0, -1)
	if err != nil {
		return nil, nil, fmt.Errorf("zrange %s: %w", tasksKey, err)
	}
	tasks := make([]deadNodeReconcileTask, 0, len(pairs))
	var malformed []string
	for _, p := range pairs {
		zoneID, nodeID, ok := parseDeadNodeReconcileMember(p.Key)
		if !ok || zoneID != ownZone {
			malformed = append(malformed, p.Key)
			continue
		}
		tasks = append(tasks, deadNodeReconcileTask{member: p.Key, zoneID: zoneID, nodeID: nodeID, queuedAt: time.UnixMilli(p.Score)})
	}
	return tasks, malformed, nil
}

// drainPendingDeadNodeReconciles 对屏障已过的收尾任务各做一轮,返回本轮出队(做完、放弃,或因判死那一代回来而撤掉)的任务数。
//
// 由 LoadReporter 的 5s ticker 与每次 fullSync 的领导者分支驱动:屏障(默认 20s)远大于一拍,「等屏障」就是
// 「多等几拍」,不需要额外的定时器或每个死节点一条 goroutine。新当选的领导者经 resync → fullSync 立刻接手
// 旧领导者没做完的任务(R1-I4)。
func drainPendingDeadNodeReconciles(ctx context.Context, svcCtx *svc.ServiceContext) int {
	if !isLeader() {
		// 跟随者不读不写任务表。本进程当领导者时留下的重试计时与队列深度一并清掉:再当选时从头计时
		// (放弃更晚,方向保守),深度由现任领导者发布。
		resetDeadNodeReconcileLocalState()
		return 0
	}
	tasks, malformed, err := listDeadNodeReconcileTasks(svcCtx)
	if err != nil {
		logx.Errorf("[ReentryBarrier] Failed to list dead node cleanup tasks; retrying next tick: %v", err)
		return 0
	}
	for _, member := range malformed {
		// 对不上任何 (zone,node),驱动不了任何销毁;摘掉,免得每拍报一次。
		logx.Errorf("[ReentryBarrier] Dropping malformed dead node cleanup task %q from %s", member, deadNodeReconcileTasksKey(svcCtx.Config.ZoneId))
		if _, err := svcCtx.Redis.Zrem(deadNodeReconcileTasksKey(svcCtx.Config.ZoneId), member); err != nil {
			logx.Errorf("[ReentryBarrier] Failed to drop malformed dead node cleanup task %q: %v", member, err)
		}
	}
	keepDeadNodeReconcileRetriesOnlyFor(tasks)

	pending := make(map[uint32]int, len(tasks))
	for _, task := range tasks {
		pending[task.zoneID]++
	}
	done := 0
	for _, task := range tasks {
		if !isLeader() {
			// 降级即停:剩下的原样留在 Redis 里给新领导者;深度也不发布,下一拍走上面的跟随者分支清掉。
			return done
		}
		// 先于屏障判:判死那一代回来了就撤任务(R1-I5)。重新注册的 PUT 会清掉 death_at,屏障随即放行,
		// 放在屏障之后就正好赶上「放行的那一拍」把活实例拆掉。
		switch cancelDeadNodeReconcileIfInstanceBack(svcCtx, task) {
		case instanceBackCancelled:
			pending[task.zoneID]--
			done++
			continue
		case instanceBackUnknown:
			continue
		}
		if reentryBarrierBlocks(svcCtx, task.zoneID, task.nodeID, barrierSiteDeadNodeCleanup) {
			continue
		}
		finished, lostLeadership := reconcileDeadNodeTask(ctx, svcCtx, task)
		if lostLeadership {
			return done
		}
		if finished {
			pending[task.zoneID]--
			done++
		}
	}
	publishDeadNodeReconcilePending(svcCtx, pending)
	return done
}

// instanceBackVerdict 是 cancelDeadNodeReconcileIfInstanceBack 的结论。
type instanceBackVerdict int

const (
	// instanceNotBack:判死那一代不在注册表里,或认不出是不是它(换了一代、任务没记是哪一代):照常走屏障与收尾。
	instanceNotBack instanceBackVerdict = iota
	// instanceBackCancelled:判死那一代回来了,任务已撤,场景与计数原样。
	instanceBackCancelled
	// instanceBackUnknown:注册表里有这个 node_id,撤销脚本却报错,不知道是不是原来那一代:这一拍不收。
	instanceBackUnknown
)

// cancelDeadNodeReconcileIfInstanceBack:判死的那一代进程又在注册表里了,就撤掉它的收尾任务(R1-I5)。
//
// 看的是本副本的注册表镜像:watch PUT 先写镜像、后清 death_at,所以屏障因重新注册而放行的那一拍,镜像里一定已经有它;
// fullSync 补上的注册同样先进镜像。镜像里这个 (zone,node) 一条注册都没有 → instanceNotBack。有注册时逐个拿它们的
// node_uuid 去比任务记下的那一代;比对与撤销在同一段脚本里,不会撤掉刚被别的领导者改写过的任务。都对不上
// (换了一代进程、或任务不知道是哪一代)照常收尾。脚本报错时这一拍不收:node_id 又注册上来了,却不知道是不是
// 原来那一代,拆下去可能拆的是活实例;下一拍再判。
func cancelDeadNodeReconcileIfInstanceBack(svcCtx *svc.ServiceContext, task deadNodeReconcileTask) instanceBackVerdict {
	instances := registeredInstancesOf(task.zoneID, task.nodeID)
	if len(instances) == 0 {
		return instanceNotBack
	}
	keys := []string{
		deadNodeReconcileTasksKey(task.zoneID),
		nodeReconcileScenesKey(task.zoneID, task.nodeID),
		nodeReconcileInstanceKey(task.zoneID, task.nodeID),
	}
	for _, instance := range instances {
		raw, err := svcCtx.Redis.Eval(luaCancelDeadNodeReconcileForLiveInstance, keys, task.member, instance)
		if err != nil {
			logx.Errorf("[ReentryBarrier] Dead node cleanup paused this tick: the node_id is registered again but checking whether it is the same process failed, nothing destroyed: zone=%d node=%s instance=%s err=%v",
				task.zoneID, task.nodeID, instance, err)
			return instanceBackUnknown
		}
		if toInt64(raw) == 1 {
			forgetDeadNodeReconcileRetry(task.member)
			metrics.ObserveDeadNodeReconcile(task.zoneID, metrics.DeadNodeReconcileOutcomeInstanceBack)
			logx.Infof("[ReentryBarrier] Dead node cleanup cancelled: the same process re-registered after losing its lease, its instances are alive; task removed, scenes and counters kept: zone=%d node=%s instance=%s queued_at=%s",
				task.zoneID, task.nodeID, instance, task.queuedAt.Format(time.RFC3339))
			return instanceBackCancelled
		}
	}
	return instanceNotBack
}

// registeredInstancesOf 返回镜像里 (zone,node) 现在注册着的 node_uuid,去重、去空串、升序。
// 一般只有一个;同一身份挂着两条注册(两代进程并存)时两个都返回,任一个是判死那一代就撤。
func registeredInstancesOf(zoneID uint32, nodeID string) []string {
	knownNodesMu.RLock()
	defer knownNodesMu.RUnlock()
	seen := make(map[string]struct{})
	for _, entry := range knownNodes {
		if entry.reg.ZoneId == zoneID && entry.nodeID == nodeID && entry.reg.NodeUuid != "" {
			seen[entry.reg.NodeUuid] = struct{}{}
		}
	}
	instances := make([]string, 0, len(seen))
	for instance := range seen {
		instances = append(instances, instance)
	}
	slices.Sort(instances)
	return instances
}

// reconcileDeadNodeTask 对一条屏障已过的任务做一轮收尾。finished = 任务已出队(做完或放弃);
// lostLeadership = 中途失去领导权,什么都没写回,调用方停掉本轮。
func reconcileDeadNodeTask(ctx context.Context, svcCtx *svc.ServiceContext, task deadNodeReconcileTask) (finished, lostLeadership bool) {
	scopeKey := nodeReconcileScenesKey(task.zoneID, task.nodeID)
	members, err := svcCtx.Redis.Smembers(scopeKey)
	if err != nil {
		return retryOrAbandonDeadNodeReconcile(svcCtx, task, fmt.Errorf("smembers %s: %w", scopeKey, err)), false
	}
	result := reconcileDeadNodeScenes(ctx, svcCtx, task.entry(), members)
	if result.lostLeadership {
		return false, true
	}
	// 有结论的摘出收尾范围,下一拍只重试剩下的。摘失败也不要紧:重做它们只会落到 stale / world_skipped。
	if len(result.resolved) > 0 {
		if _, err := svcCtx.Redis.Srem(scopeKey, stringsAsArgs(result.resolved)...); err != nil {
			return retryOrAbandonDeadNodeReconcile(svcCtx, task, fmt.Errorf("srem %s: %w", scopeKey, err)), false
		}
	}
	if len(result.failed) > 0 || result.indexUnlinkFailed {
		return retryOrAbandonDeadNodeReconcile(svcCtx, task, fmt.Errorf("%d of %d scenes without a verdict (index unlink failed: %v)",
			len(result.failed), len(members), result.indexUnlinkFailed)), false
	}
	finished, err = finishDeadNodeReconcile(svcCtx, task, false)
	if err != nil {
		return retryOrAbandonDeadNodeReconcile(svcCtx, task, err), false
	}
	if !finished {
		// 读完收尾范围之后又进来了新成员(同一 node_id 再次判死):下一拍接着收,不算失败。
		logx.Infof("[ReentryBarrier] Dead node cleanup scope grew during this pass; continuing next tick: zone=%d node=%s",
			task.zoneID, task.nodeID)
		return false, false
	}
	forgetDeadNodeReconcileRetry(task.member)
	metrics.ObserveDeadNodeReconcile(task.zoneID, metrics.DeadNodeReconcileOutcomeCompleted)
	logx.Infof("[ReentryBarrier] Dead node cleanup finished; counters deleted, task removed: zone=%d node=%s scenes=%d queued_at=%s",
		task.zoneID, task.nodeID, len(members), task.queuedAt.Format(time.RFC3339))
	return true, false
}

// retryOrAbandonDeadNodeReconcile 处理没做完的一轮:放弃期限内留到下一拍;满期限就放弃(删计数、删任务)。
// 返回任务是否已出队。期限从本进程第一次没做完这条任务时起算(单调时钟),见本段开头「有界重试」。
func retryOrAbandonDeadNodeReconcile(svcCtx *svc.ServiceContext, task deadNodeReconcileTask, cause error) bool {
	waited := time.Since(deadNodeReconcileRetryStartedAt(task.member))
	if waited < deadNodeReconcileGiveUpAfter {
		metrics.ObserveDeadNodeReconcile(task.zoneID, metrics.DeadNodeReconcileOutcomeRetry)
		logx.Errorf("[ReentryBarrier] Dead node cleanup incomplete; retrying the rest of %s next tick (waited %v / give-up %v): zone=%d node=%s err=%v",
			nodeReconcileScenesKey(task.zoneID, task.nodeID), waited, deadNodeReconcileGiveUpAfter, task.zoneID, task.nodeID, cause)
		return false
	}
	finished, err := finishDeadNodeReconcile(svcCtx, task, true)
	if err != nil {
		logx.Errorf("[ReentryBarrier] Failed to abandon dead node cleanup; retrying next tick: zone=%d node=%s cause=%v err=%v",
			task.zoneID, task.nodeID, cause, err)
		return false
	}
	forgetDeadNodeReconcileRetry(task.member)
	metrics.ObserveDeadNodeReconcile(task.zoneID, metrics.DeadNodeReconcileOutcomeAbandoned)
	logx.Errorf("[ReentryBarrier] Dead node cleanup still incomplete after %v; abandoned: counters deleted, task removed, unresolved scenes stay in %s (handled only if this node_id is declared dead again, or by idle reclaim): zone=%d node=%s err=%v",
		waited, nodeScenesKey(task.zoneID, task.nodeID), task.zoneID, task.nodeID, cause)
	return finished
}

// finishDeadNodeReconcile 结束一条任务:收尾范围已空时删死节点的计数键并出队;abandon=true 时不看收尾范围,
// 连同它一起删。返回 true = 已出队;false = 收尾范围不空,任务原样留着。
//
// 为什么要删计数(R1-I3):这两个键全仓没有任何「节点重新上线时归零」的路径,死节点残留的正计数与成员
// 会被复用同 id 的新节点原样继承,负载分永久虚高,调度一直躲着它。
//
// 为什么等收尾范围空了才删:收尾期间 destroyOrphanOfDeadNode 每认领一个场景,就在认领脚本里同一步扣一次死节点的
// scene_count(按期望属主,scene_atomic.go luaSceneClaimCommit),等范围空了一次删掉,计数在收尾期间一直是「还剩几个
// 没收」。顺序不再关系正确性(2026-11-05):认领脚本只在计数 > 0 时才减,计数键先被删掉也不会重建成 -1 —— 放弃之后
// 剩下的场景由空闲回收销毁时同样如此;认领之后没做完、由拆除任务补做的副作用(scene_teardown_job.go)不碰计数,
// 成员集合的 SREM 不会重建键。删计数与出队在同一次脚本里,不会出现「任务出队了、计数没删」,也不会出现反过来的半截。
func finishDeadNodeReconcile(svcCtx *svc.ServiceContext, task deadNodeReconcileTask, abandon bool) (bool, error) {
	keys := []string{
		nodeReconcileScenesKey(task.zoneID, task.nodeID),
		nodeSceneCountKey(task.zoneID, task.nodeID),
		nodeMembersKey(task.zoneID, task.nodeID),
		deadNodeReconcileTasksKey(task.zoneID),
		nodeReconcileInstanceKey(task.zoneID, task.nodeID),
	}
	abandonFlag := "0"
	if abandon {
		abandonFlag = "1"
	}
	raw, err := svcCtx.Redis.Eval(luaFinishDeadNodeReconcile, keys, task.member, abandonFlag)
	if err != nil {
		return false, fmt.Errorf("结束死节点收尾任务失败: zone=%d node=%s: %w", task.zoneID, task.nodeID, err)
	}
	return toInt64(raw) == 1, nil
}

// 本进程的收尾重试计时与已发布的队列深度。只在 LoadReporter goroutine 上写;加锁是为了让测试能从别的 goroutine 改。
var (
	deadNodeReconcileLocalMu sync.Mutex
	// deadNodeReconcileRetrySince:本进程第一次没做完某条任务的时刻(time.Now(),带单调读数),键 = 任务成员。
	// 重复失败不刷新:截止时间必须从最早一次算,否则一直失败会把放弃无限推后。
	deadNodeReconcileRetrySince = make(map[string]time.Time)
	// deadNodeReconcilePublishedZones:上一次发布时队列深度非 0 的 zone,见 publishDeadNodeReconcilePending。
	deadNodeReconcilePublishedZones = make(map[uint32]struct{})
)

// deadNodeReconcileRetryStartedAt 返回本进程第一次没做完 member 的时刻;第一次调用时记为此刻。
func deadNodeReconcileRetryStartedAt(member string) time.Time {
	deadNodeReconcileLocalMu.Lock()
	defer deadNodeReconcileLocalMu.Unlock()
	if at, ok := deadNodeReconcileRetrySince[member]; ok {
		return at
	}
	now := time.Now()
	deadNodeReconcileRetrySince[member] = now
	return now
}

func forgetDeadNodeReconcileRetry(member string) {
	deadNodeReconcileLocalMu.Lock()
	delete(deadNodeReconcileRetrySince, member)
	deadNodeReconcileLocalMu.Unlock()
}

// keepDeadNodeReconcileRetriesOnlyFor 丢掉已不在任务表里的计时(别的领导者做完了,或合服清掉了),表不会无限增长。
func keepDeadNodeReconcileRetriesOnlyFor(tasks []deadNodeReconcileTask) {
	listed := make(map[string]struct{}, len(tasks))
	for _, task := range tasks {
		listed[task.member] = struct{}{}
	}
	deadNodeReconcileLocalMu.Lock()
	defer deadNodeReconcileLocalMu.Unlock()
	for member := range deadNodeReconcileRetrySince {
		if _, ok := listed[member]; !ok {
			delete(deadNodeReconcileRetrySince, member)
		}
	}
}

// resetDeadNodeReconcileLocalState 清掉本进程的重试计时,并把上次发布过非 0 的 zone 写回 0(本副本不再是领导者)。
// 跟随者的常态是两张表都空,每拍只付一次加锁。
func resetDeadNodeReconcileLocalState() {
	deadNodeReconcileLocalMu.Lock()
	if len(deadNodeReconcileRetrySince) == 0 && len(deadNodeReconcilePublishedZones) == 0 {
		deadNodeReconcileLocalMu.Unlock()
		return
	}
	published := deadNodeReconcilePublishedZones
	deadNodeReconcileRetrySince = make(map[string]time.Time)
	deadNodeReconcilePublishedZones = make(map[uint32]struct{})
	deadNodeReconcileLocalMu.Unlock()

	for zoneID := range published {
		metrics.SetDeadNodeReconcilePending(zoneID, 0)
	}
}

// publishDeadNodeReconcilePending 把本 zone 还没收完的任务数发到 Prometheus(只由领导者调用)。任务表按 zone 之后
// counts 只会有本 zone;本 zone 恒发一次(没有任务时写 0)。上一次发布过非 0、这次没有任务的 zone 一并写 0,
// 否则仪表盘会一直停在最后一个非零值上(换 ZoneId 重启的进程不会遇到,留着只为不依赖这一点)。
func publishDeadNodeReconcilePending(svcCtx *svc.ServiceContext, counts map[uint32]int) {
	deadNodeReconcileLocalMu.Lock()
	drained := make([]uint32, 0, len(deadNodeReconcilePublishedZones))
	for zoneID := range deadNodeReconcilePublishedZones {
		if counts[zoneID] == 0 {
			drained = append(drained, zoneID)
		}
	}
	deadNodeReconcilePublishedZones = make(map[uint32]struct{}, len(counts))
	for zoneID, n := range counts {
		if n > 0 {
			deadNodeReconcilePublishedZones[zoneID] = struct{}{}
		}
	}
	deadNodeReconcileLocalMu.Unlock()

	for _, zoneID := range orchestratedZones(svcCtx) {
		metrics.SetDeadNodeReconcilePending(zoneID, counts[zoneID])
	}
	for _, zoneID := range drained {
		metrics.SetDeadNodeReconcilePending(zoneID, 0)
	}
	for zoneID, n := range counts {
		metrics.SetDeadNodeReconcilePending(zoneID, n)
	}
}

// stringsAsArgs 把字符串切片转成 go-zero 变参命令(Srem / Eval)要的 []any。
func stringsAsArgs(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// ---------------------------------------------------------------------------
// 推迟的摘除(GO-6):death_at 没落地,就不把节点摘出负载集
// ---------------------------------------------------------------------------
//
// 以前 removeNodeFromRedis / fullSync 陈旧清扫在 markNodeDeath 的 Setex 失败后照样 ZREM,
// 读者于是看到「不在负载集(IsNodeAlive=false)+ 没有 death_at(屏障放行)」,在老节点
// 冲突关停的有界疏散(15s)还在 SavePlayer 时就改派 / 接管 —— 老节点的最后一笔进度会被
// TiDB 写权行拒掉而丢失,这是再入屏障要避免的(迟到写的正确性由写权行兜底,只丢不盖)。
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
//     (按存活处理,方向 fail-closed),放弃后由下一次 fullSync 在负载集里重扫到它。2026-10-22 起放弃时
//     主动请求一次 fullSync(RequestLoadReporterResync,非阻塞、合并信号),不再干等 watch 重连 / 领导权
//     变化 / 进程重启:此前死节点会一直留在负载集,名下玩家一直回 18(cross-zone-scene-travel.md §13.4 复审
//     建议)。重扫若仍摘不掉,会以新的首次观察时刻再推迟,每个卡住的节点最多每个放弃期限触发一次 fullSync。
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
	// 判死时刻的场景快照(收尾范围,R1-I2)。推迟结束或节点重新注册时写进收尾任务;
	// 没抄到时 scenesCaptured=false,摘除时在摘负载集的同一次脚本里从反向索引现抄。
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
		logx.Errorf("[ReentryBarrier][DeferDetach] Still not removed from load set; retaining initial deferral time and snapshot (waited %v): zone=%d node=%s err=%v",
			time.Since(existing.firstObservedAt), zoneID, nodeID, cause)
		return
	}

	task := deferredNodeDetach{entry: entry, firstObservedAt: time.Now()}
	scenes, snapErr := svcCtx.Redis.Smembers(nodeScenesKey(zoneID, nodeID))
	if snapErr != nil {
		// 快照没抄到不影响推迟本身;摘除时退回从反向索引现抄(与摘负载集同一次脚本,失败就继续推迟)。
		logx.Errorf("[ReentryBarrier][DeferDetach] Failed to capture dead node scene snapshot; will reread at removal: zone=%d node=%s err=%v",
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
	logx.Errorf("[ReentryBarrier][DeferDetach] Node declared dead but death_at not persisted or load-set removal failed; retaining in load set (treated as alive), retrying each tick: zone=%d node=%s scenes=%d captured=%v pending=%d err=%v",
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
// 快照抄到了就写进收尾任务(只交快照、不碰负载集),与旧代码「DELETE 入队 → PUT 清 death_at → 下一拍 drain
// 收尾旧化身的孤儿实例并删计数」时序一致(含其既有取舍:drain 按「没有 death_at」立即收尾,会删掉新化身已开始
// 累计的计数,有界)。以下三种情况只打 Errorf,旧化身的孤儿实例映射与计数留到该节点下一次死亡时收:
// 快照没抄到;本副本已不是领导者(R1-I4,跟随者不写任务表;推迟表只在领导者上有内容,只有刚降级的副本会走到);
// 写任务失败。
func cancelDeferredNodeDetachOnReregister(svcCtx *svc.ServiceContext, zoneID uint32, nodeID string) {
	key := pendingDeadNodeKey(zoneID, nodeID)
	deferredNodeDetachMu.Lock()
	task, ok := deferredNodeDetaches[key]
	delete(deferredNodeDetaches, key)
	deferredNodeDetachMu.Unlock()
	if !ok {
		return
	}

	metrics.ObserveNodeDetachDeferred(zoneID, metrics.NodeDetachOutcomeDropped)
	if !task.scenesCaptured {
		logx.Errorf("[ReentryBarrier][DeferDetach] Node registered again during deferred removal; abandoning removal, death-time snapshot unavailable, old incarnation orphan instances and counts retained until next node death: zone=%d node=%s",
			zoneID, nodeID)
		return
	}
	if !isLeader() {
		logx.Errorf("[ReentryBarrier][DeferDetach] Node registered again during deferred removal, but this replica is no longer leader; dropping death-time snapshot, old incarnation orphan instances and counts retained until next node death: zone=%d node=%s scenes=%d",
			zoneID, nodeID, len(task.scenes))
		return
	}
	logx.Infof("[ReentryBarrier][DeferDetach] Node registered again during deferred removal; abandoning removal, passing death-time snapshot to cleanup task: zone=%d node=%s scenes=%d",
		zoneID, nodeID, len(task.scenes))
	if _, err := enqueueDeadNodeReconcile(svcCtx, task.entry, false, task.scenes, true); err != nil {
		logx.Errorf("[ReentryBarrier][DeferDetach] Failed to hand death-time snapshot to cleanup task; old incarnation orphan instances and counts retained until next node death: %v", err)
	}
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
				logx.Infof("[ReentryBarrier][DeferDetach] Replica no longer leader; abandoning deferred removal (Redis untouched, new leader will rescan node in load set): zone=%d node=%s",
					z, n)
			}
			continue
		}
		// 漏了 PUT 也不能给活节点打死亡标记:它已经回到注册表,按重新注册处理。
		if knownNodeIdentityMatchCount(z, n) > 0 {
			cancelDeferredNodeDetachOnReregister(svcCtx, z, n)
			continue
		}

		waited := time.Since(task.firstObservedAt)
		// ZREM 一直失败时每拍都会重写 death_at:屏障只会往后推(保守),不会提前失效。
		markErr := markNodeDeath(svcCtx, z, n)
		if markErr != nil && waited < barrier {
			logx.Errorf("[ReentryBarrier][DeferDetach] Still unable to write death_at, waited %v / barrier %v; retrying next tick: zone=%d node=%s err=%v",
				waited, barrier, z, n, markErr)
			continue
		}
		if err := detachDeadNode(svcCtx, task.entry, task.scenes, task.scenesCaptured); err != nil {
			if waited >= deferredNodeDetachGiveUpAfter {
				if forgetDeferredNodeDetach(z, n) {
					metrics.ObserveNodeDetachDeferred(z, metrics.NodeDetachOutcomeAbandoned)
				}
				logx.Errorf("[ReentryBarrier][DeferDetach] Still unable to remove from load set after %v since first attempt; abandoning, node retained as alive for next fullSync rescan: zone=%d node=%s err=%v",
					waited, z, n, err)
				// 让那次 fullSync 马上来,而不是等 watch 中断 / 换领导者 / 重启(见上面「有界性」)。
				RequestLoadReporterResync()
				continue
			}
			logx.Errorf("[ReentryBarrier][DeferDetach] Failed to remove from load set; retrying next tick (waited %v / abandonment deadline %v): zone=%d node=%s err=%v",
				waited, deferredNodeDetachGiveUpAfter, z, n, err)
			continue
		}

		forgetDeferredNodeDetach(z, n)
		if markErr == nil {
			metrics.ObserveNodeDetachDeferred(z, metrics.NodeDetachOutcomeRecovered)
			logx.Infof("[ReentryBarrier][DeferDetach] death_at backfill succeeded; removed from load set (waited %v): zone=%d node=%s",
				waited, z, n)
		} else {
			metrics.ObserveNodeDetachDeferred(z, metrics.NodeDetachOutcomeExpired)
			logx.Errorf("[ReentryBarrier][DeferDetach] Still unable to write death_at after %v since first attempt >= barrier %v; removing without marker (IsNodeAlive true during deferral, this leader's CanReclaimDeadNode always rejects, full barrier covered): zone=%d node=%s err=%v",
				waited, barrier, z, n, markErr)
		}
		done++
	}
	return done
}
