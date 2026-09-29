package kafka

import (
	"context"
	"db/internal/logic/pkg/proto_sql"
	"db/internal/metrics"
	"errors"
	"fmt"
	db_proto "proto/db"
	"shared/placement"
	"strconv"
	"time"

	"github.com/luyuancpp/proto2mysql"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// ───────────────────────── 按玩家存储落点选库(player-storage-placement.md §6.2 / §6.3)─────────────────────────
//
// 「玩家属于哪个区」(home_zone,player:zone:{id})与「玩家主数据在哪个库」(落点记录,player:placement:{id})
// 已经拆开:合服只改 home_zone,搬库只改落点记录。选库只在这里发生,只看处理那一刻的记录(P-3)——
// 生产者消息里没有任何落点信息,也不信任它。db_task 的 Key 恒为 player_id(C++ SavePlayerToRedis 与
// login 预加载两个生产者的契约),所以两把键都按 Key 拼。
//
// 一条任务的落点生命周期:
//
//	选库(routeTask,一次 MGET 读两把键)→ 在选中的库上执行 SQL → 复核(confirm,再 MGET 一次)
//	→ 共享缓存回写 / 读结果发布 → (写)游标与 applied epoch
//
// 复核卡在「SQL 已执行、别人还没看到结果」这一个点上,关掉「选库之后、落库之前记录被搬库工具改掉」的
// TOCTOU:搬库冻结之后才落进源库的写,复核必然看到冻结或已切换,于是不标记游标、稍后重投到新库。
// 正确性因此不依赖排序锁的 TTL(§6.3 末段)。

var (
	// errPlacementFrozen:玩家的落点记录处于搬库冻结态,写任务延后,不耗重试次数(§6.2)。
	errPlacementFrozen = errors.New("task deferred because the player's placement record is frozen for relocation")
	// errPlacementStoreUnavailable:落点库暂时打不开(不被放行 / 库不存在 / 连不上 / schema 闸未过)。
	// 写任务不耗重试次数(§6.1),读任务照常耗(见 retryBudgetExempt)。
	errPlacementStoreUnavailable = errors.New("task deferred because its placement store is unavailable")
	// errPlacementMoved:落库后复核发现路由变了(或写遇到冻结),本次结果作废重试,不耗重试次数(§6.3)。
	errPlacementMoved = errors.New("placement changed between store selection and the post-write recheck")
)

// StoreResolver 按落点编号给出已打开的库(生产实现是 proto_sql.StoreRegistry)。
// 实现必须并发安全;返回的错误一律按「落点库暂不可用」处理。
type StoreResolver interface {
	Store(ctx context.Context, storageID uint32) (*proto_sql.GameDB, error)
}

// placementRouter 是选库所需的全部依赖,构造后只读,所有 worker 共用。
type placementRouter struct {
	// rc 是落点记录所在的 Redis(Placement.Redis,缺省复用 RedisClient)。与排序锁 / 游标 / 缓存用的
	// worker.redisClient 分开:两者在部署上可以是不同实例。
	rc redis.Cmdable
	// zone 是本进程的 zone(config ZoneId),即 §6.2 的 Z:本进程只消费 db_task_zone_{Z}。
	zone     uint32
	required bool
	stores   StoreResolver
}

// placementSnapshot 是一次 MGET 读到的两把键。
type placementSnapshot struct {
	rec         placement.Record
	recPresent  bool
	home        uint32
	homePresent bool
}

// route 返回快照指向的有效落点(§1):有记录用记录,无记录用 home_zone,都没有用本进程 zone。
func (s placementSnapshot) route(zone uint32) uint32 {
	return placement.EffectiveStorage(s.rec, s.recPresent, s.home, s.homePresent, zone)
}

func (s placementSnapshot) String() string {
	record, home := "-", "-"
	if s.recPresent {
		record = s.rec.Encode()
	}
	if s.homePresent {
		home = strconv.FormatUint(uint64(s.home), 10)
	}
	return "record=" + record + " home=" + home
}

// lookup 一次 MGET 读 player:placement:{key} 与 player:zone:{key}。
// 查询失败或任一值畸形都返回错误,调用方 fail-closed —— 畸形**不得**当作缺席:缺席会让玩家回落
// home_zone / 本进程 zone 选库,正是落点记录要防的错库写。
func (r *placementRouter) lookup(ctx context.Context, key uint64) (placementSnapshot, error) {
	vals, err := r.rc.MGet(ctx, placement.Key(key), placement.HomeZoneKey(key)).Result()
	if err != nil {
		return placementSnapshot{}, fmt.Errorf("MGET placement/home_zone for key %d: %w", key, err)
	}
	snap, err := parsePlacementSnapshot(vals)
	if err != nil {
		return placementSnapshot{}, fmt.Errorf("key %d: %w", key, err)
	}
	return snap, nil
}

// parsePlacementSnapshot 解析 MGET 的两个值(nil = 键不存在)。
func parsePlacementSnapshot(vals []interface{}) (placementSnapshot, error) {
	if len(vals) != 2 {
		return placementSnapshot{}, fmt.Errorf("MGET returned %d values, want 2", len(vals))
	}
	var snap placementSnapshot
	if raw, present, err := mgetString(vals[0]); err != nil {
		return placementSnapshot{}, err
	} else if present {
		rec, err := placement.Parse(raw)
		if err != nil {
			return placementSnapshot{}, err
		}
		snap.rec, snap.recPresent = rec, true
	}
	if raw, present, err := mgetString(vals[1]); err != nil {
		return placementSnapshot{}, err
	} else if present {
		home, ok, err := placement.ParseHomeZone(raw)
		if err != nil {
			return placementSnapshot{}, err
		}
		// home_zone 必须能当 zone 库的落点编号用(无记录时就落它):>= 1000000 派生出的是
		// player_store_* 而不是这个 zone 的库,按畸形 fail-closed。
		if ok && !placement.IsZoneStorage(home) {
			return placementSnapshot{}, fmt.Errorf("%w: home zone %d is outside the zone storage range 1..%d",
				placement.ErrMalformed, home, placement.MaxZoneStorageID)
		}
		snap.home, snap.homePresent = home, ok
	}
	return snap, nil
}

// mgetString 把 MGET 的一个元素转成字符串;nil = 键不存在。
func mgetString(v interface{}) (string, bool, error) {
	switch s := v.(type) {
	case nil:
		return "", false, nil
	case string:
		return s, true, nil
	default:
		return "", false, fmt.Errorf("unexpected MGET element type %T", v)
	}
}

// placementDecision 是一次选库判定。outcome 直接是 metrics 的 label 取值;
// storageID 只在 placed / home / legacy 时有意义(frozen_deferred 时记下冻结记录的库,仅供日志)。
type placementDecision struct {
	outcome   string
	storageID uint32
}

// decideWritePlacement 按 §6.2 写任务表逐格判定(纯函数,不碰 Redis / metrics / 日志)。
// 分支顺序就是表的行序,不能调换:
//   - stale_topic 先于冻结:过期 topic 的写即使赶上搬库也不能落任何库(P-6)——游标按 topic 分,
//     跨 topic 的两条写没有可比的顺序,放它落库就可能盖掉 home 变化之后经新 topic 落下的更新版本;
//   - 冻结先于 placed:冻结期间写一律延后,不管落点指向哪。
func decideWritePlacement(snap placementSnapshot, zone uint32, required bool) placementDecision {
	switch {
	case snap.homePresent && snap.home != zone:
		return placementDecision{outcome: metrics.PlacementStaleTopic}
	case snap.recPresent && snap.rec.Frozen:
		return placementDecision{outcome: metrics.PlacementFrozenDeferred, storageID: snap.rec.StorageID}
	case snap.recPresent:
		return placementDecision{outcome: metrics.PlacementPlaced, storageID: snap.rec.StorageID}
	case required:
		return placementDecision{outcome: metrics.PlacementMissingRequired}
	case snap.homePresent:
		// 走到这里 home 必然 == zone(不等的已在第一格进死信)。
		return placementDecision{outcome: metrics.PlacementHome, storageID: snap.home}
	default:
		return placementDecision{outcome: metrics.PlacementLegacy, storageID: zone}
	}
}

// decideReadPlacement 按 §6.2 读规则判定。读不看冻结(冻结期间源库不再变化,读它得到的正是将被拷走的
// 内容),也不适用 P-6:home 与本进程 zone 不同是常态(访客从别区登录,预加载发往登录所在 zone 的 topic)。
func decideReadPlacement(snap placementSnapshot, zone uint32, required bool) placementDecision {
	switch {
	case snap.recPresent:
		return placementDecision{outcome: metrics.PlacementPlaced, storageID: snap.rec.StorageID}
	case required:
		return placementDecision{outcome: metrics.PlacementMissingRequired}
	case snap.homePresent:
		return placementDecision{outcome: metrics.PlacementHome, storageID: snap.home}
	default:
		return placementDecision{outcome: metrics.PlacementLegacy, storageID: zone}
	}
}

// sameRoute 判定两次读到的落点是否仍指向同一个库的同一个版本。
//
//   - 记录出现 / 消失:不同(§6.3 第三行);
//   - 都有记录:比 (storage_id, version),不看冻结 —— 冻结 / 解冻不改版本(P-2),单独由调用方判;
//   - 都没有记录:比有效落点(home_zone ?? 本进程 zone)。设计 §6.3 只写了「再读一次记录」,这里连 home_zone
//     一起比:无记录玩家的库由 home_zone 决定,copy 模式合服在 SQL 与复核之间把行拷走并改 home 时,
//     只比记录会判「无记录 → 无记录」通过、标记游标,而这笔写留在了已不再是真源的源区库里。
func sameRoute(selected, current placementSnapshot, zone uint32) bool {
	if selected.recPresent != current.recPresent {
		return false
	}
	if selected.recPresent {
		return selected.rec.SameRoute(current.rec)
	}
	return selected.route(zone) == current.route(zone)
}

// recheckPasses 是 §6.3 的复核表:
//
//	选库时        复核时              写                    读
//	稳定(s,v)     稳定(s,v)           通过                  通过
//	稳定(s,v)     冻结(s,v)           重试,不标记游标        通过(冻结期间源库不变)
//	任意          版本/库变化、记录出现/消失   重试,不标记游标        丢弃结果重试
//	无记录        无记录(有效落点不变)  通过                  通过
//
// 写在复核时看到冻结必须重试:搬库工具可能已在排序锁过期后拷完源库,不能确定这笔写被拷走了。
// 重投时冻结中 → 延后;已切换 → 落新库;源库那份按 P-1 是冷副本。整行 REPLACE,重复落地幂等。
func recheckPasses(op string, selected, current placementSnapshot, zone uint32) bool {
	if !sameRoute(selected, current, zone) {
		return false
	}
	if op == "write" && current.recPresent && current.rec.Frozen {
		return false
	}
	return true
}

// placementRoute 是一条任务选好的库与选库时的快照,在 SQL 执行之后用于复核。
type placementRoute struct {
	router    *placementRouter
	op        string
	key       uint64
	storageID uint32
	store     *proto2mysql.DB
	selected  placementSnapshot
}

// confirm 执行落库后复核(§6.3)。调用点固定在 runDBOp 里「SQL 之后、共享缓存回写 / 读结果发布之前」,
// 游标与 applied epoch 又在 runDBOp 返回之后才推进,所以复核不通过时这三样都不会动。
//
// 复核读失败按普通 Redis 故障处理(耗重试次数):此时 SQL 已执行,但没有复核就不能确认它落在真源上。
func (r *placementRoute) confirm(ctx context.Context) error {
	current, err := r.router.lookup(ctx, r.key)
	if err != nil {
		metrics.CountPlacementGuard(r.op, metrics.PlacementLookupError)
		return fmt.Errorf("placement recheck unavailable: %w", err)
	}
	if recheckPasses(r.op, r.selected, current, r.router.zone) {
		return nil
	}
	metrics.CountPlacementGuard(r.op, metrics.PlacementRecheckMoved)
	return fmt.Errorf("%w: key=%d op=%s executed_on_storage=%d selected{%s} now{%s}",
		errPlacementMoved, r.key, r.op, r.storageID, r.selected, current)
}

// routeTask 为一条 read / write 任务选库(§6.2)。
//
// 返回 (route, true) 表示继续执行;返回 false 表示任务已被终态处理(进死信 / 延后重试 / 读失败回执),
// 调用方直接 return。写任务必须在排序锁、游标守卫、owner_epoch 守卫之后调用:被判为过期的写不值得再花
// 一次 MGET;选库结果必须在持锁期间用掉 —— 锁挡着搬库工具的「等在途写完成」(R2)。
func (w *worker) routeTask(task *workerTask, dbTask *db_proto.DBTask) (*placementRoute, bool) {
	op := dbTask.Op
	router := w.placement
	if router == nil || router.rc == nil || router.stores == nil {
		// 装配缺失是编程错误。fail-closed 成可重试错误(耗重试次数,最终进死信由人处理),绝不回落到某个
		// 默认库:回落就是旧版「按进程 zone 选库」,会把被钉到别处的玩家写错库。
		w.deferFailedTask(task, dbTask, errors.New("placement router not configured"))
		return nil, false
	}

	snap, err := router.lookup(w.ctx, dbTask.Key)
	if err != nil {
		metrics.CountPlacementGuard(op, metrics.PlacementLookupError)
		w.deferFailedTask(task, dbTask, fmt.Errorf("placement lookup failed: %w", err))
		return nil, false
	}
	var decision placementDecision
	if op == "write" {
		decision = decideWritePlacement(snap, router.zone, router.required)
	} else {
		decision = decideReadPlacement(snap, router.zone, router.required)
	}
	metrics.CountPlacementGuard(op, decision.outcome)

	switch decision.outcome {
	case metrics.PlacementStaleTopic:
		w.quarantineOrderingConflict(task, dbTask, fmt.Sprintf(
			"PLACEMENT stale_topic (P-6): topic zone %d != player home_zone %d; the write predates a home_zone change and must not land in any database (%s)",
			router.zone, snap.home, snap))
		return nil, false
	case metrics.PlacementFrozenDeferred:
		// 搬库期间每条写每轮重试都会走到这里,只打 DEBUG;冻结长时间不解除看 frozen_deferred 计数告警。
		logx.Debugf("placement frozen, write deferred: key=%d taskID=%s msgType=%s %s",
			dbTask.Key, dbTask.TaskId, dbTask.MsgType, snap)
		w.deferFailedTask(task, dbTask, fmt.Errorf("%w: key=%d msgType=%s %s",
			errPlacementFrozen, dbTask.Key, dbTask.MsgType, snap))
		return nil, false
	case metrics.PlacementMissingRequired:
		reason := fmt.Sprintf("PLACEMENT missing_required: player %d has no player:placement record and Placement.Required=true (%s)",
			dbTask.Key, snap)
		if op == "write" {
			w.quarantineOrderingConflict(task, dbTask, reason)
		} else {
			w.failReadTask(task, dbTask, reason)
		}
		return nil, false
	}

	store, err := router.stores.Store(w.ctx, decision.storageID)
	if err == nil && (store == nil || store.SqlModel == nil) {
		err = errors.New("store resolver returned no store")
	}
	if err != nil {
		w.deferFailedTask(task, dbTask, fmt.Errorf("%w: storage=%d key=%d msgType=%s: %w",
			errPlacementStoreUnavailable, decision.storageID, dbTask.Key, dbTask.MsgType, err))
		return nil, false
	}
	return &placementRoute{
		router:    router,
		op:        op,
		key:       dbTask.Key,
		storageID: decision.storageID,
		store:     store.SqlModel,
		selected:  snap,
	}, true
}

// failReadTask 以失败回执结束一条读任务(Placement.Required=true 且无记录,§6.2「任务失败」)。
//
// 读不改数据,进死信没有可保全的东西,重试也等不来记录;立即给等待方(login 预加载)回 Success=false,
// 让它秒级失败而不是等满超时。回执写失败时按普通 Redis 故障延后重试。
func (w *worker) failReadTask(task *workerTask, dbTask *db_proto.DBTask, reason string) {
	if dbTask.TaskId != "" {
		failure := &db_proto.TaskResult{Success: false, Error: reason}
		if err := publishTaskResult(w.ctx, w.redisClient, dbTask.TaskId, failure); err != nil {
			w.deferFailedTask(task, dbTask, fmt.Errorf("publish placement failure result: %w", err))
			return
		}
	}
	logx.Errorf("%s: read answered with a failure result: taskID=%s msgType=%s", reason, dbTask.TaskId, dbTask.MsgType)
	if task.fromRetry {
		if err := ackRetryReceipt(w.ctx, w.redisClient, w.retryProcessingKey, task.retryReceipt); err != nil {
			logx.Errorf("ack rejected read retry receipt failed; it remains recoverable: taskID=%s err=%v", dbTask.TaskId, err)
		}
	}
	ackKafkaTask(task)
}

// retryBudgetExempt 判定一次延后是否不消耗有限的业务重试次数(retryMaxTimes)。
//
// 豁免的原因都不是「这次落库失败了」,而是「现在还不该落」:排序锁被上一任持有者占着、搬库冻结中、
// 复核发现落点刚变、落点库暂时打不开。它们只会随外部状态变化自行消失;耗预算会把一笔完好的存盘在
// 几次重试后推进没有消费者的死信队列 —— 等于丢盘。
//
// 落点库打不开只对写豁免:读不改数据,等待方(login 预加载)早已超时,库长期打不开时无限期重排只会让
// 每次登录尝试都往重试队列里加一条永远排不完的读,挤占真正需要重试的写。读照常耗预算,几次后进死信。
func retryBudgetExempt(dbTask *db_proto.DBTask, cause error) bool {
	switch {
	case errors.Is(cause, errOrderingLockBusy),
		errors.Is(cause, errPlacementFrozen),
		errors.Is(cause, errPlacementMoved):
		return true
	case errors.Is(cause, errPlacementStoreUnavailable):
		return dbTask.Op == "write"
	}
	return false
}

// 能力标记是**心跳**,不是一次性登记(§4.3):
//
//   - 标记只能证明「最近 PlacementCapabilityTTL 之内,这个 zone 有一个新版 go/db 在续写」。永久标记做不到这一点:
//     §13 允许第 4 步(pin 合服 / relocate / PinOnCreate)之前独立回退任何组件,回退后的旧版 go/db 不认识这把键,
//     永久标记会一直替它作证,工具据此放行,旧版按进程 zone 选库,把被钉走 / 已搬走的玩家写进非真源库,且零报错。
//   - 带 TTL 后旧版不续写,标记在 TTL 内自然消失;进程被 kill -9 / 崩溃同理。
//   - 进程退出时**不主动 DEL**:同一 zone 可能有多个副本,一个副本退出不代表该 zone 失去能力;交给 TTL 收尾。
//   - 管不住的窗口(部署纪律,见设计 §13 / runbook):滚动发布新旧 Pod 重叠期间标记已存在,回退后 TTL 到期之前
//     标记仍残留。go/db 发布完成(新 Pod 全部 Ready、旧 Pod 全部退出)之前、回退后 TTL 过去或手工
//     DEL db:capability:zone:{Z} 之前,不得执行 pin 合服、relocate 或打开 PinOnCreate。
//
// TTL 取续写间隔的 3 倍:允许连续两次续写失败(Redis 抖动)而标记不掉;真掉了只会让工具拒绝动手(安全方向),
// 下一次续写成功即恢复,不需要重启进程。
const (
	// PlacementCapabilityTTL 是能力标记的过期时间。
	PlacementCapabilityTTL = 90 * time.Second
	// PlacementCapabilityRefreshInterval 是续写间隔。
	PlacementCapabilityRefreshInterval = 30 * time.Second
	// placementCapabilityWriteTimeout 是单次续写的预算,必须小于续写间隔,免得一次卡住的 SET 吞掉下一轮。
	placementCapabilityWriteTimeout = 5 * time.Second
)

// MarkPlacementCapability 写一次能力标记 db:capability:zone:{zone} = placement-routing-v1,过期时间
// PlacementCapabilityTTL(§4.3)。启动时同步写一次,之后由 KeepPlacementCapability 按间隔续写。
//
// 合服 pin 模式与搬库工具动手前检查相关 zone 的标记:没有标记 = 那个 zone 此刻没有新版 go/db 在续写
// (没升级、已回退、或整个 zone 的 go/db 都停了),工具拒绝执行。值描述的是「这个 zone 正在跑的 go/db
// 具备的能力」,所以每次直接覆盖为本版本的常量;能力升级时在 shared/placement 加新常量,不改旧值。
func MarkPlacementCapability(ctx context.Context, rc redis.Cmdable, zone uint32) error {
	if rc == nil {
		return errors.New("placement capability needs a Redis client")
	}
	if zone == 0 {
		return errors.New("placement capability needs a zone > 0")
	}
	return rc.Set(ctx, placement.CapabilityKey(zone), placement.CapabilityRoutingV1, PlacementCapabilityTTL).Err()
}

// KeepPlacementCapability 每 PlacementCapabilityRefreshInterval 续写一次能力标记,阻塞到 ctx 取消。
//
// 由 main 在启动期 MarkPlacementCapability 之后用独立 goroutine 启动,ctx 在进程退出时取消。续写失败打 ERROR
// (每个间隔至多一条),恢复后打一条 INFO;不重试、不退出 —— 标记掉了的后果只是工具拒绝动手。
func KeepPlacementCapability(ctx context.Context, rc redis.Cmdable, zone uint32) {
	ticker := time.NewTicker(PlacementCapabilityRefreshInterval)
	defer ticker.Stop()
	failing := false
	runPlacementCapabilityHeartbeat(ctx, rc, zone, ticker.C, func(err error) {
		if err != nil {
			failing = true
			logx.Errorf("[placement] 能力标记续写失败(标记过期后合服 pin / 搬库工具会拒绝对本 zone 动手,Redis 恢复后自动补写): key=%s err=%v",
				placement.CapabilityKey(zone), err)
			return
		}
		if failing {
			failing = false
			logx.Infof("[placement] capability marker refresh recovered: %s", placement.CapabilityKey(zone))
		}
	})
}

// runPlacementCapabilityHeartbeat 每收到一个 tick 续写一次标记,把结果(成功为 nil)交给 report;
// ctx 取消或 ticks 关闭时返回。report 与循环在同一 goroutine 里调用。ctx 取消导致的那次续写失败不上报。
// tick 由调用方给出,测试可以逐拍驱动,不依赖墙钟。
func runPlacementCapabilityHeartbeat(ctx context.Context, rc redis.Cmdable, zone uint32,
	ticks <-chan time.Time, report func(error)) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			writeCtx, cancel := context.WithTimeout(ctx, placementCapabilityWriteTimeout)
			err := MarkPlacementCapability(writeCtx, rc, zone)
			cancel()
			if ctx.Err() != nil {
				return
			}
			report(err)
		}
	}
}
