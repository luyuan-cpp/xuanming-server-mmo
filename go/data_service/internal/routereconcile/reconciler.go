// Package routereconcile 是 db_first 下玩家归属 / 落点映射的对账 leader(D1 第 1b-2 批)。
//
// 依据:docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §2(M1–M5)、§4.2–§4.5、§13.1(与正文冲突时以 §13.1 为准);
// 底稿 spec_D1_batch1b_scout.md「plan」1b-2 各项与「# VERDICT」;提案 docs/design/player-route-mapping-durability-proposal.md §5.4、§11.2。
//
// 职责(全服唯一一个 leader,由全局库 route_reconcile 的租约选出):
//   - 补发「已提交、未发布」的孤儿行(增量推送),Redis 重启 / 切主之后回看修补,Redis 整体丢失之后全量预热;
//   - 每拍检测库回档；命中后关闭本 boot 接纳并交独立维护 owner，normal 不修改 writable;
//   - 维护合走标记 merge:merged_into:{src}(只增);
//   - ArmReadyMarker=true 时在完整预热 / 修补之后挂就绪标记 mapping:route:ready(预热令牌 M2 fencing + 同连接 WAITAOF),
//     false 时每拍撤掉现存标记(M4);每拍用读方同一段脚本自检并导出 verdict。
//
// redis 模式下不构造(svc.newRouteReconciler / New 拒绝):那时 Redis 才是提交点,库只是 P1 时刻的快照。
//
// 并发结构:一个决策循环(Run → tickOnce)独占全部可变状态;一个任务 goroutine,同一时刻最多一个任务(预热 / 修补 / 追平 / 增量);
// 任务只读库、推 Redis、复读库,结果经 channel 交回循环,由循环统一改状态、挂标记(挂标记的全部前置条件都在循环手里)。
// 「本地租约有效至」用 atomic 发布给任务 goroutine,任务每批之前检查,过点即中止。
//
// 每拍顺序(规格 §4.3,叠加 §13.1):
//  0. 收任务结果(完成 → 记 cvm 起点 S、追平置 VI、预热 / 修补产出挂标记候选;中止 → 修补可重排);
//  1. 租约:leader 每 RenewInterval 续约;非 leader 只 Peek,看起来过期才 Renew(副本多时不抢同一行锁);未持有 → 结束本拍;
//  2. 读身份(INFO server / replication / stats + CONFIG GET maxmemory-policy / enable-debug-command);出错或 role≠master 记
//     redis_error,连续超过 RedisFailureReleaseMs 让出租约;
//  3. 读三把控制键(ready / warm / watermark,一次 MGET + TYPE ready + TYPE watermark);WRONGTYPE 无条件 DEL、畸形 CAD、版本更新只记日志
//     (水位键:不是 string 的 DEL 后当拍重建,畸形的记 ERROR 后提交时覆盖,版本更新的记 ERROR,见 settleWatermarkShape);
//     三把都不在而本任期见过 = 被 rewarm / FLUSH 清掉:解除 armingBlocked、重取 evicted_keys 基线;
//     「副本对账到哪」的证据没了(三把都不在,或 Arm=false 下水位键不在)→ cvm 作废(本拍 AdvanceReconcile 写 0,见下面「偏离规格」),
//     本拍不起增量;Arm=false 时撤现存标记(M4,只要 Redis 与仍有效的本地租约,所以放在库内部分之前);
//  4. AdvanceReconcile(水位键 + 世代 + 容差;cvm 按完成任务的起点写);冻结 → 停推送、身份不符 / 淘汰异常时撤标记;
//     未冻结 → 标记世代 ≠ 库世代即冻结(scout D4 保留的那条),否则单调推进水位键(GET → 判 → CAS / SET NX);
//  5. 淘汰策略 / evicted_keys / enable-debug-command 自检:异常时撤标记、禁止挂标记;
//  6. (已删:合服围栏镜像对账。别名合服不再有围栏,其余步骤编号不变)
//  7. Arm=false:VI 为空(或身份变了)排追平(catch_up,设新令牌,起点 max(0, 库里的 cvm − RepairLookbackMs));
//  8. Arm=true:标记在且身份相等 → VI = 当前身份,CAS 把标记水位推进到 cvm;标记在而身份不等 → CAD、排修补(起点
//     max(0, min(标记水位, cvm) − RepairLookbackMs));标记不在 → **一律**全量预热(armingBlocked 除外),预热 / 修补完成且无阻塞
//     → 挂标记;
//  9. 距上次增量满 IncrementalInterval 且没有任务 → 排增量(起点 max(0, cvm − IncrementalOverlapMs),并复读未决行);
//  10. 自检(RouteReadyReadScript 不带映射键)并写 leader 观测型 gauge。
//
// 第 5、6、7 / 8 步开始前与写水位键、SET 预热令牌之前各复核一次本地租约(核实意见 LR-10):一拍可能被慢 Redis / 慢库拖过本地截止,
// 之后的 Redis 写不受库侧租约谓词保护;过点即降级、结束本拍。
//
// cache_verified_ms(cvm)的语义(规格 §4.2 must-2):「updated_at_ms < cvm − IncrementalOverlapMs 的行都已推过,且期间 Redis 身份未变」。
// 一个任务从它开始时的库时钟 S 完整跑完、首尾身份相同(带令牌的任务还要令牌仍是自己的)才把 cvm 写成 S;新 leader 的首次增量从
// max(0, cvm − Overlap) 开始;长任务结束后的首次增量从该任务的 S − Overlap 开始;被限速拖长的增量只推进到它自己的 S。
//
// 偏离规格(记在交付说明):
//   - 「副本对账到哪」的证据丢了就把 cvm 写成 0(核实意见 A1-1 / LR-2,2026-10-20):水位键由每拍的 leader 维护,它不在说明 Redis 是
//     空实例 / 被 FLUSH / 被 rewarm / 键被删或被淘汰;三把控制键都不在同理。这时「updated_at_ms < cvm − Overlap 的行都已推进缓存」
//     不再成立,cvm 只能是 0。写进库(而不是只在 leader 内存里记「要全量」)是为了让这个结论跨拍、跨任务失败、跨换届与滚动重启保留:
//     旧做法只在发现缺席的那一拍按 0 起追平,同拍又 SET NX 重建水位键,追平中途失败、那一拍恰有增量在跑、或追平途中换 leader,
//     下一次追平就退化成 cvm − Lookback,被清掉的映射再也补不回。在途任务除「带令牌、从 0 起」的(FLUSH / rewarm 会换掉令牌,
//     它们由令牌自证)之外一律取消,已收的 pendingCvm 丢弃。水位键照旧当拍重建:M3、merge_zone 的 -route-source 成对核对、
//     barrierrestore 都要它在。Arm=false 下身份变化后的追平起点仍按 max(0, cvm − RepairLookbackMs)。
//     (2026-10-22 落码,见 checkCacheEvidence:不论本任期见没见过控制键都判;不是 string 的水位键不算「不在」;证据刚丢的那一拍不起增量;
//     冻结的拍库里不写 0,水位键也不重建,解冻后第一拍再判。代价:Arm=false 下有人只删了水位键也会触发一次全量增量。)
//   - Arm=false 的撤标记(M4)挪到库内部分之前(核实意见 LR-12),库故障的拍里照撤;于是 Arm=false 下不再拿将撤的标记做
//     「标记世代 ≠ 库世代」的冻结判据(Arm=false 本不该有标记,库被重建由水位键 M3 与 cache_ahead 发现)。
//   - 未决行溢出(超过 ArmingBlockedReportLimit)不再算挂标记的阻塞原因(核实意见 A1-3 / LR-3):记下的 id 全部解决后闩锁解除,
//     丢掉记不全的候选、重新全量预热;记下的还没解决时照旧闩住、不重跑预热。
//   - cache_ahead 的冻结不随任务被取消而回滚(核实意见 A1-2):push 里用 context.WithoutCancel;被取消的任务若冻结没做成,
//     证据转成 cacheAheadPending 由循环每拍重试。
//   - 合走标记镜像的冲突只记 ERROR 与 mirror_total{merged_into_conflict},不阻止挂标记(db_first 下 W9 / W10 读库里的 zone_merged_into)。
package routereconcile

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"data_service/internal/config"
	"data_service/internal/metrics"
	"data_service/internal/store"

	"shared/drainproof"
	"shared/placement"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
)

// ── 依赖(消费方定义的接口)────────────────────────────────────────────────────────

// Store 是 leader 对全局库的依赖。*store.RouteStore 天然满足(编译期断言在 reconciler_test.go)。
// 库写全部带租约谓词(store/route_reconcile.go 文件头),这里只读、只按约定调用。
type Store interface {
	PeekReconcileLease(ctx context.Context) (store.RouteLease, error)
	RenewReconcileLease(ctx context.Context, leaderID string, leaseMs uint64) (store.RouteLease, error)
	ReleaseReconcileLease(ctx context.Context, leaderID string) (bool, error)
	AdvanceReconcile(ctx context.Context, req store.ReconcileTickRequest) (store.ReconcileTickResult, error)
	FreezeByLeader(ctx context.Context, leaderID string, reason store.RouteFreezeReason, freezeTimeout time.Duration) (store.ReconcileTickResult, error)
	ScanRoutes(ctx context.Context, after store.RouteScanCursor, limit int) ([]store.RouteRow, error)
	ScanRoutesUpdatedSince(ctx context.Context, shard uint32, after store.RouteUpdatedCursor, limit int) ([]store.RouteRow, error)
	GetRoutes(ctx context.Context, ids []uint64) (map[uint64]store.RouteRow, error)
	ListMergedInto(ctx context.Context) ([]store.MergedIntoRow, error)
	DBNowMs(ctx context.Context) (uint64, error)
}

// Probe 是一次实例自检的结果(Cache.Probe:一个 pipeline 里的 INFO server / replication / stats 与两条 CONFIG GET)。
type Probe struct {
	Identity placement.RedisInstanceIdentity
	// MaxmemoryPolicy:CONFIG GET maxmemory-policy;"" = CONFIG 不可用(被禁用 / 改名,回了错误回复)。
	MaxmemoryPolicy string
	// EvictedKeys:INFO stats 的 evicted_keys(自实例启动以来的累计值);-1 = 取不到。
	EvictedKeys int64
	// DebugCommand:CONFIG GET enable-debug-command(Redis 7:no / yes / local);"" = CONFIG 不可用。
	DebugCommand string
}

// Control 是三把控制键一次读到的原值(一次 MGET + TYPE mapping:route:ready + TYPE mapping:route:watermark)。
// nil = 键不在(或不是 string)。
type Control struct {
	Ready *string
	// ReadyWrongType:就绪标记键在,但不是 string 类型(MGET 回 nil 而 TYPE 不是 none)。
	ReadyWrongType bool
	Warm           *string
	Watermark      *string
	// WatermarkWrongType:水位键在,但不是 string 类型(同上口径;核实意见 A1-8)。
	WatermarkWrongType bool
}

// present 报告三把控制键里有没有任何一把在(不是 string 的也算在)。
func (ctl Control) present() bool {
	return ctl.Ready != nil || ctl.ReadyWrongType || ctl.Warm != nil || ctl.Watermark != nil || ctl.WatermarkWrongType
}

// watermarkAbsent 报告水位键确实不在(不是 string 的不算:那是被别人写坏了,不说明副本被清空)。
func (ctl Control) watermarkAbsent() bool { return ctl.Watermark == nil && !ctl.WatermarkWrongType }

// PushOutcome 是一行推送的结局(cache_ahead 不在这里:它是 superseded 之后复读库的结论,由 leader 判)。
type PushOutcome uint8

const (
	PushSkippedCurrent PushOutcome = iota + 1
	PushApplied
	PushConfirmed
	PushSuperseded
	PushConflict
	PushLegacyConflict
	PushTombstoneSkipped
	PushDBInvalid
	PushError
)

// label 是 route_reconcile_push_total{result} 的取值。
func (o PushOutcome) label() string {
	switch o {
	case PushSkippedCurrent:
		return metrics.RouteReconcilePushSkippedCurrent
	case PushApplied:
		return metrics.RouteReconcilePushApplied
	case PushConfirmed:
		return metrics.RouteReconcilePushConfirmed
	case PushSuperseded:
		return metrics.RouteReconcilePushSuperseded
	case PushConflict:
		return metrics.RouteReconcilePushConflict
	case PushLegacyConflict:
		return metrics.RouteReconcilePushLegacyConflict
	case PushTombstoneSkipped:
		return metrics.RouteReconcilePushTombstoneSkipped
	case PushDBInvalid:
		return metrics.RouteReconcilePushDBInvalid
	default:
		return metrics.RouteReconcilePushError
	}
}

// unresolved 报告这个结局是不是「未决」(挂标记要求零未决,规格 scout D3):冲突、遗留值冲突、库行无效、逐行的错误回复。
func (o PushOutcome) unresolved() bool {
	switch o {
	case PushConflict, PushLegacyConflict, PushDBInvalid, PushError:
		return true
	}
	return false
}

// PushRowResult 是一行的推送结果。CacheRev 只在 PushSuperseded 时有值(缓存里更大的 rev);Detail 只进日志。
type PushRowResult struct {
	PlayerID uint64
	Outcome  PushOutcome
	CacheRev string
	Detail   string
}

// MergedIntoMirrorResult 是一次合走标记镜像(只增)的统计。
type MergedIntoMirrorResult struct {
	Added, Present, Conflicts, Errors int
	// ConflictDetails 只进日志(src、库值、缓存值)。
	ConflictDetails []string
}

// Cache 是 leader 对 mapping Redis 的依赖。生产实现是 RedisCache(cache.go,独立 go-redis 客户端,不经 go-zero 熔断器)。
// 全部方法并发安全(任务 goroutine 与决策循环会同时用它)。
type Cache interface {
	Probe(ctx context.Context) (Probe, error)
	ReadControl(ctx context.Context) (Control, error)
	// SelfCheck 跑一次读方同一段 RouteReadyReadScript(不带映射键)。
	SelfCheck(ctx context.Context) (placement.RouteReadyRead, error)
	SetWarmToken(ctx context.Context, token string) error
	// ArmReady 用 RouteReadyArmScript 挂标记,同一连接紧跟 WAITAOF;挂上了却确认不了落盘时按原值撤掉并返回 error。
	ArmReady(ctx context.Context, token string, m placement.RouteReadyMarker) (placement.RouteReadyArmOutcome, error)
	CompareAndSetReady(ctx context.Context, old, next string) (bool, error)
	CompareAndDeleteReady(ctx context.Context, old string) (bool, error)
	// DeleteReadyWrongType 无条件 DEL 就绪标记键,只给「键在但不是 string」用。
	DeleteReadyWrongType(ctx context.Context) error
	// DeleteWatermarkWrongType 无条件 DEL 水位键,只给「键在但不是 string」用(核实意见 A1-8)。
	DeleteWatermarkWrongType(ctx context.Context) error
	// AdvanceWatermark:prev 为 nil 时 SET NX,否则按原值 CAS 成 next。
	AdvanceWatermark(ctx context.Context, prev *string, next string) (bool, error)
	Push(ctx context.Context, rows []store.RouteRow) ([]PushRowResult, error)
	MirrorMergedInto(ctx context.Context, rows []store.MergedIntoRow) (MergedIntoMirrorResult, error)
	Close() error
}

// Clock 是单调时钟、可取消的等待与定时通道。生产用 realClock;单测注入假钟,不依赖墙钟、不真睡。
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Observer 是指标出口(生产是 metrics 包,单测注入记录器)。label 取 metrics.RouteReconcile* 常量。
type Observer interface {
	Tick(result string)
	Event(event string)
	Push(phase, result string, n int)
	Mirror(result string, n int)
	TaskSeconds(phase string, d time.Duration)
	TaskRows(phase string, n int)
	Leader(held bool)
	LeaderObservations(markerPresent, writable bool, cacheVerifiedAgeSeconds float64, selfCheck string)
	ClearLeaderObservations()
}

// metricsObserver 把 Observer 接到 metrics 包(进程内的默认注册表)。
type metricsObserver struct{}

func (metricsObserver) Tick(result string) { metrics.ObserveRouteReconcileTick(result) }

func (metricsObserver) Event(event string) { metrics.ObserveRouteReconcileEvent(event) }

func (metricsObserver) Push(phase, result string, n int) {
	metrics.ObserveRouteReconcilePush(phase, result, n)
}

func (metricsObserver) Mirror(result string, n int) { metrics.ObserveRouteReconcileMirror(result, n) }

func (metricsObserver) TaskSeconds(phase string, d time.Duration) {
	metrics.ObserveRouteReconcileTaskSeconds(phase, d)
}

func (metricsObserver) TaskRows(phase string, n int) { metrics.SetRouteReconcileTaskRows(phase, n) }

func (metricsObserver) Leader(held bool) { metrics.SetRouteReconcileLeader(held) }

func (metricsObserver) LeaderObservations(markerPresent, writable bool, age float64, selfCheck string) {
	metrics.SetRouteLeaderObservations(markerPresent, writable, age, selfCheck)
}

func (metricsObserver) ClearLeaderObservations() { metrics.ClearRouteLeaderObservations() }

// ── 构造 ──────────────────────────────────────────────────────────────────────

// ErrDisabled 表示配置不要求 leader(redis 模式,或 Reconcile.Enabled=false):调用方不构造。
var ErrDisabled = errors.New("route reconcile leader is disabled (needs RouteStore.Mode=db_first and Reconcile.Enabled)")

// Deps 是 New 的依赖。Store / Cache / LeaderID / Random 必填;Clock、Observer 为 nil 时用真实时钟与 metrics 包。
type Deps struct {
	// OnDanger 必须同步关闭本进程 root/relay 接纳并落真实危险原件；不能在此等待本 worker 自己退出。
	// 它不持 operator，也不把观察改成已冻结收据；独立维护 owner 负责申请紧急票据。
	OnDanger func(context.Context, *store.RouteEmergencyError) error
	Store    Store
	Cache    Cache
	LeaderID string
	// Random 是预热令牌的随机源(生产 crypto/rand.Reader)。
	Random   io.Reader
	Clock    Clock
	Observer Observer
}

// 各处的固定时限(不进配置:它们是实现细节,上限由租约与 go-zero 的 5.5s 关停预算决定)。
const (
	// tickOpTimeout 是拍内一次 Redis / 库调用的时限(冻结事务另用 FreezeTimeout)。
	tickOpTimeout = 3 * time.Second
	// taskOpTimeout 是任务里一次扫库 / 推送 / 复读的时限(一批至多 1000 行)。
	taskOpTimeout = 10 * time.Second
	// releaseTimeout 是关停 / 让出租约时释放租约的独立时限(尽力而为,失败不重试,租约到期自然失效)。
	releaseTimeout = time.Second
	// taskStopWait 是 Run 退出时等在途任务收尾的上限;data_service.go 等 Run 整体退出最多 500ms(规格 §4.2「关停顺序」)。
	taskStopWait = 200 * time.Millisecond
	// unresolvedLogSample 是一次日志里列出的未决 player_id 个数上限。
	unresolvedLogSample = 10
)

type options struct {
	arm                 bool
	leaseMs             uint64
	leaseSafety         time.Duration
	lease               time.Duration
	renewInterval       time.Duration
	tick                time.Duration
	incrementalInterval time.Duration
	freezeTimeout       time.Duration
	redisFailureRelease time.Duration
	overlapMs           uint64
	lookbackMs          uint64
	toleranceMs         uint64
	pushBatch           int
	pushRate            int
	pendingLimit        int
	assumeNoEviction    bool
}

func optionsFrom(rc config.RouteStoreConfig) options {
	r := rc.Reconcile
	ms := func(v int) time.Duration { return time.Duration(v) * time.Millisecond }
	return options{
		arm:                 r.ArmReadyMarker,
		leaseMs:             uint64(r.LeaseMs),
		leaseSafety:         ms(r.LeaseSafetyMs()),
		lease:               ms(r.LeaseMs),
		renewInterval:       ms(r.RenewIntervalMs),
		tick:                ms(r.TickIntervalMs),
		incrementalInterval: ms(r.IncrementalIntervalMs),
		freezeTimeout:       ms(r.FreezeTimeoutMs),
		redisFailureRelease: ms(r.RedisFailureReleaseMs),
		overlapMs:           uint64(r.IncrementalOverlapMs),
		lookbackMs:          uint64(r.RepairLookbackMs),
		toleranceMs:         r.ToleranceMs(),
		pushBatch:           r.PushBatch,
		pushRate:            r.PushMaxRowsPerSec,
		pendingLimit:        r.ArmingBlockedReportLimit,
		assumeNoEviction:    r.AssumeNoEviction,
	}
}

// Reconciler 是对账 leader。构造时不开连接、不起 goroutine(单测可以多次构造);Start / Run 才开始工作。
type Reconciler struct {
	onDanger    func(context.Context, *store.RouteEmergencyError) error
	dangerOnce  sync.Once
	halted      atomic.Bool
	started     atomic.Bool
	stopping    atomic.Bool
	effects     *drainproof.Session
	shutdownErr error
	opts        options
	st          Store
	cache       Cache
	clock       Clock
	rnd         io.Reader
	obs         Observer
	leaderID    string

	// epoch 是本地单调时钟的基准;leaseDeadlineNs = 「本地租约有效至」相对 epoch 的纳秒数(0 = 不持有),发布给任务 goroutine。
	epoch           time.Time
	leaseDeadlineNs atomic.Int64

	// ── 以下只由决策循环读写 ──
	leader        bool
	localDeadline time.Time
	lastRenew     time.Time
	// acquireHoldoffUntil:因连不上 Redis 让出租约后,这段时间内不再抢(让连得上的副本接手)。
	acquireHoldoffUntil time.Time
	redisFailSince      time.Time

	evictedBaseline int64
	baselineSet     bool
	// baselineIdentity 是取基线时的 Redis 身份:evicted_keys 是每个进程自己的累计值,身份变了基线就要重取。
	baselineIdentity placement.RedisInstanceIdentity
	sawControlKeys   bool

	// vi 是「已验证身份」:这份副本被确认对应的 Redis 实例身份(追平完成、标记身份相符、挂标记成功时置)。
	vi        *placement.RedisInstanceIdentity
	candidate *armCandidate
	latch     *armLatch
	// queuedRepair:撤掉身份不符的标记时有任务在跑,修补排在它之后(标记已撤,下一拍看到的是「标记不在」,不能因此改成全量预热)。
	queuedRepair *task
	lastRefusal  string

	pending         map[uint64]struct{}
	pendingOverflow bool

	pendingCvm         *uint64
	pendingCvmIdentity placement.RedisInstanceIdentity
	cacheAheadPending  bool
	// cacheEvidenceLost:上一拍判过「副本对账到哪」的证据已丢(checkCacheEvidence),只用来让 ERROR 日志按变化打一次。
	cacheEvidenceLost bool
	// lastWatermarkShape:上一次记过日志的畸形 / 版本更新水位键(种类 + 原值),同一值只记一次(settleWatermarkShape)。
	lastWatermarkShape string

	generation string
	cvm        uint64
	wm         uint64
	dbNow      uint64

	lastIncremental time.Time
	running         *runningTask
	taskSeq         uint64
	taskDone        chan taskResult
	startedThisTick bool

	// syncTasks:单测用,任务在启动它的那一拍里同步跑完,结果在下一拍开头收(等价于「两拍之间跑完」)。
	syncTasks  bool
	syncResult *taskResult
}

// New 构造 leader。rc 必须是 Normalize 过的 RouteStore 段;不要求 leader(redis 模式 / Enabled=false)时回 ErrDisabled。
func New(rc config.RouteStoreConfig, d Deps) (*Reconciler, error) {
	if !rc.ReconcileEnabled() {
		return nil, ErrDisabled
	}
	if err := rc.Validate(); err != nil {
		return nil, fmt.Errorf("route reconcile: %w", err)
	}
	switch {
	case d.OnDanger == nil:
		return nil, errors.New("route reconcile 缺少关闭本 boot 接纳与保存危险原件的 owner")
	case d.Store == nil:
		return nil, errors.New("route reconcile: the route store is required")
	case d.Cache == nil:
		return nil, errors.New("route reconcile: the mapping redis cache is required")
	case d.Random == nil:
		return nil, errors.New("route reconcile: a random source is required for warm tokens")
	case d.LeaderID == "":
		return nil, errors.New("route reconcile: the leader id is required (store.NewRouteLeaderID)")
	}
	if d.Clock == nil {
		d.Clock = realClock{}
	}
	if d.Observer == nil {
		d.Observer = metricsObserver{}
	}
	effects := drainproof.New()
	r := &Reconciler{
		effects:  effects,
		opts:     optionsFrom(rc),
		onDanger: d.OnDanger,
		st:       &drainStore{Store: d.Store, effects: effects},
		cache:    &drainCache{Cache: d.Cache, effects: effects},
		clock:    d.Clock,
		rnd:      d.Random,
		obs:      d.Observer,
		leaderID: d.LeaderID,
		pending:  map[uint64]struct{}{},
		taskDone: make(chan taskResult, 1),
	}
	r.epoch = r.clock.Now()
	return r, nil
}

// LeaderID 返回本副本的 leader 标识(日志 / 横幅用)。
func (r *Reconciler) LeaderID() string { return r.leaderID }

// ArmEnabled 返回 ArmReadyMarker 的取值(横幅、arm_enabled gauge 用)。
func (r *Reconciler) ArmEnabled() bool { return r.opts.arm }

// Start 在 safego goroutine 里跑 Run,返回 stop:取消并最多等 wait,返回 Run 是否在时限内退出。
// 关停顺序(规格 §4.2):data_service.go 把 stop 注册在 Kafka 的 defer 之前(LIFO,在 Kafka flush 之后执行)、等待 ≤ 500ms;
// 释放租约是 Run 退出路径上的尽力而为(独立 1s 时限,失败不重试,租约 LeaseMs 后自然过期)。整个关停不超过 go-zero proc 的 5.5s。
func (r *Reconciler) Start(parent context.Context) (stop func(wait time.Duration) bool) {
	life, cancelLife := context.WithCancel(parent)
	finish := r.StartWithReport(life)
	return func(wait time.Duration) bool {
		cancelLife()
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		return finish(ctx) == nil
	}
}

// StartWithReport 只在真实 worker 已 join 且无未决结果后成功；超时返回错误，后台任务仍由 owner 保留并等待。
func (r *Reconciler) StartWithReport(parent context.Context) func(context.Context) error {
	if !r.started.CompareAndSwap(false, true) {
		return func(context.Context) error { return errors.New("reconciler 不能重复启动") }
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	safego.Go("data_service.route_reconcile", func() {
		defer close(done)
		defer cancel()
		defer func() {
			if recovered := recover(); recovered != nil {
				r.shutdownErr = errors.Join(r.shutdownErr, errors.New("reconciler panic，排空结局未知"))
				if r.effects != nil {
					r.effects.Hold()
				}
			}
		}()
		r.Run(ctx)
	})
	return func(wait context.Context) error {
		r.stopping.Store(true)
		select {
		case <-done:
			return r.shutdownErr
		case <-wait.Done():
			return wait.Err()
		}
	}
}

// Run 是唯一决策循环；退出等待真实任务 join。调用者超时仍须保留该 owner，不能宣称排空。
func (r *Reconciler) Run(ctx context.Context) {
	defer r.shutdown()
	logx.Infof("[RouteReconcile] started: leader_id=%s arm_ready_marker=%t lease_ms=%d tick=%s incremental=%s overlap_ms=%d lookback_ms=%d push_batch=%d push_rate=%d/s",
		r.leaderID, r.opts.arm, r.opts.leaseMs, r.opts.tick, r.opts.incrementalInterval, r.opts.overlapMs, r.opts.lookbackMs, r.opts.pushBatch, r.opts.pushRate)
	for ctx.Err() == nil && !r.halted.Load() && !r.stopping.Load() {
		r.tickOnce(ctx)
		if err := r.clock.Sleep(ctx, r.opts.tick); err != nil {
			return
		}
	}
}

func (r *Reconciler) shutdown() {
	if r.halted.Load() || len(r.pending) != 0 || r.pendingOverflow {
		r.shutdownErr = errors.Join(r.shutdownErr, errors.New("reconciler 存在危险或历史未决状态"))
	}
	if rt := r.running; rt != nil {
		// 正式排空只停新拍，允许原任务以原上下文结束；外部取消仍保留UNKNOWN。
		if !r.stopping.Load() {
			rt.abort("shutdown")
		}
		var result taskResult
		if r.syncResult == nil {
			result = <-r.taskDone
		} else {
			result = *r.syncResult
			r.syncResult = nil
		}
		if result.err != nil || result.freezeErr != nil || len(result.unresolved) != 0 || result.overflow || !result.complete {
			r.shutdownErr = errors.Join(r.shutdownErr, errors.New("reconciler 任务已 join，但仍有取消或未决发布结果，不能声明排空"), result.err, result.freezeErr)
		}
		r.running = nil
	}
	if r.leader {
		ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
		released, err := r.st.ReleaseReconcileLease(ctx, r.leaderID)
		cancel()
		switch {
		case err != nil:
			r.shutdownErr = errors.Join(r.shutdownErr, err)
			logx.Errorf("[RouteReconcile] shutdown: releasing the lease failed (it expires on its own after %dms): %v", r.opts.leaseMs, err)
		case !released:
			logx.Infof("[RouteReconcile] shutdown: the lease was no longer ours")
		default:
			logx.Infof("[RouteReconcile] shutdown: lease released")
		}
	}
	r.leader = false
	r.leaseDeadlineNs.Store(0)
	r.obs.Leader(false)
	r.obs.ClearLeaderObservations()
	if err := r.cache.Close(); err != nil {
		r.shutdownErr = errors.Join(r.shutdownErr, err)
		logx.Errorf("[RouteReconcile] shutdown: close mapping redis client: %v", err)
	}
	if r.effects != nil {
		if r.shutdownErr != nil {
			r.effects.Hold()
		}
		r.effects.CloseAdmission()
		r.shutdownErr = errors.Join(r.shutdownErr, r.effects.Wait(context.Background()))
	}
	logx.Infof("[RouteReconcile] stopped")
}

// ── 任务 ──────────────────────────────────────────────────────────────────────

type taskPhase uint8

const (
	phasePrewarm taskPhase = iota + 1
	phaseRepair
	phaseCatchUp
	phaseIncremental
)

func (p taskPhase) String() string {
	switch p {
	case phasePrewarm:
		return metrics.RouteReconcilePhasePrewarm
	case phaseRepair:
		return metrics.RouteReconcilePhaseRepair
	case phaseCatchUp:
		return metrics.RouteReconcilePhaseCatchUp
	case phaseIncremental:
		return metrics.RouteReconcilePhaseIncremental
	}
	return "unknown"
}

// tokened 报告这类任务是否带预热令牌(可能以挂标记收尾、或要靠令牌证明期间没有 FLUSH / rewarm)。
func (p taskPhase) tokened() bool { return p != phaseIncremental }

type task struct {
	seq   uint64
	phase taskPhase
	// fromMs:repair / catch_up / incremental 扫 updated_at_ms >= fromMs 的行(prewarm 全表,不用它)。
	fromMs     uint64
	token      string
	identity   placement.RedisInstanceIdentity
	generation string
	// pending:incremental 顺带复读的未决 player_id。
	pending []uint64
	// pendingSet:任务开始时 leader 记下的未决 player_id(预热为 nil:startTask 先清空了未决表)。任务只对其中的 id 记 resolved
	// (核实意见 LR-8):applyTaskResult 只拿 resolved 去删未决表,记全表每一行只会让 leader 内存随表长到 O(行数)。
	pendingSet map[uint64]struct{}
}

type taskResult struct {
	seq           uint64
	phase         taskPhase
	token         string
	generation    string
	startMs       uint64
	startIdentity placement.RedisInstanceIdentity
	endIdentity   placement.RedisInstanceIdentity
	// identityStable:任务开始与结束时 Redis 身份相同;tokenHeld:结束时 mapping:route:warm 仍是本任务的令牌。
	identityStable bool
	tokenHeld      bool
	complete       bool
	rows           int
	unresolved     []uint64
	overflow       bool
	resolved       []uint64
	cacheAhead     bool
	freezeOutcome  store.ReconcileTickOutcome
	freezeErr      error
	err            error
}

type runningTask struct {
	task      task
	cancel    context.CancelFunc
	startedAt time.Time
	// aborted 非空 = 循环主动取消(降级、冻结、身份变化、令牌被换);结果到达时整份丢弃。
	aborted string
}

func (rt *runningTask) abort(reason string) {
	if rt.aborted == "" {
		rt.aborted = reason
	}
	rt.cancel()
}

// armCandidate 是一次完整预热 / 修补的结果:令牌、它对应的 Redis 身份与库世代。挂标记在决策循环里做。
type armCandidate struct {
	token      string
	identity   placement.RedisInstanceIdentity
	generation string
	phase      taskPhase
}

// armLatch 是 armingBlocked 闩锁(规格 §4.2「挂标记被拒后无限预热」):被拒之后不再重跑全量预热,只做增量并复读未决行;
// 阻塞原因清空后用手里的候选挂标记。candidate 为 nil = 没有可挂的完整结果(例如淘汰导致撤标记),解除后要全量预热一次;
// evictionTaint = 等待期间发生过淘汰,候选作废,解除后同样要全量预热。身份变化、控制键被清(rewarm)时整个闩锁作废。
type armLatch struct {
	candidate     *armCandidate
	identity      placement.RedisInstanceIdentity
	evictionTaint bool
}

// startTask 起一个任务(调用方已确认 running == nil)。带令牌的任务先 SET 新令牌(M2):SET 失败就不起。
func (r *Reconciler) startTask(ctx context.Context, t task) bool {
	if t.phase.tokened() {
		if !r.leaseValid() {
			// 慢拍里本地租约已过点(核实意见 LR-10):SET 令牌不受库侧租约谓词保护,会让接手的新 leader 的任务挂不上标记。
			logx.Errorf("[RouteReconcile] %s not started: the local lease expired during this tick", t.phase)
			return false
		}
		token, err := newWarmToken(r.rnd)
		if err == nil {
			opCtx, cancel := context.WithTimeout(ctx, tickOpTimeout)
			err = r.cache.SetWarmToken(opCtx, token)
			cancel()
		}
		if err != nil {
			logx.Errorf("[RouteReconcile] %s not started: setting a new warm token failed: %v", t.phase, err)
			return false
		}
		t.token = token
	}
	if t.phase == phasePrewarm {
		// 全量预热重新发现全部未决行。
		r.pending = map[uint64]struct{}{}
		r.pendingOverflow = false
	}
	if t.phase == phaseIncremental {
		r.lastIncremental = r.clock.Now()
		t.pending = r.pendingIDs()
	}
	t.pendingSet = nil
	if len(r.pending) > 0 {
		t.pendingSet = make(map[uint64]struct{}, len(r.pending))
		for id := range r.pending {
			t.pendingSet[id] = struct{}{}
		}
	}
	r.taskSeq++
	t.seq = r.taskSeq
	t.generation = r.generation
	tctx, cancel := context.WithCancel(ctx)
	r.running = &runningTask{task: t, cancel: cancel, startedAt: r.clock.Now()}
	r.startedThisTick = true
	r.obs.Event(metrics.RouteReconcileEventTaskStarted)
	r.obs.TaskRows(t.phase.String(), 0)
	logx.Infof("[RouteReconcile] %s seq=%d started: from_ms=%d pending=%d run_id=%s", t.phase, t.seq, t.fromMs, len(t.pending), t.identity.RunID)
	if r.syncTasks {
		res := r.runTask(tctx, t)
		r.syncResult = &res
		return true
	}
	go func() {
		var res taskResult
		if !safego.Run("data_service.route_reconcile.task", func() { res = r.runTask(tctx, t) }) {
			res = taskResult{seq: t.seq, phase: t.phase, err: errors.New("task panicked (see the safego log)")}
		}
		r.taskDone <- res
	}()
	return true
}

// collectTaskResult 收已结束任务的结果(非阻塞),交给 applyTaskResult。
func (r *Reconciler) collectTaskResult(ctx context.Context) {
	var res taskResult
	switch {
	case r.syncResult != nil:
		res = *r.syncResult
		r.syncResult = nil
	case r.running != nil:
		select {
		case res = <-r.taskDone:
		default:
			return
		}
	default:
		return
	}
	rt := r.running
	r.running = nil
	if rt == nil || rt.task.seq != res.seq {
		logx.Errorf("[RouteReconcile] dropped the result of an unknown task seq=%d (internal bug)", res.seq)
		return
	}
	rt.cancel()
	r.obs.TaskSeconds(res.phase.String(), r.clock.Now().Sub(rt.startedAt))
	r.obs.TaskRows(res.phase.String(), res.rows)
	if rt.aborted != "" {
		r.obs.Event(metrics.RouteReconcileEventTaskAborted)
		logx.Infof("[RouteReconcile] %s seq=%d aborted by the leader loop (%s) after %d rows; result discarded", res.phase, res.seq, rt.aborted, res.rows)
		// 结果整份丢弃,但已确认的 cache_ahead 证据不能跟着丢(核实意见 A1-2)。
		r.keepCacheAheadEvidence(res)
		return
	}
	r.applyTaskResult(ctx, res, rt.task)
}

// keepCacheAheadEvidence 处理任务确认过的 cache_ahead(库丢了已提交的写):冻结做成了只计事件;没做成(出错,或结局不是
// Frozen / AlreadyFrozen)而本副本仍是 leader → cacheAheadPending,之后每拍重试冻结,直到冻结或丢租约(fail-closed)。
// 证据只在 leader 内存里:增量从 cvm − Overlap 起扫,扫不到回档前的旧行;FLUSH 之后缓存里的超前值也没了,丢了就再也发现不了。
// 已降级时不置:resetTerm 清掉本任期状态,新任期不替前任背这份证据(push 里的冻结不随任务取消回滚,多半已经做成)。
func (r *Reconciler) keepCacheAheadEvidence(res taskResult) {
	if !res.cacheAhead {
		return
	}
	switch {
	case res.freezeErr == nil && res.freezeOutcome == store.ReconcileTickFrozen:
		r.obs.Event(metrics.RouteReconcileEventFreezeCacheAhead)
	case res.freezeErr == nil && res.freezeOutcome == store.ReconcileTickAlreadyFrozen:
	case r.leader:
		logx.Errorf("[RouteReconcile] %s seq=%d confirmed cache_ahead but the freeze did not commit (outcome %s, err %v); retrying it every tick",
			res.phase, res.seq, res.freezeOutcome, res.freezeErr)
		r.cacheAheadPending = true
	}
}

func (r *Reconciler) applyTaskResult(ctx context.Context, res taskResult, t task) {
	if res.cacheAhead {
		r.obs.Event(metrics.RouteReconcileEventTaskAborted)
		r.keepCacheAheadEvidence(res)
		return
	}
	if !res.complete {
		r.obs.Event(metrics.RouteReconcileEventTaskAborted)
		logx.Errorf("[RouteReconcile] %s seq=%d aborted after %d rows: %v", res.phase, res.seq, res.rows, res.err)
		if t.phase == phaseRepair && r.queuedRepair == nil {
			// 标记已经撤掉了;只要身份没再变,修补重排(不因一次网络错误升级成全量预热)。
			retry := t
			retry.token, retry.seq = "", 0
			r.queuedRepair = &retry
		}
		return
	}
	r.obs.Event(metrics.RouteReconcileEventTaskCompleted)
	for _, id := range res.resolved {
		delete(r.pending, id)
	}
	for _, id := range res.unresolved {
		r.addPending(id)
	}
	if res.overflow {
		r.pendingOverflow = true
	}
	if !res.identityStable || (res.phase.tokened() && !res.tokenHeld) {
		logx.Errorf("[RouteReconcile] %s seq=%d finished but cannot be trusted: identity_stable=%t token_held=%t (start run_id=%s end run_id=%s); "+
			"cache_verified_ms is not advanced and nothing is armed", res.phase, res.seq, res.identityStable, res.tokenHeld,
			res.startIdentity.RunID, res.endIdentity.RunID)
		return
	}
	s := res.startMs
	r.pendingCvm = &s
	r.pendingCvmIdentity = res.endIdentity
	logx.Infof("[RouteReconcile] %s seq=%d completed: rows=%d unresolved=%d overflow=%t start_ms=%d", res.phase, res.seq, res.rows,
		len(res.unresolved), res.overflow, res.startMs)
	switch res.phase {
	case phaseCatchUp:
		id := res.endIdentity
		r.vi = &id
	case phasePrewarm, phaseRepair:
		r.candidate = &armCandidate{token: res.token, identity: res.endIdentity, generation: res.generation, phase: res.phase}
	}
}

func (r *Reconciler) addPending(id uint64) {
	if _, ok := r.pending[id]; ok {
		return
	}
	if len(r.pending) >= r.opts.pendingLimit {
		r.pendingOverflow = true
		return
	}
	r.pending[id] = struct{}{}
}

func (r *Reconciler) pendingIDs() []uint64 {
	ids := make([]uint64, 0, len(r.pending))
	for id := range r.pending {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// leaseValid 报告本地租约此刻是否有效(任务 goroutine 每批之前调)。
func (r *Reconciler) leaseValid() bool {
	d := r.leaseDeadlineNs.Load()
	return d != 0 && r.clock.Now().Sub(r.epoch).Nanoseconds() < d
}

var (
	errLeaseExpired = errors.New("local lease expired")
	errCacheAhead   = errors.New("cache ahead of the database")
)

// taskRun 是一个任务的执行状态(只在任务 goroutine 里用)。
type taskRun struct {
	r            *Reconciler
	t            task
	res          *taskResult
	throttleBase time.Time
	throttleRows int
	seen         map[uint64]bool
}

// runTask 跑一个任务。只读库、推 Redis、复读库;cache_ahead 时直接调 FreezeByLeader(带租约谓词)并中止。不改 Reconciler 的循环状态。
func (r *Reconciler) runTask(ctx context.Context, t task) taskResult {
	res := taskResult{seq: t.seq, phase: t.phase, token: t.token, generation: t.generation, startIdentity: t.identity}
	run := &taskRun{r: r, t: t, res: &res, throttleBase: r.clock.Now(), seen: map[uint64]bool{}}

	opCtx, cancel := context.WithTimeout(ctx, taskOpTimeout)
	s, err := r.st.DBNowMs(opCtx)
	cancel()
	if err != nil {
		res.err = fmt.Errorf("read the database clock (task start S): %w", err)
		return res
	}
	res.startMs = s

	switch t.phase {
	case phasePrewarm:
		err = run.prewarm(ctx)
	default:
		err = run.scanUpdatedSince(ctx)
	}
	if err == nil && t.phase == phaseIncremental {
		err = run.recheckPending(ctx)
	}
	if err == nil {
		err = run.mirrorMergedInto(ctx)
	}
	if err != nil {
		res.err = err
		return res
	}

	opCtx, cancel = context.WithTimeout(ctx, taskOpTimeout)
	defer cancel()
	p, err := r.cache.Probe(opCtx)
	if err != nil {
		res.err = fmt.Errorf("re-read the redis identity at the end: %w", err)
		return res
	}
	res.endIdentity = p.Identity
	res.identityStable = p.Identity == t.identity
	if t.phase.tokened() {
		ctl, err := r.cache.ReadControl(opCtx)
		if err != nil {
			res.err = fmt.Errorf("re-read the warm token at the end: %w", err)
			return res
		}
		res.tokenHeld = ctl.Warm != nil && *ctl.Warm == t.token
	}
	res.complete = true
	return res
}

// gate 在每批之前检查:ctx 没结束、本地租约仍有效(规格 scout「LeaseExpiryMidPrewarmAbortsBeforeNextBatch」)。
func (run *taskRun) gate(ctx context.Context) error {
	if run.r.halted.Load() {
		return errors.New("本 boot 已因路由危险永久停止发布")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !run.r.leaseValid() {
		return errLeaseExpired
	}
	return nil
}

// throttle 按 PushMaxRowsPerSec 限速(注入的时钟,不依赖墙钟):累计行数对应的最早时刻还没到就等。
func (run *taskRun) throttle(ctx context.Context, n int) error {
	run.throttleRows += n
	due := run.throttleBase.Add(time.Duration(float64(run.throttleRows) / float64(run.r.opts.pushRate) * float64(time.Second)))
	if wait := due.Sub(run.r.clock.Now()); wait > 0 {
		return run.r.clock.Sleep(ctx, wait)
	}
	return nil
}

func (run *taskRun) prewarm(ctx context.Context) error {
	r := run.r
	cursor := store.RouteScanCursor{}
	for {
		if err := run.gate(ctx); err != nil {
			return err
		}
		opCtx, cancel := context.WithTimeout(ctx, taskOpTimeout)
		rows, err := r.st.ScanRoutes(opCtx, cursor, r.opts.pushBatch)
		cancel()
		if err != nil {
			return fmt.Errorf("scan player_route after (%d, %d): %w", cursor.Shard, cursor.PlayerID, err)
		}
		if len(rows) > 0 {
			if err := run.push(ctx, rows); err != nil {
				return err
			}
			cursor = store.RouteScanCursorAt(rows[len(rows)-1])
		}
		if len(rows) < r.opts.pushBatch {
			return nil
		}
	}
}

// scanUpdatedSince 对 64 个行片各自按 (updated_at_ms, player_id) 键集翻页,起点 updated_at_ms >= fromMs(提案 §11.2 G1a 模板)。
func (run *taskRun) scanUpdatedSince(ctx context.Context) error {
	r := run.r
	for shard := uint32(0); shard < placement.RouteRowShards; shard++ {
		cursor := store.RouteUpdatedSince(run.t.fromMs)
		for {
			if err := run.gate(ctx); err != nil {
				return err
			}
			opCtx, cancel := context.WithTimeout(ctx, taskOpTimeout)
			rows, err := r.st.ScanRoutesUpdatedSince(opCtx, shard, cursor, r.opts.pushBatch)
			cancel()
			if err != nil {
				return fmt.Errorf("scan player_route shard %d updated after (%d, %d): %w", shard, cursor.UpdatedAtMs, cursor.PlayerID, err)
			}
			if len(rows) > 0 {
				if err := run.push(ctx, rows); err != nil {
					return err
				}
				cursor = store.RouteUpdatedCursorAt(rows[len(rows)-1])
			}
			if len(rows) < r.opts.pushBatch {
				break
			}
		}
	}
	return nil
}

// recheckPending 复读上一轮留下的未决行并重推(armingBlocked 期间靠它解除「未决」)。库里读不到的行仍算未决(只记日志)。
func (run *taskRun) recheckPending(ctx context.Context) error {
	r := run.r
	ids := run.t.pending
	for len(ids) > 0 {
		n := min(len(ids), store.RouteBatchLimit, r.opts.pushBatch)
		chunk := ids[:n]
		ids = ids[n:]
		if err := run.gate(ctx); err != nil {
			return err
		}
		opCtx, cancel := context.WithTimeout(ctx, taskOpTimeout)
		got, err := r.st.GetRoutes(opCtx, chunk)
		cancel()
		if err != nil {
			return fmt.Errorf("re-read %d unresolved rows: %w", len(chunk), err)
		}
		rows := make([]store.RouteRow, 0, len(got))
		for _, id := range chunk {
			if row, ok := got[id]; ok {
				rows = append(rows, row)
			} else {
				logx.Errorf("[RouteReconcile] unresolved player %d has no player_route row any more; it stays unresolved (check route_tool audit)", id)
				run.unresolve(id)
			}
		}
		if len(rows) > 0 {
			if err := run.push(ctx, rows); err != nil {
				return err
			}
		}
	}
	return nil
}

func (run *taskRun) unresolve(id uint64) {
	if len(run.res.unresolved) >= run.r.opts.pendingLimit {
		run.res.overflow = true
		return
	}
	run.res.unresolved = append(run.res.unresolved, id)
}

// resolve 记一行「已解决」:只记任务开始时 leader 记下的未决 id(task.pendingSet;预热为空集,一行都不记)。
// resolved 唯一的用途是让 applyTaskResult 把它们从未决表删掉,别的 id 记了也是空操作(核实意见 LR-8)。
func (run *taskRun) resolve(id uint64) {
	if _, ok := run.t.pendingSet[id]; ok {
		run.res.resolved = append(run.res.resolved, id)
	}
}

// push 推一批行:限速 → Push → superseded 的复读库(库 rev < 缓存 rev 或读不到行 = cache_ahead,冻结并中止)→ 计数。
func (run *taskRun) push(ctx context.Context, rows []store.RouteRow) error {
	r := run.r
	phase := run.t.phase.String()
	if err := run.throttle(ctx, len(rows)); err != nil {
		return err
	}
	if err := run.gate(ctx); err != nil {
		return err
	}
	opCtx, cancel := context.WithTimeout(ctx, taskOpTimeout)
	results, err := r.cache.Push(opCtx, rows)
	cancel()
	if err != nil {
		return fmt.Errorf("push %d rows: %w", len(rows), err)
	}
	counts := map[string]int{}
	var superseded []PushRowResult
	for _, pr := range results {
		switch {
		case pr.Outcome == PushSuperseded:
			superseded = append(superseded, pr)
			continue
		case pr.Outcome.unresolved():
			run.unresolve(pr.PlayerID)
			// seen 只记打过日志的 id(至多 unresolvedLogSample*10 个):遗留值冲突成片时它不随未决行数长(核实意见 LR-8 同类)。
			if !run.seen[pr.PlayerID] && len(run.seen) < unresolvedLogSample*10 {
				logx.Errorf("[RouteReconcile] %s: player %d unresolved (%s): %s", phase, pr.PlayerID, pr.Outcome.label(), pr.Detail)
				run.seen[pr.PlayerID] = true
			}
		default:
			run.resolve(pr.PlayerID)
		}
		counts[pr.Outcome.label()]++
	}
	defer func() {
		for label, n := range counts {
			r.obs.Push(phase, label, n)
		}
		run.res.rows += len(rows)
		r.obs.TaskRows(phase, run.res.rows)
	}()
	if len(superseded) == 0 {
		return nil
	}
	ids := make([]uint64, len(superseded))
	for i, s := range superseded {
		ids[i] = s.PlayerID
	}
	opCtx, cancel = context.WithTimeout(ctx, taskOpTimeout)
	db, err := r.st.GetRoutes(opCtx, ids)
	cancel()
	if err != nil {
		return fmt.Errorf("re-read %d superseded rows: %w", len(ids), err)
	}
	var ahead []string
	for _, s := range superseded {
		row, ok := db[s.PlayerID]
		cmp := -1
		if ok {
			cmp, err = placement.CompareRev(placement.FormatRev(row.Rev), s.CacheRev)
			if err != nil {
				run.unresolve(s.PlayerID)
				counts[metrics.RouteReconcilePushError]++
				logx.Errorf("[RouteReconcile] %s: player %d superseded with an uncomparable rev (db %d, cache %q): %v", phase, s.PlayerID, row.Rev, s.CacheRev, err)
				continue
			}
		}
		if cmp < 0 {
			// 库 rev < 缓存 rev,或库里干脆没有这一行:缓存比库新 —— 库丢了已提交的写(回档),或有写者绕过了库。
			counts[metrics.RouteReconcilePushCacheAhead]++
			if ok {
				ahead = append(ahead, fmt.Sprintf("%d(db_rev=%d cache_rev=%s)", s.PlayerID, row.Rev, s.CacheRev))
			} else {
				ahead = append(ahead, fmt.Sprintf("%d(no db row, cache_rev=%s)", s.PlayerID, s.CacheRev))
			}
			continue
		}
		// 库 rev >= 缓存 rev:在线写者在我们读库之后提交并发布了更新的版本,正常竞态(更新的库值由写者自己发布,或由下一轮增量补)。
		counts[metrics.RouteReconcilePushSuperseded]++
		run.resolve(s.PlayerID)
	}
	if len(ahead) == 0 {
		return nil
	}
	if len(ahead) > unresolvedLogSample {
		ahead = append(ahead[:unresolvedLogSample], fmt.Sprintf("... %d more", len(ahead)-unresolvedLogSample))
	}
	logx.Errorf("[RouteReconcile] %s: 实际缓存版本领先 SQL: %s；关闭本 boot 并请求独立维护 owner 核验", phase, strings.Join(ahead, ", "))
	run.res.cacheAhead = true
	// 冻结是 fail-closed 动作,不随任务被取消而回滚(核实意见 A1-2):TiDB 档冻结要等缓存表读租约(至多 FreezeTimeout),这几秒里
	// 循环可能因令牌被换 / 身份变化 / 排修补 / 降级取消本任务;用任务 ctx 的话冻结事务跟着回滚,结果又被整份丢弃。
	// FreezeByLeader 自带 FreezeTimeout 时限与库侧租约谓词(失权回 LeaseLost,不会越权冻结);关停时最多在后台多跑 FreezeTimeout。
	fr, ferr := r.st.FreezeByLeader(context.WithoutCancel(ctx), r.leaderID, store.RouteFreezeCacheAhead, r.opts.freezeTimeout)
	run.res.freezeOutcome, run.res.freezeErr = fr.Outcome, ferr
	if ferr != nil {
		r.handleDanger(ctx, ferr)
		logx.Errorf("[RouteReconcile] 危险观察未形成正式冻结收据，本 boot 保持停止: %v", ferr)
	}
	return errCacheAhead
}

func (run *taskRun) mirrorMergedInto(ctx context.Context) error {
	r := run.r
	if err := run.gate(ctx); err != nil {
		return err
	}
	opCtx, cancel := context.WithTimeout(ctx, taskOpTimeout)
	defer cancel()
	rows, err := r.st.ListMergedInto(opCtx)
	if err != nil {
		return fmt.Errorf("list zone_merged_into: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	m, err := r.cache.MirrorMergedInto(opCtx, rows)
	r.obs.Mirror(metrics.RouteReconcileMirrorMergedIntoAdded, m.Added)
	r.obs.Mirror(metrics.RouteReconcileMirrorMergedIntoPresent, m.Present)
	r.obs.Mirror(metrics.RouteReconcileMirrorMergedIntoConflict, m.Conflicts)
	r.obs.Mirror(metrics.RouteReconcileMirrorError, m.Errors)
	for _, d := range m.ConflictDetails {
		logx.Errorf("[RouteReconcile] merge:merged_into mirror CONFLICT (add-only, not overwritten; zone_merged_into is the source of truth): %s", d)
	}
	if err != nil {
		return fmt.Errorf("mirror merge:merged_into: %w", err)
	}
	return nil
}

// ── 决策循环 ──────────────────────────────────────────────────────────────────

// tickOnce 跑一拍并记 tick_total;返回本拍的结果 label。单测直接调它(不起计时器)。
func (r *Reconciler) tickOnce(ctx context.Context) string {
	if ctx.Err() != nil {
		return ""
	}
	r.startedThisTick = false
	r.collectTaskResult(ctx)
	res := r.tick(ctx)
	r.obs.Tick(res)
	return res
}

// markerKind 是一次读到的就绪标记 / 水位键的形状。
type markerKind uint8

const (
	keyAbsent markerKind = iota
	keyValid
	keyMalformed
	keyNewer
	keyWrongType
)

type markerState struct {
	kind markerKind
	raw  string
	m    placement.RouteReadyMarker
}

func classifyMarker(ctl Control) markerState {
	switch {
	case ctl.ReadyWrongType:
		return markerState{kind: keyWrongType}
	case ctl.Ready == nil:
		return markerState{kind: keyAbsent}
	}
	raw := *ctl.Ready
	m, err := placement.DecodeRouteReadyMarker(raw)
	switch {
	case errors.Is(err, placement.ErrRouteReadyMarkerNewerVersion):
		return markerState{kind: keyNewer, raw: raw}
	case err != nil:
		return markerState{kind: keyMalformed, raw: raw}
	}
	return markerState{kind: keyValid, raw: raw, m: m}
}

type watermarkState struct {
	kind markerKind
	raw  *string
	w    placement.RouteWatermark
}

func classifyWatermark(raw *string) watermarkState {
	if raw == nil {
		return watermarkState{kind: keyAbsent}
	}
	w, err := placement.DecodeRouteWatermark(*raw)
	switch {
	case errors.Is(err, placement.ErrRouteWatermarkNewerVersion):
		return watermarkState{kind: keyNewer, raw: raw}
	case err != nil:
		return watermarkState{kind: keyMalformed, raw: raw}
	}
	return watermarkState{kind: keyValid, raw: raw, w: w}
}

// tick 是一拍的主体(规格 §4.3 叠加 §13.1,步骤编号见包注释)。
func (r *Reconciler) tick(ctx context.Context) string {
	now := r.clock.Now()
	if res, ok := r.ensureLease(ctx, now); !ok {
		return res
	}

	// 2. 身份与自检项。
	p, err := r.probe(ctx)
	if err != nil {
		return r.onRedisFailure(ctx, now, "probe", err)
	}
	// 3. 三把控制键。
	opCtx, cancel := context.WithTimeout(ctx, tickOpTimeout)
	ctl, err := r.cache.ReadControl(opCtx)
	cancel()
	if err != nil {
		return r.onRedisFailure(ctx, now, "read control keys", err)
	}
	r.redisFailSince = time.Time{}
	r.takeEvictedBaseline(p)
	if r.pendingCvm != nil && r.pendingCvmIdentity != p.Identity {
		logx.Errorf("[RouteReconcile] redis identity changed since the last task finished (run_id %s → %s); its cache_verified_ms is discarded",
			r.pendingCvmIdentity.RunID, p.Identity.RunID)
		r.pendingCvm = nil
	}
	ms := r.settleMarkerShape(ctx, ctl)
	wm := r.settleWatermarkShape(ctx, ctl)
	r.checkControlKeysWiped(ctl, p)
	r.checkRunningToken(ctl, p)
	evidenceLost := r.checkCacheEvidence(ctl, p)
	if !r.opts.arm && (ms.kind == keyValid || ms.kind == keyNewer) && r.leaseValid() {
		// M4 撤标记只要 Redis 与仍有效的本地租约,放在库内部分之前(核实意见 LR-12):库故障、AdvanceReconcile 出错的拍里也照撤,
		// 回退规程等的 marker_dropped_disarmed 不被库故障卡住。代价:Arm=false 下不再拿将撤的标记做「标记世代 ≠ 库世代」的冻结判据
		// (Arm=false 本不该有标记;库被重建由水位键 M3 与推送时的 cache_ahead 发现)。
		r.dropMarker(ctx, &ms, metrics.RouteReconcileEventMarkerDroppedDisarmed, "ArmReadyMarker=false (M4: disarming withdraws trust)")
	}

	// 4. 每拍的库内部分。
	if res, ok := r.advance(ctx, now, wm, &ms, p); !ok {
		return res
	}

	// 5. 淘汰 / DEBUG 自检。
	if !r.leaseStillHeld("the eviction / debug checks") {
		return metrics.RouteReconcileTickLeaseLost
	}
	evBad, evTaint := r.evictionState(p)
	dbgBad := p.DebugCommand != "no"
	if ms.kind == keyValid && evBad {
		r.dropMarker(ctx, &ms, metrics.RouteReconcileEventMarkerDroppedEviction,
			fmt.Sprintf("eviction is possible or happened (maxmemory-policy=%q evicted_keys=%d baseline=%d)", p.MaxmemoryPolicy, p.EvictedKeys, r.evictedBaseline))
		r.vi = nil
		if r.opts.arm && r.latch == nil {
			r.latch = &armLatch{identity: p.Identity, evictionTaint: evTaint}
		}
	}
	if ms.kind == keyValid && dbgBad {
		// 事件(arming_refused_debug_enabled)由第 8 步的 reportRefusal 计,这里不重复计。
		r.dropMarker(ctx, &ms, "",
			fmt.Sprintf("enable-debug-command=%q (DEBUG RELOAD replaces the data without changing run_id; must be \"no\")", p.DebugCommand))
		r.vi = nil
	}

	// 7 / 8. 标记与任务。
	if !r.leaseStillHeld("the marker / task decision") {
		return metrics.RouteReconcileTickLeaseLost
	}
	if r.opts.arm {
		r.decideArmed(ctx, &ms, p, blockers{eviction: evBad, debug: dbgBad}, evTaint)
	} else {
		r.decideDisarmed(ctx, p)
	}

	// 9. 增量。证据刚丢的这一拍不起(cvm 刚作废;若水位键也没重建成,下一拍还会再判丢、把它取消),下一拍从 0 起全量。
	if !evidenceLost && r.running == nil && !r.startedThisTick && r.queuedRepair == nil && r.clock.Now().Sub(r.lastIncremental) >= r.opts.incrementalInterval {
		r.startTask(ctx, task{phase: phaseIncremental, fromMs: satSub(r.cvm, r.opts.overlapMs), identity: p.Identity})
	}

	// 10. 自检与观测。
	r.observe(ctx, true, ms.kind != keyAbsent)
	return metrics.RouteReconcileTickOK
}

// ensureLease 是第 1 步。返回 (tick 结果, 是否继续本拍)。
func (r *Reconciler) ensureLease(ctx context.Context, now time.Time) (string, bool) {
	if r.leader {
		if !now.Before(r.localDeadline) {
			r.demote("the local lease expired before it could be renewed")
			return metrics.RouteReconcileTickLeaseLost, false
		}
		if now.Sub(r.lastRenew) < r.opts.renewInterval {
			return "", true
		}
		return r.renew(ctx, now)
	}
	if now.Before(r.acquireHoldoffUntil) {
		return metrics.RouteReconcileTickNotLeader, false
	}
	opCtx, cancel := context.WithTimeout(ctx, tickOpTimeout)
	l, err := r.st.PeekReconcileLease(opCtx)
	cancel()
	if err != nil {
		logx.Errorf("[RouteReconcile] peek lease: %v", err)
		return metrics.RouteReconcileTickDBError, false
	}
	if l.Held && l.Holder != r.leaderID {
		return metrics.RouteReconcileTickNotLeader, false
	}
	return r.renew(ctx, now)
}

// renew 续约 / 抢租约。本地截止 = 续约发出时刻 + LeaseMs − Safety(scout D6;库侧 fencing 靠 SQL 谓词)。
func (r *Reconciler) renew(ctx context.Context, sent time.Time) (string, bool) {
	opCtx, cancel := context.WithTimeout(ctx, tickOpTimeout)
	l, err := r.st.RenewReconcileLease(opCtx, r.leaderID, r.opts.leaseMs)
	cancel()
	if err != nil {
		logx.Errorf("[RouteReconcile] renew lease: %v", err)
		return metrics.RouteReconcileTickDBError, false
	}
	if !l.HeldBy(r.leaderID) {
		if r.leader {
			r.demote(fmt.Sprintf("the lease is held by %q now", l.Holder))
			return metrics.RouteReconcileTickLeaseLost, false
		}
		return metrics.RouteReconcileTickNotLeader, false
	}
	if !r.leader {
		r.promote()
	}
	r.lastRenew = sent
	r.localDeadline = sent.Add(r.opts.lease - r.opts.leaseSafety)
	r.leaseDeadlineNs.Store(max(1, r.localDeadline.Sub(r.epoch).Nanoseconds()))
	return "", true
}

func (r *Reconciler) promote() {
	r.leader = true
	r.resetTerm()
	r.obs.Leader(true)
	logx.Infof("[RouteReconcile] became the leader (%s)", r.leaderID)
}

// demote 降级:取消任务、清掉本任期的全部内存状态、删 leader 观测型 gauge。
func (r *Reconciler) demote(reason string) {
	if !r.leader {
		return
	}
	r.leader = false
	r.leaseDeadlineNs.Store(0)
	if r.running != nil {
		r.running.abort("demoted")
	}
	r.resetTerm()
	r.obs.Leader(false)
	r.obs.ClearLeaderObservations()
	logx.Errorf("[RouteReconcile] no longer the leader: %s", reason)
}

// leaseStillHeld 是拍内各步 Redis 写之前的本地租约复核(核实意见 LR-10):拍首判过一次,但一拍可能因 Redis / 库变慢拖过本地截止,
// 之后的 Redis 写(预热令牌、标记)不受库侧租约谓词保护,会与已接手的新 leader 打架。过点即降级,调用方结束本拍。
func (r *Reconciler) leaseStillHeld(before string) bool {
	if r.leaseValid() {
		return true
	}
	r.demote("the local lease expired during the tick (before " + before + ")")
	return false
}

func (r *Reconciler) resetTerm() {
	r.vi, r.candidate, r.latch, r.queuedRepair = nil, nil, nil, nil
	r.lastRefusal, r.lastWatermarkShape = "", ""
	r.cacheEvidenceLost = false
	r.pending = map[uint64]struct{}{}
	r.pendingOverflow = false
	r.pendingCvm = nil
	r.cacheAheadPending = false
	r.baselineSet = false
	r.sawControlKeys = false
	r.redisFailSince = time.Time{}
	r.lastIncremental = time.Time{}
	r.generation, r.cvm, r.wm, r.dbNow = "", 0, 0, 0
}

func (r *Reconciler) probe(ctx context.Context) (Probe, error) {
	opCtx, cancel := context.WithTimeout(ctx, tickOpTimeout)
	defer cancel()
	p, err := r.cache.Probe(opCtx)
	if err != nil {
		return Probe{}, err
	}
	if p.Identity.Role != "master" {
		return Probe{}, fmt.Errorf("mapping redis role is %q, not master", p.Identity.Role)
	}
	return p, nil
}

// onRedisFailure 记一次读不到 mapping Redis。连续失败超过 RedisFailureReleaseMs 就让出租约(核实意见 should「连不上 Redis 占着租约」),
// 并在同样长的时间内不再抢,让连得上 Redis 的副本接手。撤不了标记(撤不了):读方自己按 M1 判身份。
func (r *Reconciler) onRedisFailure(ctx context.Context, now time.Time, what string, err error) string {
	if r.redisFailSince.IsZero() {
		r.redisFailSince = now
	}
	failing := now.Sub(r.redisFailSince)
	logx.Errorf("[RouteReconcile] mapping redis %s failed (failing for %s): %v", what, failing, err)
	if failing < r.opts.redisFailureRelease {
		return metrics.RouteReconcileTickRedisError
	}
	relCtx, cancel := context.WithTimeout(ctx, releaseTimeout)
	released, rerr := r.st.ReleaseReconcileLease(relCtx, r.leaderID)
	cancel()
	if rerr != nil {
		logx.Errorf("[RouteReconcile] releasing the lease after %s of redis failures failed (it expires on its own): %v", failing, rerr)
	}
	r.obs.Event(metrics.RouteReconcileEventLeaseReleasedRedisUnreachable)
	r.demote(fmt.Sprintf("mapping redis unreachable for %s (released=%t); holding off acquisition for %s", failing, released, r.opts.redisFailureRelease))
	r.acquireHoldoffUntil = now.Add(r.opts.redisFailureRelease)
	return metrics.RouteReconcileTickRedisError
}

// settleMarkerShape 处理就绪标记的形状:WRONGTYPE 无条件 DEL、畸形按原值 CAD(都计 marker_dropped_malformed),版本更新只记日志。
func (r *Reconciler) settleMarkerShape(ctx context.Context, ctl Control) markerState {
	ms := classifyMarker(ctl)
	switch ms.kind {
	case keyWrongType:
		opCtx, cancel := context.WithTimeout(ctx, tickOpTimeout)
		err := r.cache.DeleteReadyWrongType(opCtx)
		cancel()
		if err != nil {
			logx.Errorf("[RouteReconcile] %s is not a string; DEL failed: %v", placement.RouteReadyKey, err)
		} else {
			r.obs.Event(metrics.RouteReconcileEventMarkerDroppedMalformed)
			logx.Errorf("[RouteReconcile] %s was not a string (readers treat it as absent); deleted", placement.RouteReadyKey)
		}
		return markerState{kind: keyAbsent}
	case keyMalformed:
		r.dropMarker(ctx, &ms, metrics.RouteReconcileEventMarkerDroppedMalformed, "malformed marker value")
	case keyNewer:
		logx.Errorf("[RouteReconcile] %s has a newer format version than this binary; left untouched and not trusted", placement.RouteReadyKey)
	}
	return ms
}

// settleWatermarkShape 处理水位键的形状(核实意见 A1-8;此前三种情形都静默,M3 停摆没人知道):
//   - 不是 string:记 ERROR、无条件 DEL(计 marker_dropped_malformed),本拍按「不在」交给 advanceWatermarkKey 当拍 SET NX 重建。
//     不当成 FLUSH(checkCacheEvidence 不把它算「水位键不在」):只是有人把这一把键写坏了,映射键没动,不该触发全量追平;
//     不删的话 SET NX 每拍都因键在而失败,M3 永久失效。DEL 失败回 keyWrongType,本拍不写水位键;
//   - 畸形:记 ERROR(带原值)、计 marker_dropped_malformed;AdvanceReconcile 拿不到可判的值,本拍不做 M3,提交后按原值 CAS 覆盖;
//   - 版本更新:记 ERROR —— 本 leader 的 M3 回档检测停摆(不判也不写),要换新版本二进制。
//
// 畸形与版本更新每拍都在(冻结期也不覆盖),同一原值只在第一次见到时记日志与事件,免得刷屏、计数按秒涨。
func (r *Reconciler) settleWatermarkShape(ctx context.Context, ctl Control) watermarkState {
	if ctl.WatermarkWrongType {
		r.lastWatermarkShape = ""
		opCtx, cancel := context.WithTimeout(ctx, tickOpTimeout)
		err := r.cache.DeleteWatermarkWrongType(opCtx)
		cancel()
		if err != nil {
			logx.Errorf("[RouteReconcile] %s is not a string (M3 rollback detection cannot run); DEL failed: %v", placement.RouteWatermarkKey, err)
			return watermarkState{kind: keyWrongType}
		}
		r.obs.Event(metrics.RouteReconcileEventMarkerDroppedMalformed)
		logx.Errorf("[RouteReconcile] %s was not a string (M3 rollback detection skipped this tick); deleted, rebuilt from the database watermark this tick",
			placement.RouteWatermarkKey)
		return watermarkState{kind: keyAbsent}
	}
	wm := classifyWatermark(ctl.Watermark)
	switch wm.kind {
	case keyMalformed, keyNewer:
		shape := fmt.Sprintf("%d:%s", wm.kind, *wm.raw)
		if shape == r.lastWatermarkShape {
			return wm
		}
		r.lastWatermarkShape = shape
		if wm.kind == keyMalformed {
			r.obs.Event(metrics.RouteReconcileEventMarkerDroppedMalformed)
			logx.Errorf("[RouteReconcile] %s holds a malformed value %q: M3 rollback detection skipped; it is overwritten with the database watermark after the commit",
				placement.RouteWatermarkKey, *wm.raw)
		} else {
			logx.Errorf("[RouteReconcile] %s has a newer format version than this binary (%q): left untouched; M3 rollback detection is NOT running on this leader "+
				"(upgrade the data_service replicas)", placement.RouteWatermarkKey, *wm.raw)
		}
	default:
		r.lastWatermarkShape = ""
	}
	return wm
}

// dropMarker 按原值 CAD 就绪标记,成功时计 event(event 为空不计);ms 置为缺席(CAD 输了说明值已被换,下一拍重判)。
// 返回 false = CAD 本身出错(Redis 故障),标记可能还在,调用方本拍不要据「已撤」往下走。
func (r *Reconciler) dropMarker(ctx context.Context, ms *markerState, event, why string) bool {
	if ms.kind == keyAbsent {
		return true
	}
	opCtx, cancel := context.WithTimeout(ctx, tickOpTimeout)
	ok, err := r.cache.CompareAndDeleteReady(opCtx, ms.raw)
	cancel()
	switch {
	case err != nil:
		logx.Errorf("[RouteReconcile] compare-and-delete %s (%s) failed: %v", placement.RouteReadyKey, why, err)
		return false
	case ok:
		if event != "" {
			r.obs.Event(event)
		}
		logx.Errorf("[RouteReconcile] dropped %s: %s", placement.RouteReadyKey, why)
	default:
		logx.Infof("[RouteReconcile] %s changed before it could be dropped (%s); re-judged next tick", placement.RouteReadyKey, why)
	}
	*ms = markerState{kind: keyAbsent}
	return true
}

// checkControlKeysWiped:三把控制键都不在,而本任期见过它们 = 被 rewarm(一条 DEL 删三把)或 FLUSHALL / FLUSHDB 清掉。
// 闩锁、候选、VI 全部作废,重取 evicted_keys 基线(规格 §13.1 第 6 条);在途的带令牌任务也挂不上了,取消。
func (r *Reconciler) checkControlKeysWiped(ctl Control, p Probe) {
	if ctl.present() {
		r.sawControlKeys = true
		return
	}
	if !r.sawControlKeys {
		return
	}
	r.sawControlKeys = false
	logx.Errorf("[RouteReconcile] all three control keys are gone (rewarm, FLUSHALL or FLUSHDB): resetting arming state; evicted_keys baseline %d → %d",
		r.evictedBaseline, p.EvictedKeys)
	r.vi, r.candidate, r.latch, r.queuedRepair = nil, nil, nil, nil
	r.lastRefusal = ""
	r.pending = map[uint64]struct{}{}
	r.pendingOverflow = false
	r.baselineSet = p.EvictedKeys >= 0
	r.evictedBaseline = max(p.EvictedKeys, 0)
	r.baselineIdentity = p.Identity
	if r.running != nil && r.running.task.phase.tokened() {
		r.running.abort("control keys wiped")
	}
}

// checkRunningToken:在途的带令牌任务,令牌已不是它的(被 FLUSH、rewarm 或别的 leader 覆写)→ 它挂不上标记、也证明不了数据完整,取消。
func (r *Reconciler) checkRunningToken(ctl Control, p Probe) {
	rt := r.running
	if rt == nil || !rt.task.phase.tokened() || rt.aborted != "" {
		return
	}
	if ctl.Warm != nil && *ctl.Warm == rt.task.token && rt.task.identity == p.Identity {
		return
	}
	reason := "warm token replaced"
	if rt.task.identity != p.Identity {
		reason = "redis identity changed"
	}
	logx.Errorf("[RouteReconcile] cancelling %s seq=%d: %s", rt.task.phase, rt.task.seq, reason)
	rt.abort(reason)
	r.vi = nil
}

// checkCacheEvidence 判「副本对账到哪」的证据还在不在(核实意见 A1-1 / LR-2,包注释「偏离规格」第 1 条)。证据丢了 = 三把控制键都不在
// (空实例 / FLUSH / rewarm,不论本任期见没见过它们),或 Arm=false 下水位键不在(不是 string 的不算,见 settleWatermarkShape)。
// 这时「updated_at_ms < cvm − Overlap 的行都已推进缓存」不再成立:
//   - pendingCvm 换成 0,本拍 AdvanceReconcile 把库里的 cvm 写成 0。写进库,「要从 0 全量追平」才跨拍、跨任务失败、跨换届与滚动重启保留
//     (追平 / 修补起点都按库里的 cvm 算);写库在当拍 SET NX 重建水位键之前(advance 的顺序),库写失败时水位键不重建、下一拍再判。
//     冻结的拍库里不写(AlreadyFrozen 零写入),水位键也不重建,证据留在 Redis 里,解冻之后第一拍再判;
//   - 已收而未写的 pendingCvm(本拍开头刚收的任务结果)一并作废;
//   - 在途任务除「带令牌、从 0 起」的(预热、从 0 起的追平 / 修补:它们覆盖全表,FLUSH / rewarm 会换掉令牌,已由 checkRunningToken 取消)
//     之外一律取消 —— 尤其是不带令牌的增量:FLUSH 不改身份,它跑完会把 FLUSH 之前的起点 S 写成 cvm,追平又退化成 S − Lookback。
//
// 返回证据是否丢了(第 9 步本拍不起增量)。只有本任期第一次判丢时记 ERROR。
func (r *Reconciler) checkCacheEvidence(ctl Control, p Probe) bool {
	if ctl.present() && (r.opts.arm || !ctl.watermarkAbsent()) {
		r.cacheEvidenceLost = false
		return false
	}
	if !r.cacheEvidenceLost {
		r.cacheEvidenceLost = true
		logx.Errorf("[RouteReconcile] the cache verification evidence is gone (ready=%t warm=%t watermark=%t arm=%t): "+
			"cache_verified_ms %d is reset to 0, so the next catch-up / incremental re-pushes every row from updated_at_ms 0",
			ctl.Ready != nil, ctl.Warm != nil, !ctl.watermarkAbsent(), r.opts.arm, r.cvm)
	}
	zero := uint64(0)
	r.pendingCvm, r.pendingCvmIdentity = &zero, p.Identity
	if rt := r.running; rt != nil && rt.aborted == "" && !(rt.task.phase.tokened() && rt.task.fromMs == 0) {
		logx.Errorf("[RouteReconcile] cancelling %s seq=%d (from_ms=%d): the cache verification evidence is gone", rt.task.phase, rt.task.seq, rt.task.fromMs)
		rt.abort("cache verification evidence lost")
	}
	return true
}

// advance 是第 4 步。返回 (tick 结果, 是否继续)。冻结时在这里收尾(撤该撤的标记、写观测)。
func (r *Reconciler) advance(ctx context.Context, now time.Time, wm watermarkState, ms *markerState, p Probe) (string, bool) {
	// 冻结前本地剩余租约不够一次冻结事务 + 安全余量,先续约(规格 §4.2「TiDB 缓存表冻结超时」)。
	if r.localDeadline.Sub(now) < r.opts.freezeTimeout+r.opts.leaseSafety {
		if res, ok := r.renew(ctx, now); !ok {
			return res, false
		}
	}
	if r.cacheAheadPending {
		fr, err := r.st.FreezeByLeader(ctx, r.leaderID, store.RouteFreezeCacheAhead, r.opts.freezeTimeout)
		if err != nil {
			r.handleDanger(ctx, err)
			logx.Errorf("[RouteReconcile] retrying the cache_ahead freeze failed: %v", err)
			return metrics.RouteReconcileTickDBError, false
		}
		r.cacheAheadPending = false
		switch fr.Outcome {
		case store.ReconcileTickLeaseLost:
			r.demote("lease lost while freezing for cache_ahead")
			return metrics.RouteReconcileTickLeaseLost, false
		case store.ReconcileTickFrozen:
			r.obs.Event(metrics.RouteReconcileEventFreezeCacheAhead)
		}
	}
	var key *placement.RouteWatermark
	if wm.kind == keyValid {
		w := wm.w
		key = &w
	}
	res, err := r.st.AdvanceReconcile(ctx, store.ReconcileTickRequest{
		LeaderID:           r.leaderID,
		WatermarkKey:       key,
		ToleranceMs:        r.opts.toleranceMs,
		SetCacheVerifiedMs: r.pendingCvm,
		FreezeTimeout:      r.opts.freezeTimeout,
	})
	if err != nil {
		r.handleDanger(ctx, err)
		logx.Errorf("[RouteReconcile] advance: %v", err)
		return metrics.RouteReconcileTickDBError, false
	}
	r.generation, r.cvm, r.wm, r.dbNow = res.Generation, res.CacheVerifiedMs, res.WatermarkMs, res.NowMs
	switch res.Outcome {
	case store.ReconcileTickLeaseLost:
		r.demote("AdvanceReconcile found the lease lost")
		return metrics.RouteReconcileTickLeaseLost, false
	case store.ReconcileTickFrozen:
		r.pendingCvm = nil
		r.obs.Event(freezeEvent(res.FreezeReason))
		logx.Errorf("[RouteReconcile] FROZE the route writers: %s (watermark key %+v, db watermark_ms=%d generation=%s)",
			res.FreezeReason, key, res.WatermarkMs, res.Generation)
		return r.onFrozen(ctx, ms, p), false
	case store.ReconcileTickAlreadyFrozen:
		r.pendingCvm = nil
		return r.onFrozen(ctx, ms, p), false
	case store.ReconcileTickAdvanced:
	default:
		logx.Errorf("[RouteReconcile] advance returned an unexpected outcome %s", res.Outcome)
		return metrics.RouteReconcileTickDBError, false
	}
	r.pendingCvm = nil

	// 标记世代 ≠ 库世代:标记属于另一份库(库被重建 / 换库),冻结(scout D4 保留的世代判据;水位键缺席时只有它能发现)。
	if ms.kind == keyValid && ms.m.Generation != res.Generation {
		fr, err := r.st.FreezeByLeader(ctx, r.leaderID, store.RouteFreezeGenerationMismatch, r.opts.freezeTimeout)
		if err != nil {
			r.handleDanger(ctx, err)
			logx.Errorf("[RouteReconcile] marker generation %s != db generation %s; freezing failed: %v", ms.m.Generation, res.Generation, err)
			return metrics.RouteReconcileTickDBError, false
		}
		switch fr.Outcome {
		case store.ReconcileTickLeaseLost:
			r.demote("lease lost while freezing for a marker generation mismatch")
			return metrics.RouteReconcileTickLeaseLost, false
		case store.ReconcileTickFrozen:
			r.obs.Event(metrics.RouteReconcileEventFreezeGeneration)
			logx.Errorf("[RouteReconcile] FROZE the route writers: marker generation %s != db generation %s", ms.m.Generation, res.Generation)
		}
		return r.onFrozen(ctx, ms, p), false
	}
	r.advanceWatermarkKey(ctx, wm, res)
	return "", true
}

func freezeEvent(reason store.RouteFreezeReason) string {
	switch reason {
	case store.RouteFreezeGenerationMismatch:
		return metrics.RouteReconcileEventFreezeGeneration
	case store.RouteFreezeCacheAhead:
		return metrics.RouteReconcileEventFreezeCacheAhead
	default:
		return metrics.RouteReconcileEventFreezeWatermark
	}
}

// onFrozen:冻结期停止一切推送(取消在途任务、不起新任务、不推进水位键);标记不动,除非它已不可信
// (身份不符、淘汰异常、DEBUG 开着)或 Arm=false(撤销信任永远安全)。
func (r *Reconciler) onFrozen(ctx context.Context, ms *markerState, p Probe) string {
	if r.running != nil {
		r.running.abort("route writers frozen")
	}
	// 冻结期间库可能正处在回档之后:之前完成的预热 / 修补结果都不再作数,解冻之后按标记现状重新判(多半是全量预热)。
	r.candidate, r.queuedRepair, r.latch, r.lastRefusal = nil, nil, nil, ""
	if ms.kind == keyValid || (ms.kind == keyNewer && !r.opts.arm) {
		evBad, _ := r.evictionState(p)
		switch {
		case !r.opts.arm:
			r.dropMarker(ctx, ms, metrics.RouteReconcileEventMarkerDroppedDisarmed, "ArmReadyMarker=false (frozen)")
		case ms.m.RedisRunID != p.Identity.RunID || ms.m.MasterReplID != p.Identity.MasterReplID:
			r.dropMarker(ctx, ms, metrics.RouteReconcileEventMarkerDroppedIdentity, "redis identity changed (frozen)")
		case evBad:
			r.dropMarker(ctx, ms, metrics.RouteReconcileEventMarkerDroppedEviction, "eviction possible (frozen)")
		case p.DebugCommand != "no":
			r.dropMarker(ctx, ms, metrics.RouteReconcileEventArmingRefusedDebugEnabled, "enable-debug-command is not no (frozen)")
		}
	}
	r.vi = nil
	r.observe(ctx, false, ms.kind != keyAbsent)
	return metrics.RouteReconcileTickFrozen
}

// advanceWatermarkKey 在本拍提交之后单调推进水位键(M3):GET(本拍 MGET 的原值)→ 判 → 按原值 CAS / 缺席时 SET NX。
// 只增不减、世代不同不写(那种情况 AdvanceReconcile 已冻结);版本更新的值不碰、不是 string 而 DEL 失败的不写。CAS 输了下一拍再来。
// 本地租约过点不写(核实意见 LR-10):过期 leader 迟到的 SET NX 会在 FLUSH 之后替新 leader 重建水位键,把「证据已丢」盖掉。
func (r *Reconciler) advanceWatermarkKey(ctx context.Context, wm watermarkState, res store.ReconcileTickResult) {
	if res.WatermarkMs == 0 || res.Generation == "" || !r.leaseValid() {
		return
	}
	switch wm.kind {
	case keyNewer, keyWrongType:
		return
	case keyValid:
		if wm.w.Generation != res.Generation || wm.w.WatermarkMs >= res.WatermarkMs {
			return
		}
	}
	next, err := placement.EncodeRouteWatermark(placement.RouteWatermark{Generation: res.Generation, WatermarkMs: res.WatermarkMs})
	if err != nil {
		logx.Errorf("[RouteReconcile] encode the watermark key: %v", err)
		return
	}
	opCtx, cancel := context.WithTimeout(ctx, tickOpTimeout)
	ok, err := r.cache.AdvanceWatermark(opCtx, wm.raw, next)
	cancel()
	switch {
	case err != nil:
		logx.Errorf("[RouteReconcile] advance %s: %v", placement.RouteWatermarkKey, err)
	case !ok:
		logx.Infof("[RouteReconcile] %s changed under us; retried next tick", placement.RouteWatermarkKey)
	}
}

// takeEvictedBaseline 取 evicted_keys 的基线(规格 §4.2「挂标记被拒后无限预热」:基线记在 leader 内存,换届时以新 leader
// 首拍读到的值为基线,淘汰类原因在身份变化后解除)。本任期首拍、或 Redis 身份变了(新进程 / 被提升的副本:计数器是那个进程
// 自己的累计值,和旧基线不可比)时取这一拍的值;同一实例上基线不动,计数器比它大 = 本任期内发生过淘汰。取不到(-1)时不取,
// evictionState 按「取不到」fail-closed。
func (r *Reconciler) takeEvictedBaseline(p Probe) {
	if p.EvictedKeys < 0 || (r.baselineSet && r.baselineIdentity == p.Identity) {
		return
	}
	if r.baselineSet {
		logx.Infof("[RouteReconcile] redis identity changed (run_id %s → %s): evicted_keys baseline %d → %d",
			r.baselineIdentity.RunID, p.Identity.RunID, r.evictedBaseline, p.EvictedKeys)
	}
	r.evictedBaseline, r.baselineSet, r.baselineIdentity = p.EvictedKeys, true, p.Identity
}

// evictionState:bad = 淘汰可能发生(策略不是 noeviction)或已经发生(evicted_keys 比基线大、或取不到);
// taint = 确实发生过淘汰(比基线大)—— 等待中的挂标记候选作废。
func (r *Reconciler) evictionState(p Probe) (bad, taint bool) {
	policyOK := p.MaxmemoryPolicy == "noeviction" || (p.MaxmemoryPolicy == "" && r.opts.assumeNoEviction)
	switch {
	case p.EvictedKeys < 0 || !r.baselineSet:
		return true, false
	case p.EvictedKeys > r.evictedBaseline:
		return true, true
	}
	return !policyOK, false
}

// blockers 是挂标记的阻塞原因(规格 scout D3 + §13.1 第 3、5 条)。
type blockers struct {
	eviction, debug, unresolved bool
}

func (b blockers) any() bool { return b.eviction || b.debug || b.unresolved }

// state 报告是否有「状态类」原因(与任务结果无关,不跑预热也清不掉)。
func (b blockers) state() bool { return b.eviction || b.debug }

func (b blockers) key() string {
	var parts []string
	if b.eviction {
		parts = append(parts, "eviction")
	}
	if b.debug {
		parts = append(parts, "debug_enabled")
	}
	if b.unresolved {
		parts = append(parts, "unresolved")
	}
	return strings.Join(parts, ",")
}

// reportRefusal 在阻塞原因变化时计事件、打 ERROR(每拍都在拒绝,只在变化时计数,免得计数器按秒涨)。
func (r *Reconciler) reportRefusal(b blockers) {
	k := b.key()
	if k == r.lastRefusal {
		return
	}
	r.lastRefusal = k
	if b.eviction {
		r.obs.Event(metrics.RouteReconcileEventArmingRefusedEviction)
	}
	if b.debug {
		r.obs.Event(metrics.RouteReconcileEventArmingRefusedDebugEnabled)
	}
	if b.unresolved {
		r.obs.Event(metrics.RouteReconcileEventArmingRefusedUnresolved)
	}
	sample := r.pendingIDs()
	if len(sample) > unresolvedLogSample {
		sample = sample[:unresolvedLogSample]
	}
	logx.Errorf("[RouteReconcile] arming %s REFUSED (armingBlocked: no new full prewarm; incrementals re-read the unresolved rows): "+
		"reasons=%s pending=%d overflow=%t sample=%v", placement.RouteReadyKey, k, len(r.pending), r.pendingOverflow, sample)
}

// decideDisarmed 是 Arm=false 的第 7 步:VI 为空(或身份变了)排追平,起点 max(0, cvm − RepairLookbackMs),cvm 取本拍库里的值。
// 「水位键不在 = 从 0 全量追平」不在这里按本拍读到的水位键判(那只在发现缺席的一拍里成立:同拍就 SET NX 重建了),而是由
// checkCacheEvidence 当拍把库里的 cvm 写成 0 —— 追平中途失败、那一拍恰有增量在跑、追平途中换 leader,下一次追平仍从 0 起
// (核实意见 A1-1 / LR-2)。
func (r *Reconciler) decideDisarmed(ctx context.Context, p Probe) {
	if r.vi != nil && *r.vi != p.Identity {
		logx.Errorf("[RouteReconcile] redis identity changed (run_id %s → %s); catching up", r.vi.RunID, p.Identity.RunID)
		r.vi = nil
	}
	if r.vi != nil || r.running != nil || r.startedThisTick {
		return
	}
	r.startTask(ctx, task{phase: phaseCatchUp, fromMs: satSub(r.cvm, r.opts.lookbackMs), identity: p.Identity})
}

// decideArmed 是 Arm=true 的第 8 步(规格 §4.3 第 8 步按 §13.1 第 1 条改写)。
func (r *Reconciler) decideArmed(ctx context.Context, ms *markerState, p Probe, b blockers, evTaint bool) {
	switch ms.kind {
	case keyNewer:
		r.vi = nil
		return
	case keyValid:
		if ms.m.RedisRunID == p.Identity.RunID && ms.m.MasterReplID == p.Identity.MasterReplID {
			id := p.Identity
			r.vi = &id
			r.candidate, r.latch, r.queuedRepair, r.lastRefusal = nil, nil, nil, ""
			r.raiseMarkerWatermark(ctx, ms)
			return
		}
		// 标记在、身份不符:自身 AOF / RDB 重启、副本提升 —— 标记随数据一起延续,回看修补成立(M5)。先关门(CAD),再修补。
		repairFrom := satSub(min(ms.m.WatermarkMs, r.cvm), r.opts.lookbackMs)
		r.vi, r.candidate, r.latch, r.lastRefusal = nil, nil, nil, ""
		if !r.dropMarker(ctx, ms, metrics.RouteReconcileEventMarkerDroppedIdentity,
			fmt.Sprintf("redis identity changed (marker run_id=%s replid=%s, now run_id=%s replid=%s)",
				ms.m.RedisRunID, ms.m.MasterReplID, p.Identity.RunID, p.Identity.MasterReplID)) {
			return // 门没关上就不修补:下一拍重来
		}
		r.queuedRepair = &task{phase: phaseRepair, fromMs: repairFrom, identity: p.Identity}
		if r.running != nil {
			r.running.abort("identity changed; repair queued")
			return
		}
		r.startQueuedRepair(ctx, p)
		return
	}

	// 标记不在。
	r.vi = nil
	if r.running != nil || r.startedThisTick {
		return
	}
	if r.queuedRepair != nil {
		r.startQueuedRepair(ctx, p)
		return
	}
	// 未决只看记下的 id(核实意见 A1-3 / LR-3):溢出(超过 ArmingBlockedReportLimit、记不全)若也算未决,记下的 id 全部解决之后
	// 闩锁仍永不解除,下面「解除后全量预热」那一支走不到,只能靠 rewarm / 换届解开。溢出在闩锁解除与候选两支里各自处理:
	// 记下的还没解决 → 照旧闩住(不重跑预热);都解决了 → 丢掉记不全的候选,全量预热重新发现(startTask 复位溢出)。
	b.unresolved = len(r.pending) > 0
	if r.latch != nil {
		if r.latch.identity != p.Identity {
			logx.Errorf("[RouteReconcile] redis identity changed while arming was blocked; full prewarm")
			r.latch, r.candidate, r.lastRefusal = nil, nil, ""
		} else {
			if evTaint {
				r.latch.evictionTaint = true
			}
			if b.any() {
				r.reportRefusal(b)
				return
			}
			l := r.latch
			r.latch, r.lastRefusal = nil, ""
			if l.candidate != nil && !l.evictionTaint && !r.pendingOverflow {
				r.tryArm(ctx, l.candidate, p)
				return
			}
			logx.Infof("[RouteReconcile] arming unblocked but the blocked result is not usable (candidate=%t eviction_taint=%t overflow=%t); full prewarm",
				l.candidate != nil, l.evictionTaint, r.pendingOverflow)
			r.candidate = nil
		}
	}
	if c := r.candidate; c != nil {
		if c.identity != p.Identity || c.generation != r.generation {
			logx.Errorf("[RouteReconcile] the %s result no longer matches (identity %t generation %t); full prewarm",
				c.phase, c.identity == p.Identity, c.generation == r.generation)
			r.candidate = nil
		} else {
			if b.any() {
				r.latch = &armLatch{candidate: c, identity: p.Identity, evictionTaint: evTaint}
				r.candidate = nil
				r.reportRefusal(b)
				return
			}
			r.candidate = nil
			if !r.pendingOverflow {
				r.tryArm(ctx, c, p)
				return
			}
			// 记下的未决都解决了,但溢出过:还有没记下的未决行,这份结果不能拿去挂标记。
			logx.Infof("[RouteReconcile] the %s result is not usable: unresolved rows overflowed ArmingBlockedReportLimit=%d; full prewarm",
				c.phase, r.opts.pendingLimit)
		}
	}
	if b.state() {
		// 淘汰 / DEBUG 这类状态原因下,全量预热挂不上标记:先闩住,原因清掉再预热(不白跑一遍全表)。
		r.latch = &armLatch{identity: p.Identity, evictionTaint: evTaint}
		r.reportRefusal(b)
		return
	}
	r.startTask(ctx, task{phase: phasePrewarm, identity: p.Identity})
}

func (r *Reconciler) startQueuedRepair(ctx context.Context, p Probe) {
	q := r.queuedRepair
	if q.identity != p.Identity {
		logx.Errorf("[RouteReconcile] redis identity changed again before the queued repair started; the marker is absent, so a full prewarm follows")
		r.queuedRepair = nil
		return
	}
	if r.startTask(ctx, *q) {
		r.queuedRepair = nil
	}
}

// raiseMarkerWatermark:标记在且可信时,把标记的 watermark_ms 推进到 cvm(规格 §13.1 第 2 条:标记水位 = 缓存已验证到的点)。只增不减。
func (r *Reconciler) raiseMarkerWatermark(ctx context.Context, ms *markerState) {
	if r.cvm == 0 || r.cvm <= ms.m.WatermarkMs {
		return
	}
	next := ms.m
	next.WatermarkMs = r.cvm
	raw, err := placement.EncodeRouteReadyMarker(next)
	if err != nil {
		logx.Errorf("[RouteReconcile] encode the marker with watermark_ms=%d: %v", r.cvm, err)
		return
	}
	opCtx, cancel := context.WithTimeout(ctx, tickOpTimeout)
	ok, err := r.cache.CompareAndSetReady(opCtx, ms.raw, raw)
	cancel()
	switch {
	case err != nil:
		logx.Errorf("[RouteReconcile] compare-and-set the marker watermark: %v", err)
	case ok:
		ms.raw, ms.m = raw, next
	}
}

// tryArm 挂标记(规格 §4.3 末段、§13.1 第 7 条):再 Peek 一次库租约(必须仍是自己)→ RouteReadyArmScript(令牌、标记值、身份)
// + 同连接 WAITAOF → 复读身份,变了就按原值撤掉自己的值。标记水位写 cvm,set_at_ms 取库时钟。
func (r *Reconciler) tryArm(ctx context.Context, c *armCandidate, p Probe) {
	opCtx, cancel := context.WithTimeout(ctx, tickOpTimeout)
	lease, err := r.st.PeekReconcileLease(opCtx)
	cancel()
	if err != nil {
		logx.Errorf("[RouteReconcile] arming postponed: peek lease: %v", err)
		r.candidate = c
		return
	}
	if !lease.HeldBy(r.leaderID) {
		r.demote(fmt.Sprintf("the lease is held by %q at arming time", lease.Holder))
		return
	}
	if r.cvm == 0 {
		logx.Errorf("[RouteReconcile] arming postponed: cache_verified_ms is still 0")
		r.candidate = c
		return
	}
	marker := placement.RouteReadyMarker{
		V:            placement.RouteReadyFormatVersion,
		Generation:   r.generation,
		RedisRunID:   c.identity.RunID,
		MasterReplID: c.identity.MasterReplID,
		SetAtMs:      lease.NowMs,
		WatermarkMs:  r.cvm,
		Leader:       r.leaderID,
	}
	opCtx, cancel = context.WithTimeout(ctx, tickOpTimeout)
	out, err := r.cache.ArmReady(opCtx, c.token, marker)
	cancel()
	if err != nil {
		logx.Errorf("[RouteReconcile] arming failed (retried next tick): %v", err)
		r.candidate = c
		return
	}
	switch out {
	case placement.RouteReadyArmArmed:
	case placement.RouteReadyArmTokenMismatch:
		logx.Errorf("[RouteReconcile] arming refused by the warm token (FLUSH / rewarm / another leader since the %s started): "+
			"the data may be incomplete; full prewarm next tick", c.phase)
		r.vi = nil
		return
	case placement.RouteReadyArmAlreadyPresent:
		logx.Infof("[RouteReconcile] arming found %s already present; re-judged next tick", placement.RouteReadyKey)
		return
	case placement.RouteReadyArmIdentityChanged:
		logx.Errorf("[RouteReconcile] arming refused: the redis identity is no longer the one the %s verified", c.phase)
		r.vi = nil
		return
	default:
		logx.Errorf("[RouteReconcile] arming returned %s (internal bug)", out)
		return
	}
	// 挂上了:复读身份,变了就撤掉自己写的值(scout D3)。
	raw, encErr := placement.EncodeRouteReadyMarker(marker)
	after, err := r.probe(ctx)
	if err != nil || after.Identity != c.identity {
		if encErr == nil {
			opCtx, cancel = context.WithTimeout(ctx, tickOpTimeout)
			_, derr := r.cache.CompareAndDeleteReady(opCtx, raw)
			cancel()
			if derr != nil {
				logx.Errorf("[RouteReconcile] dropping our own marker after an identity re-check failure also failed: %v", derr)
			}
		}
		r.obs.Event(metrics.RouteReconcileEventMarkerDroppedIdentity)
		logx.Errorf("[RouteReconcile] armed, but the identity re-check failed (err=%v); our marker was dropped", err)
		r.vi = nil
		return
	}
	id := c.identity
	r.vi = &id
	r.lastRefusal = ""
	r.obs.Event(metrics.RouteReconcileEventMarkerArmed)
	logx.Infof("[RouteReconcile] ARMED %s after %s: generation=%s run_id=%s watermark_ms(cvm)=%d set_at_ms=%d",
		placement.RouteReadyKey, c.phase, r.generation, c.identity.RunID, r.cvm, lease.NowMs)
}

// observe 是第 10 步:自检(RouteReadyReadScript 不带映射键,读方同一判据)并写 leader 观测型 gauge(规格 §13.1 第 8 条)。
func (r *Reconciler) observe(ctx context.Context, writable, markerPresent bool) {
	verdict := metrics.RouteReadySelfCheckScriptError
	opCtx, cancel := context.WithTimeout(ctx, tickOpTimeout)
	rr, err := r.cache.SelfCheck(opCtx)
	cancel()
	if err != nil {
		logx.Errorf("[RouteReconcile] self-check (route ready read script) failed: %v", err)
	} else {
		v, _ := placement.VerifyRouteReady(rr)
		verdict = v.String()
		markerPresent = rr.MarkerPresent
	}
	age := math.Inf(1)
	if r.cvm > 0 {
		age = float64(satSub(r.dbNow, r.cvm)) / 1000
	}
	r.obs.LeaderObservations(markerPresent, writable, age, verdict)
}

// satSub 是饱和减法(规格 §4.2 must-1:所有 X − Lookback / Overlap 饱和到 0,不回绕)。
func satSub(a, b uint64) uint64 {
	if a <= b {
		return 0
	}
	return a - b
}

// newWarmToken 生成 32 个小写十六进制字符的预热令牌(placement.ValidRouteWarmToken)。
func newWarmToken(rnd io.Reader) (string, error) {
	var b [placement.RouteWarmTokenLen / 2]byte
	if _, err := io.ReadFull(rnd, b[:]); err != nil {
		return "", fmt.Errorf("read random warm token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func (r *Reconciler) handleDanger(ctx context.Context, err error) {
	var danger *store.RouteEmergencyError
	if !errors.As(err, &danger) {
		return
	}
	r.halted.Store(true)
	r.dangerOnce.Do(func() {
		reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.opts.freezeTimeout)
		defer cancel()
		if reportErr := r.onDanger(reportCtx, danger); reportErr != nil {
			logx.Errorf("[RouteReconcile] 关闭接纳或保存危险原件未确认，保持维护: %v", reportErr)
		}
	})
}

// SettledDigest只在整个worker、派生任务、SQL租约和Redis调用已确定join后可用。
func (r *Reconciler) SettledDigest(ctx context.Context) ([]byte, error) {
	if r.effects == nil {
		return nil, drainproof.ErrHold
	}
	return r.effects.SettledDigest(ctx)
}

// 同一Session跟踪所有有副作用的依赖调用；错误/恐慌永久HOLD，不因后续成功拍清零。
func trackEffect(ctx context.Context, effects *drainproof.Session, call func() error) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	done, err := effects.Enter()
	if err != nil {
		return err
	}
	returned := false
	defer func() {
		if !returned || err != nil {
			effects.Hold()
		}
		done()
	}()
	err = call()
	returned = true
	return err
}

type drainStore struct {
	Store
	effects *drainproof.Session
}

func (s *drainStore) RenewReconcileLease(ctx context.Context, id string, ms uint64) (out store.RouteLease, err error) {
	err = trackEffect(ctx, s.effects, func() error { var e error; out, e = s.Store.RenewReconcileLease(ctx, id, ms); return e })
	return
}
func (s *drainStore) ReleaseReconcileLease(ctx context.Context, id string) (out bool, err error) {
	err = trackEffect(ctx, s.effects, func() error { var e error; out, e = s.Store.ReleaseReconcileLease(ctx, id); return e })
	return
}
func (s *drainStore) AdvanceReconcile(ctx context.Context, req store.ReconcileTickRequest) (out store.ReconcileTickResult, err error) {
	err = trackEffect(ctx, s.effects, func() error { var e error; out, e = s.Store.AdvanceReconcile(ctx, req); return e })
	return
}
func (s *drainStore) FreezeByLeader(ctx context.Context, id string, reason store.RouteFreezeReason, timeout time.Duration) (out store.ReconcileTickResult, err error) {
	err = trackEffect(ctx, s.effects, func() error { var e error; out, e = s.Store.FreezeByLeader(ctx, id, reason, timeout); return e })
	return
}

type drainCache struct {
	Cache
	effects *drainproof.Session
}

func (c *drainCache) SetWarmToken(ctx context.Context, v string) error {
	return trackEffect(ctx, c.effects, func() error { return c.Cache.SetWarmToken(ctx, v) })
}
func (c *drainCache) ArmReady(ctx context.Context, v string, m placement.RouteReadyMarker) (out placement.RouteReadyArmOutcome, err error) {
	err = trackEffect(ctx, c.effects, func() error { var e error; out, e = c.Cache.ArmReady(ctx, v, m); return e })
	return
}
func (c *drainCache) CompareAndSetReady(ctx context.Context, old, next string) (out bool, err error) {
	err = trackEffect(ctx, c.effects, func() error { var e error; out, e = c.Cache.CompareAndSetReady(ctx, old, next); return e })
	return
}
func (c *drainCache) CompareAndDeleteReady(ctx context.Context, old string) (out bool, err error) {
	err = trackEffect(ctx, c.effects, func() error { var e error; out, e = c.Cache.CompareAndDeleteReady(ctx, old); return e })
	return
}
func (c *drainCache) DeleteReadyWrongType(ctx context.Context) error {
	return trackEffect(ctx, c.effects, func() error { return c.Cache.DeleteReadyWrongType(ctx) })
}
func (c *drainCache) DeleteWatermarkWrongType(ctx context.Context) error {
	return trackEffect(ctx, c.effects, func() error { return c.Cache.DeleteWatermarkWrongType(ctx) })
}
func (c *drainCache) AdvanceWatermark(ctx context.Context, prev *string, next string) (out bool, err error) {
	err = trackEffect(ctx, c.effects, func() error { var e error; out, e = c.Cache.AdvanceWatermark(ctx, prev, next); return e })
	return
}
func (c *drainCache) Push(ctx context.Context, rows []store.RouteRow) (out []PushRowResult, err error) {
	err = trackEffect(ctx, c.effects, func() error {
		var e error
		out, e = c.Cache.Push(ctx, rows)
		for _, r := range out {
			if r.Outcome == PushError {
				c.effects.Hold()
			}
		}
		return e
	})
	return
}
func (c *drainCache) MirrorMergedInto(ctx context.Context, rows []store.MergedIntoRow) (out MergedIntoMirrorResult, err error) {
	err = trackEffect(ctx, c.effects, func() error {
		var e error
		out, e = c.Cache.MirrorMergedInto(ctx, rows)
		if out.Errors != 0 {
			c.effects.Hold()
		}
		return e
	})
	return
}
