package inbox

import (
	"context"
	"errors"
	mathrand "math/rand/v2"
	"shared/drainproof"
	"sync"
	"time"

	"player_inbox/internal/constants"
	"player_inbox/internal/metrics"

	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
)

// 后台循环(设计 §6.3、§9.1、§9.3、§9.4、§10):清理、提醒补发、扫描型 gauge、对账。
//
// **只在领导者上跑**:领导者由 go/shared/leader 选举(核心 Redis 上的 player_inbox:maintenance:leader)。
//   - 清理只删终态行、逐行 autocommit、不持流行锁;领导者切换期间重叠执行无害(删除幂等)。
//   - 补发的每一步都是 next_seq CAS,重叠执行最多多发一次提醒。
//   - gauge 只由领导者产出,卸任时归零,避免 max() 聚合里留着卸任副本的旧值。
//   - 对账只读;不一致行数是领导者产出的 gauge,重叠期间两个副本各报一份,告警取 max(),不影响结论。
//     当选后第一轮对账只等 1–5 分钟(随机错开),不等满一个间隔;之后按间隔跑,起始分片按墙钟轮转(评审 M5 / L4),
//     每轮前移 reconcileStartStride 片;gauge 是本任期各分片最近一次读数之和(reconcileTally,评审 R1)。
//
// 关停(设计 §10):先取消循环与选举,有界等待它们退出,再由装配层关连接池 —— Run 返回即全部轮次已结束。

// LoopConfig 是后台循环参数(来自 config.PlayerInboxConf)。
type LoopConfig struct {
	TerminalRetention time.Duration
	SweepInterval     time.Duration
	NotifyInterval    time.Duration
	GaugeInterval     time.Duration
	// ReconcileInterval 为 0 = 不跑对账。开着时 ReconcileWindow 必须 ≥ 2 × ReconcileInterval(同 config.Validate)。
	ReconcileInterval time.Duration
	ReconcileWindow   time.Duration
	// Jitter 返回 [0, n) 内的随机时长,只用来错开当选后第一轮对账(reconcileFirstDelay)。
	// nil = math/rand/v2;单测注入确定值。
	Jitter func(n time.Duration) time.Duration
}

// 每轮的工作量上限。
const (
	// sweepBudgetPerRound:每轮最多删 500 行(设计 §9.1)。
	sweepBudgetPerRound = 500
	// notifyPerShardLimit:补发每个分片每轮最多 100 条(设计 §6.3)。
	notifyPerShardLimit = 100
	// notifyGrace:补发只扫 notify_due_ms ≤ now − 5s 的行,不与请求内那次提醒抢(设计 §6.3)。
	notifyGrace = 5 * time.Second
	// notifyOverdue:提醒积压超过 5 分钟计入 player_inbox_notify_backlog(设计 §9.4 warning)。
	notifyOverdue = 5 * time.Minute
	// deliveredStaleAfter:最后一次投递早于 now − 24h 的已投递未确认行算陈旧(评审 M4):不进 delivered_unacked /
	// delivered_unacked_oldest_seconds / max_delivery_count(告警口径),只进 info 级的 delivered_unacked_stale。
	// 典型来源是 GM 处置后紧接着封号踢下线:实体销毁时 ackQueue 跟着丢,玩家不再上线,这一行就永远已投递未确认。
	// 取 24h:「卡住」告警(> 1h 持续 10 分钟)仍有约 23 小时可见;玩家一天内回来,落地 Fetch 会再投递、刷新它。
	deliveredStaleAfter = 24 * time.Hour
	// roundBudgetCap:单轮的整体上限,避免一轮拖到下一轮之后;关停时 ctx 取消立即生效。
	roundBudgetCap = 2 * time.Minute
	// reconcileFirstDelayMin / reconcileFirstDelaySpread:当选后第一轮对账在 [1, 5) 分钟内随机起跑(评审 L4)。
	// 不等满一个间隔:滚动发布每次都会易主,等满 60 分钟就等于窗口内的行少核对一轮;
	// 也不当选即跑:启动风暴期不抢库,随机量把接连几次易主后的首轮错开。
	reconcileFirstDelayMin    = time.Minute
	reconcileFirstDelaySpread = 4 * time.Minute
)

// reconcile_rounds_total{result} 的取值;"" = 不计数(领导权丢失 / 关停造成的取消)。
const (
	reconcileRoundOK      = "ok"
	reconcileRoundError   = "error"
	reconcileRoundTimeout = "timeout"
)

// Campaign 是「竞选领导者并在任期内运行 whileLeader」的函数形状,生产为 (*leader.Elector).Run。
// 抽成函数让「只在领导者上执行」可单测:假实现可以从不授予领导权。
type Campaign func(ctx context.Context, whileLeader func(ctx context.Context))

// MaintenanceStore 是后台循环需要的存储能力(*Store 实现它;单测注入假实现)。
type MaintenanceStore interface {
	SweepTerminal(ctx context.Context, cutoffMs uint64, budget int) (int, error)
	DueNotifications(ctx context.Context, shard uint32, dueBeforeMs uint64, limit int) ([]NotifyDue, error)
	// ResendNotify 补发一条;ctx 是本轮的 ctx,单次提醒的预算由实现自己加(没落定时的推迟不能被提醒耗尽的预算拖累)。
	ResendNotify(ctx context.Context, d NotifyDue) NotifyResult
	ScanGauges(ctx context.Context, nowMs uint64, cut GaugeCutoffs) (metrics.ScanGauges, error)
	ReconcileRecent(ctx context.Context, sinceMs uint64, startShard uint32, ledgers LedgerReader) (ReconcileRoundResult, error)
}

// Loops 持有后台循环的依赖。构造后只读。
type Loops struct {
	// 启动前显式绑定；清理是 SOURCE，通知与对账承接历史工作归 RELAY。
	RootAdmission  *drainproof.Session
	RelayAdmission *drainproof.Session
	store          MaintenanceStore
	ledgers        LedgerReader
	cfg            LoopConfig
	campaign       Campaign
	now            func() time.Time
	jitter         func(time.Duration) time.Duration
}

// NewLoops 校验参数并构造。ledgers 可为 nil:此时不跑对账(只在 ReconcileInterval > 0 时要求非 nil)。
// now 为 nil 时取 time.Now。
func NewLoops(store MaintenanceStore, ledgers LedgerReader, cfg LoopConfig, campaign Campaign, now func() time.Time) (*Loops, error) {
	if store == nil || campaign == nil {
		return nil, errors.New("inbox: NewLoops requires store and campaign")
	}
	if now == nil {
		now = time.Now
	}
	if cfg.TerminalRetention <= constants.IssuedAtMaxPast {
		return nil, errors.New("inbox: terminal retention must exceed the issued_at window (7d)")
	}
	if cfg.SweepInterval <= 0 || cfg.NotifyInterval <= 0 || cfg.GaugeInterval <= 0 || cfg.ReconcileInterval < 0 {
		return nil, errors.New("inbox: loop intervals must be positive (reconcile may be 0 = disabled)")
	}
	if cfg.ReconcileInterval > 0 && (ledgers == nil || cfg.ReconcileWindow <= 0) {
		return nil, errors.New("inbox: reconcile enabled but ledger reader or window missing")
	}
	if cfg.ReconcileInterval > 0 && cfg.ReconcileWindow < 2*cfg.ReconcileInterval {
		return nil, errors.New("inbox: reconcile window must cover at least two intervals (see config.Validate)")
	}
	jitter := cfg.Jitter
	if jitter == nil {
		jitter = randomJitter
	}
	return &Loops{store: store, ledgers: ledgers, cfg: cfg, campaign: campaign, now: now, jitter: jitter}, nil
}

// randomJitter 返回 [0, n) 内均匀分布的随机时长;n ≤ 0 时为 0。
func randomJitter(n time.Duration) time.Duration {
	if n <= 0 {
		return 0
	}
	return mathrand.N(n)
}

// Run 阻塞运行候选者循环直到 ctx 结束;当选后起各循环,丢失领导权或 ctx 结束即等全部轮次退出。
func (l *Loops) Run(ctx context.Context) {
	l.campaign(ctx, l.whileLeader)
}

// whileLeader 在任期内跑各循环;lctx 取消(丢锁 / 关停)后等它们全部退出再返回,
// 这样选主器释放锁、装配层关库时不会有在途轮次。
func (l *Loops) whileLeader(lctx context.Context) {
	logx.Info("[player_inbox] Maintenance leader acquired: sweep / notify resend / gauges / reconcile run on this replica")
	var wg sync.WaitGroup
	every(lctx, &wg, "player_inbox.sweep", l.cfg.SweepInterval, l.sweepRound)
	every(lctx, &wg, "player_inbox.notify_resend", l.cfg.NotifyInterval, l.notifyRound)
	every(lctx, &wg, "player_inbox.gauges", l.cfg.GaugeInterval, l.gaugeRound)
	if l.cfg.ReconcileInterval > 0 {
		first := reconcileFirstDelay(l.cfg.ReconcileInterval, l.jitter)
		logx.Infof("[player_inbox] First reconcile round in %v, then every %v", first, l.cfg.ReconcileInterval)
		tally := &reconcileTally{} // 本任期的分片读数,只由对账这一个 goroutine 读写
		everyAfter(lctx, &wg, "player_inbox.reconcile", first, l.cfg.ReconcileInterval,
			func(ctx context.Context) { l.reconcileRound(ctx, tally) })
	}
	<-lctx.Done()
	wg.Wait()
	metrics.ResetScanGauges()
	metrics.ResetReconcileInconsistent()
	logx.Info("[player_inbox] Maintenance leader released; loops stopped")
}

// every 起一个可等待的定时循环:先等一个间隔再跑第一轮(启动风暴期不抢库)。见 everyAfter。
func every(ctx context.Context, wg *sync.WaitGroup, point string, interval time.Duration, round func(context.Context)) {
	everyAfter(ctx, wg, point, interval, interval, round)
}

// everyAfter 起一个可等待的定时循环:等 first 跑第一轮,之后按 first + k·interval 的固定节拍一轮
// (一轮拖长了就跳过错过的节拍;first = interval 时节拍与原先的单个 ticker 相同)。
// 每一轮各自 recover(safego.Run),预算 min(interval, roundBudgetCap);ctx 取消即退出;wg 在 goroutine 退出时 Done。
func everyAfter(ctx context.Context, wg *sync.WaitGroup, point string, first, interval time.Duration, round func(context.Context)) {
	wg.Add(1)
	safego.Go(point, func() {
		defer wg.Done()
		runRound := func() {
			safego.Run(point, func() {
				roundCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), min(interval, roundBudgetCap))
				defer cancel()
				round(roundCtx)
			})
		}
		timer := time.NewTimer(first)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		ticker := time.NewTicker(interval) // 先起节拍再跑第一轮:第一轮的耗时不推迟之后的节拍
		defer ticker.Stop()
		runRound()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runRound()
			}
		}
	})
}

// reconcileFirstDelay 是当选后第一轮对账前的等待:reconcileFirstDelayMin + jitter([0, spread)),不超过一个间隔。
// jitter 的返回值钳到 [0, spread],注入的随机源越界也不会让首轮提前或拖过 5 分钟。
func reconcileFirstDelay(interval time.Duration, jitter func(time.Duration) time.Duration) time.Duration {
	j := min(max(jitter(reconcileFirstDelaySpread), 0), reconcileFirstDelaySpread)
	return min(reconcileFirstDelayMin+j, interval)
}

// reconcileStartShard 是一轮对账的起始分片:墙钟落在第 k = nowMs / intervalMs 个间隔,起点 = k × stride % 64
// (stride = reconcileStartStride,评审 M5 / R1)。不依赖内存状态:易主后新领导者从同一个位置接着轮转。
//
// 为什么步长是 stride 而不是 1:每轮至少读完从起点起的 stride 片(collectFinalized 的覆盖下界),起点每轮前移
// stride 片,相邻轮次的必读段首尾相接,连续 reconcileFullCycleRounds 轮覆盖全部 64 片 —— 不论截断多严重。
// 步长 1 时截断持续的那段时间里,一轮只读到 [start, start+c),窗口内 W 轮总共只碰得到 c+W−1 片。
func reconcileStartShard(nowMs uint64, interval time.Duration) uint32 {
	intervalMs := interval.Milliseconds()
	if intervalMs <= 0 {
		return 0
	}
	k := (nowMs / uint64(intervalMs)) % ShardCount // 先取模:k × stride 不会溢出
	return uint32((k * reconcileStartStride) % ShardCount)
}

// reconcileTally 是一个任期内各分片最近一次对账读数(评审 R1),player_inbox_reconcile_inconsistent_rows 取它们的和。
//
// 截断的轮次只覆盖从起点起的一部分分片。只拿「本轮」读数写 gauge 的话,不一致的行一旦落在本轮没读完的分片里,
// 读数就掉回 0、critical 告警跟着消失,而那一行还在窗口里、问题并没有处理。所以按分片记:
//   - 本轮读完的分片(ShardTally.Complete)用本轮读数覆盖 —— 处置之后能降下来;
//   - 只核对了一部分的分片取 max(旧读数, 本轮读数):发现的不一致是确凿的下界,一处没发现不能证明已经一致;
//   - 本轮没读到的分片保留旧读数。
//
// 截断持续时每片至多隔 reconcileFullCycleRounds 轮读完一次,所以各片读数至多滞后这么多轮。
// 任期开始时全为 0(新领导者在头几轮读到之前是偏低的下界;卸任时 whileLeader 把 gauge 归零)。
// 只由本任期的对账 goroutine 读写(everyAfter 串行跑各轮),不加锁。
type reconcileTally struct {
	byShard [ShardCount]int
}

// apply 记入一轮的分片读数,返回全部分片读数之和。
func (t *reconcileTally) apply(shards []ShardTally) int {
	for _, s := range shards {
		shard := s.Shard % ShardCount
		if s.Complete {
			t.byShard[shard] = s.Inconsistent
		} else {
			t.byShard[shard] = max(t.byShard[shard], s.Inconsistent)
		}
	}
	total := 0
	for _, n := range t.byShard {
		total += n
	}
	return total
}

// reconcileRoundResult 把一轮对账的错误折成 player_inbox_reconcile_rounds_total 的 result:
// 成功 ok;本轮预算用完 timeout;领导权丢失 / 关停造成的取消返回 ""(不计数:那一轮不是失败,是不该再跑);其余 error。
// 驱动层不一定把 ctx 错误包进返回值,所以同时看 ctx.Err()。
func reconcileRoundResult(ctx context.Context, err error) string {
	switch {
	case err == nil:
		return reconcileRoundOK
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return ""
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return reconcileRoundTimeout
	default:
		return reconcileRoundError
	}
}

func (l *Loops) nowMs() uint64 {
	ms := l.now().UnixMilli()
	if ms < 0 {
		return 0
	}
	return uint64(ms)
}

// sweepRound 清理保留期外的终态行(设计 §9.1)。
func (l *Loops) sweepRound(ctx context.Context) {
	if l.RootAdmission != nil {
		finish, err := l.RootAdmission.Enter()
		if err != nil {
			return
		}
		defer finish()
		defer func() {
			if recovered := recover(); recovered != nil {
				l.RootAdmission.Hold()
				panic(recovered)
			}
		}()
	}
	nowMs := l.nowMs()
	retentionMs := uint64(l.cfg.TerminalRetention.Milliseconds())
	if nowMs <= retentionMs {
		return
	}
	deleted, err := l.store.SweepTerminal(ctx, nowMs-retentionMs, sweepBudgetPerRound)
	if err != nil && l.RootAdmission != nil {
		l.RootAdmission.Hold()
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		logx.Errorf("[player_inbox] Sweep round failed after deleting %d rows: %v", deleted, err)
		return
	}
	if deleted > 0 {
		logx.Infof("[player_inbox] Sweep deleted %d terminal rows older than %v", deleted, l.cfg.TerminalRetention)
	}
}

// notifyRound 按分片轮转一轮补发到期提醒(设计 §6.3)。
func (l *Loops) notifyRound(ctx context.Context) {
	if l.RelayAdmission != nil {
		finish, err := l.RelayAdmission.Enter()
		if err != nil {
			return
		}
		defer finish()
		defer func() {
			if recovered := recover(); recovered != nil {
				l.RelayAdmission.Hold()
				panic(recovered)
			}
		}()
	}
	nowMs := l.nowMs()
	grace := uint64(notifyGrace.Milliseconds())
	if nowMs <= grace {
		return
	}
	for shard := uint32(0); shard < ShardCount; shard++ {
		if ctx.Err() != nil {
			return
		}
		due, err := l.store.DueNotifications(ctx, shard, nowMs-grace, notifyPerShardLimit)
		if err != nil {
			logx.Errorf("[player_inbox] Notify resend scan failed shard=%d: %v", shard, err)
			continue
		}
		for _, d := range due {
			if ctx.Err() != nil {
				return
			}
			// 单次提醒的预算在 ResendNotify 里加;没落定的行被推到 now + 退避,不会下一轮又排在队头(评审 L6)。
			if result := l.store.ResendNotify(ctx, d); result == NotifyError && l.RelayAdmission != nil {
				l.RelayAdmission.Hold()
			}
		}
	}
}

// gaugeCutoffs 按 now 算出一次扫描型 gauge 的截止:提醒积压 = 悬了 notifyOverdue 以上;
// 已投递未确认的陈旧截止 = now − deliveredStaleAfter(评审 M4)。now 早于对应时长时截止取 0。
func gaugeCutoffs(nowMs uint64) GaugeCutoffs {
	before := func(d time.Duration) uint64 {
		if ms := uint64(d.Milliseconds()); nowMs > ms {
			return nowMs - ms
		}
		return 0
	}
	return GaugeCutoffs{NotifyOverdueBeforeMs: before(notifyOverdue), DeliveredStaleBeforeMs: before(deliveredStaleAfter)}
}

// gaugeRound 刷新扫描型 gauge(设计 §9.4)。
func (l *Loops) gaugeRound(ctx context.Context) {
	nowMs := l.nowMs()
	overdue := uint64(notifyOverdue.Milliseconds())
	if nowMs <= overdue {
		return
	}
	g, err := l.store.ScanGauges(ctx, nowMs, gaugeCutoffs(nowMs))
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			logx.Errorf("[player_inbox] Gauge scan failed: %v", err)
		}
		return
	}
	metrics.SetScanGauges(g)
}

// reconcileRound 跑一轮定时对账(设计 §9.3):窗口 [now − ReconcileWindow, now],起始分片按墙钟轮转;
// 记 reconcile_rounds_total{result},把本轮的分片读数记进 tally,再以各片读数之和刷新不一致行数 gauge。
// 收集阶段就失败的轮次没有分片读数,gauge 保持原值。tally 属于当前任期(whileLeader 建)。
func (l *Loops) reconcileRound(ctx context.Context, tally *reconcileTally) {
	if l.RelayAdmission != nil {
		finish, err := l.RelayAdmission.Enter()
		if err != nil {
			return
		}
		defer finish()
		defer func() {
			if recovered := recover(); recovered != nil {
				l.RelayAdmission.Hold()
				panic(recovered)
			}
		}()
	}
	nowMs := l.nowMs()
	window := uint64(l.cfg.ReconcileWindow.Milliseconds())
	since := uint64(0)
	if nowMs > window {
		since = nowMs - window
	}
	start := reconcileStartShard(nowMs, l.cfg.ReconcileInterval)
	res, err := l.store.ReconcileRecent(ctx, since, start, l.ledgers)
	result := reconcileRoundResult(ctx, err)
	if result == "" {
		return // 领导权丢失 / 关停:不计数、不刷新 gauge(卸任时 whileLeader 会把它归零)
	}
	metrics.ObserveReconcileRound(result)
	metrics.SetReconcileInconsistent(tally.apply(res.Shards))
	if res.Truncated && l.cfg.ReconcileWindow < reconcileFullCycleRounds*l.cfg.ReconcileInterval {
		logx.Errorf("[player_inbox] Reconcile window %v is shorter than %d intervals of %v: while rounds stay truncated, "+
			"some shards may not be fully read even once before their rows leave the window",
			l.cfg.ReconcileWindow, reconcileFullCycleRounds, l.cfg.ReconcileInterval)
	}
	switch result {
	case reconcileRoundTimeout:
		logx.Errorf("[player_inbox] Reconcile round ran out of its %v budget after checking %d of %d players (start_shard=%d); "+
			"unchecked rows are left to later rounds and their shards keep the previous gauge readings",
			min(l.cfg.ReconcileInterval, roundBudgetCap), res.PlayersChecked, res.PlayersPlanned, start)
	case reconcileRoundError:
		logx.Errorf("[player_inbox] Reconcile round failed after checking %d of %d players (start_shard=%d): %v",
			res.PlayersChecked, res.PlayersPlanned, start, err)
	}
	if res.Inconsistent > 0 {
		logx.Errorf("[player_inbox] Reconcile round found %d inconsistent rows (see preceding ERROR lines)", res.Inconsistent)
	}
}
