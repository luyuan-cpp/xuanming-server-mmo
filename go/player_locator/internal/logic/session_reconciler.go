package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/proc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/config"
	"player_locator/internal/svc"
	pb "proto/player_locator"
	"shared/leader"
	"shared/playerredis"
	"shared/redisslot"
	"shared/safego"
)

// 会话对账:兜住「gate 整机崩溃 → TCP 断开回调不执行 → SetDisconnecting
// 永远不来 → 会话永久 ONLINE」的结构性缺口。
//
// 会话进入清理状态机的唯一入口是 SetDisconnecting(它的 Lua 才会 ZADD 租约
// ZSET,LeaseMonitor 只消费该 ZSET),而会话键是无 TTL 写入的。断线通知链
// 三处都可能整段蒸发:gate 崩溃(回调不执行)、断线瞬间无 login 节点
// (gate 静默跳过)、login→locator RPC 穷尽重试仍失败。落到这里的会话没有
// 别的自愈路径;gate 活着而通知丢失的那一类对账同样看不到,只能按
// release-checklist F.B-1 的定向补救人工处理。
//
// 对账原则(fail-closed,宁可漏扫不可误杀):
//  1. 只动 State==ONLINE 且 GateInstanceId 指向死 gate 的会话;
//  2. gate 存活性以 etcd 注册表为准(C++ gate 以 lease 注册 NodeInfo,
//     node_uuid 即会话里的 GateInstanceId);etcd 列举失败 → 本拍不推进去抖、不转换;
//  3. 同一 gate 必须**连续两拍**都不在存活集内才动手 —— 单拍缺席可能只是 etcd 视图
//     抖动或 gate 正在重启重注册;
//  4. 动手 = 走与 SetDisconnecting 完全相同的提交点 transitionToDisconnecting(整字节 CAS +
//     版本号递进 + ZADD 租约),期间玩家经新 gate 重连/重登会改写会话字节,
//     CAS 自动失败成 no-op —— 绝不会踢掉活人。
//
// 怎么做(docs/design/player-locator-gate-session-index.md,§11 的按 slot 分桶已落地):
//   - 会话写入在同一段 Lua 里维护本 slot 的「gate 实例 → 会话」索引(session_cas.go)。对账只读死 gate 的索引键,
//     不扫全键空间。
//   - 死 gate 清扫按「成员登记 + 段租约」分给各副本(gate_sweep.go、slot_shard.go),不选主:16384 个 slot 的登记集
//     一个 leader 读不动,而且一个 leader 看不清 etcd 就是全服停摆。
//   - 写者检查、就绪标记、补建 / 审计是全服一份的控制面,由一个 leader 跑(本文件,shared/leader,控制域 Redis 锁)。
//     选主只为削减负载,不是正确性 fence:两个 leader 短暂重叠时,所有动作都走整字节 CAS 或字节守卫 Lua,重叠无害。
//     leader 看不清 etcd / 控制域 Redis 时主动让位(连续 3 拍失败,或 3 拍内没有一次成功),本进程多等一个 TTL 再竞选。
//   - 漏进索引的会话(旧版写者、外部篡改)靠补建 / 审计扫描补:就绪标记缺失时连续跑限速的全键扫描,
//     就绪后按 AuditIntervalSeconds 跑。扫描只补索引、清脏成员,不转换会话 —— 转换只有清扫一条路径。
//   - 就绪标记是租约(只由「本拍 etcd 读成功且零旧写者」的 leader 续期)+ 控制域代次(看到旧写者就 INCR,
//     补建结束时「代次未变才设标记」)。一次性回滚、换主、两个 leader 重叠时标记都不会错误地留着。

const (
	defaultReconcileInterval = 60 * time.Second
	// reconcilerLeaderTTL 是选主锁的 TTL,也是让位后本进程再竞选前多等的时长。
	reconcilerLeaderTTL = 30 * time.Second
	// maxConsecutiveRoundFailures / staleSuccessIntervals 是 leader 让位、副本退出清扫分片的两个门槛:
	// 连续这么多拍失败,或距上次成功超过这么多拍。
	maxConsecutiveRoundFailures = 3
	staleSuccessIntervals       = 3

	// maxKeyTypeErrorsPerRound:一拍里最多吸收这么多单键错误(WRONGTYPE,逐个跳过),再多就按实例级故障整拍失败、
	// 计入让位门槛。几个被篡改的键不该让全服对账停摆;成百上千个说明不是「个别键坏了」,要喊出来而不是一拍拍报 ok。
	maxKeyTypeErrorsPerRound = 100
	// keyErrorLogLimit:一拍 / 一遍里逐条打 ERROR 的单键错误数,其余只计数,在拍 / 遍的汇总日志里出现。
	keyErrorLogLimit = 10

	// 补建 / 审计一遍扫描内,单页处理失败时重做这一页:最多 passPageMaxAttempts 次,退避从 passPageRetryBase
	// 翻倍到 passPageRetryMax。一遍可能跑几个小时,一次抖动不该让整遍作废。只有一个 leader 在扫,不存在多个客户端
	// 同时重试的惊群,所以不加抖动。SCAN 本身的失败不在这里重试:ScanEachMaster 按 fail-closed 让整遍失败。
	passPageMaxAttempts = 5
	passPageRetryBase   = time.Second
	passPageRetryMax    = 30 * time.Second
	// auditFailureCooldown:审计失败后至少隔这么久再试(Redis 侧 EX,换主不重置),不让持续的故障每拍重开一遍全键扫描。
	// 审计周期更短时取审计周期。
	auditFailureCooldown = time.Hour
)

// ReconcilerParams 是 StartSessionReconciler 的装配参数。
type ReconcilerParams struct {
	// EtcdClient 复用 Node 注册用的客户端(生命周期归 Node);nil 时对账不运行。
	EtcdClient *clientv3.Client
	// IntervalSeconds 即 Lease.ReconcileIntervalSeconds:0 = 60s,-1 = 关闭。
	IntervalSeconds int64
	// Reconcile 即 Lease.Reconcile(这里再 Normalized 一次,幂等)。
	Reconcile config.ReconcileConf
	// LeaderID 进选主锁值前缀与日志,方便 redis-cli GET 一眼看出 leader 是哪个 Pod。
	LeaderID string
	// InstanceID 是本副本在清扫分片里的身份(成员登记与段租约的值)。
	InstanceID string
}

// reconcilerDeps 是对账循环的外部依赖:生产由 productionReconcilerDeps 装配,单测注入假实现(固定 now、
// 不限速的 pacer、受控的拍),不依赖墙钟。
type reconcilerDeps struct {
	view     clusterView
	now      func() time.Time
	newPacer func(perSecond int) pacer
	// newTicker 返回「等到下一拍」与它的停止函数。
	newTicker func(interval time.Duration) (wait func(ctx context.Context) error, stop func())
	// sleep 等一段时间或 ctx 结束:补建循环的间隔、让位后再竞选前的等待、扫描单页重试的退避。
	sleep func(ctx context.Context, d time.Duration) error
	// scanSessions 逐主节点扫玩家域的键,生产为 playerredis.ScanEachMaster:它核对扫描前后每个主节点的 run_id 与
	// slot 分布、有无进行中的迁移,变了就返回错误(SCAN 游标只在同一个 Redis 进程、同一份拓扑上有覆盖保证)。
	// 单测注入,miniredis 不支持 INFO server。
	scanSessions func(ctx context.Context, c redis.UniversalClient, match string, count int64, fn func(keys []string) error) error
}

func productionReconcilerDeps(cli *clientv3.Client) reconcilerDeps {
	return reconcilerDeps{
		view:         newEtcdClusterView(cli),
		now:          time.Now,
		newPacer:     newIntervalPacer,
		newTicker:    newRealTicker,
		sleep:        sleepCtx,
		scanSessions: playerredis.ScanEachMaster,
	}
}

func newRealTicker(interval time.Duration) (func(ctx context.Context) error, func()) {
	t := time.NewTicker(interval)
	return func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}, t.Stop
}

func sleepCtx(ctx context.Context, d time.Duration) error {
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

// StartSessionReconciler 起本副本的死 gate 清扫(及其分片),并竞选 leader、任期内跑写者检查与补建 / 审计。
// 阻塞到 ctx 结束,调用方 go 出去。
//
// 必须在 go-zero 的 Prometheus agent 启动之后(zrpc.MustNewServer 之后)调用:agent 启动前的指标写入会被丢弃,
// 预置 0 与 is_leader 就落空了(reconcile_metrics.go 文件头)。
func StartSessionReconciler(ctx context.Context, svcCtx *svc.ServiceContext, p ReconcilerParams) {
	if p.IntervalSeconds < 0 {
		logx.Info("SessionReconciler disabled by config")
		return
	}
	if p.EtcdClient == nil {
		// 看不到 etcd 只能放弃对账(fail-closed:不清理),但要喊出来 ——
		// 这是 gate 崩溃兜底的唯一自动化路径。
		logx.Error("SessionReconciler: etcd client unavailable, reconciler NOT running")
		return
	}
	interval := defaultReconcileInterval
	if p.IntervalSeconds > 0 {
		interval = time.Duration(p.IntervalSeconds) * time.Second
	}
	cfg := p.Reconcile.Normalized()
	primeReconcileMetrics()
	deps := productionReconcilerDeps(p.EtcdClient)

	sweeper := newGateSweeper(svcCtx, nil, cfg, interval, deps)
	shard := newSlotShard(svcCtx.PlayerRedis, shardRoleGateSweep, p.InstanceID, sweeper.eligible.Load)
	sweeper.owner = shard
	proc.AddShutdownListener(shard.Stop)
	safego.Go("player_locator.gate_sweep_shard", func() { shard.Run(ctx) })
	safego.Go("player_locator.gate_sweep", func() { sweeper.run(ctx) })

	elector := leader.New(leader.NewGoRedisStore(svcCtx.RedisClient), reconcilerLeaderKey, leader.Options{
		TTL:           reconcilerLeaderTTL,
		ID:            p.LeaderID,
		OnStateChange: onReconcilerLeaderChange,
	})
	// 优雅退出时主动让位,不把锁留到 TTL 自然过期(scene_manager 同法):go-zero 的 SIGTERM 走 os.Exit,
	// main 的 defer 不执行。
	proc.AddShutdownListener(elector.Stop)

	logx.Infof("SessionReconciler started: interval=%v leader_key=%s id=%s audit_interval_s=%d scan_count=%d "+
		"scan_pages_per_s=%d transitions_per_s=%d dead_gate_members_per_round=%d",
		interval, reconcilerLeaderKey, p.LeaderID, cfg.AuditIntervalSeconds, cfg.ScanCount,
		cfg.ScanPagesPerSecond, cfg.MaxTransitionsPerSecond, cfg.DeadGateMembersPerRound)
	campaignSessionReconciler(ctx, elector.Run, reconcilerLeaderTTL, deps.sleep, func(lctx context.Context) bool {
		return newSessionReconciler(svcCtx, cfg, interval, deps).run(lctx)
	})
	logx.Info("SessionReconciler stopped")
}

// reconcilerCampaign 是「竞选并在任期内运行 whileLeader」的函数形状,生产为 (*leader.Elector).Run。
type reconcilerCampaign func(ctx context.Context, whileLeader func(ctx context.Context))

// campaignSessionReconciler 反复竞选,任期内跑 term。term 返回 true 表示主动让位:结束这次 campaign
// (选主器带属主校验释放锁),本进程等 yieldBackoff 再竞选,让健康副本先抢到锁。
// term 返回 false(丢锁 / 关停)时由选主器自己的循环继续竞选;campaign 返回而没有让位 = ctx 结束或选主器已 Stop。
func campaignSessionReconciler(
	ctx context.Context,
	campaign reconcilerCampaign,
	yieldBackoff time.Duration,
	sleep func(ctx context.Context, d time.Duration) error,
	term func(ctx context.Context) bool,
) {
	for ctx.Err() == nil {
		runCtx, cancel := context.WithCancel(ctx)
		yielded := false // 只在 campaign 同步调用的 whileLeader 里写,与这里同一个 goroutine
		campaign(runCtx, func(lctx context.Context) {
			if term(lctx) {
				yielded = true
				cancel()
			}
		})
		cancel()
		if !yielded {
			return
		}
		logx.Errorf("SessionReconciler: leadership yielded, waiting %v before campaigning again", yieldBackoff)
		if err := sleep(ctx, yieldBackoff); err != nil {
			return
		}
	}
}

// sessionReconciler 是一个 leader 任期内的对账状态:写者检查、就绪标记、补建 / 审计。换主即新建。
type sessionReconciler struct {
	ctrl     *redis.Client         // 控制域:就绪、代次、审计时间
	player   redis.UniversalClient // 玩家域:会话、索引、登记集
	cfg      config.ReconcileConf
	interval time.Duration
	deps     reconcilerDeps
	// scanPacer 只给补建 / 审计循环。
	scanPacer pacer
}

func newSessionReconciler(svcCtx *svc.ServiceContext, cfg config.ReconcileConf, interval time.Duration, deps reconcilerDeps) *sessionReconciler {
	return &sessionReconciler{
		ctrl:      svcCtx.RedisClient,
		player:    svcCtx.PlayerRedis,
		cfg:       cfg,
		interval:  interval,
		deps:      deps,
		scanPacer: deps.newPacer(cfg.ScanPagesPerSecond),
	}
}

// readyLease 是就绪标记的租约:3 拍 + 半拍余量。连续两拍失败后第三拍按时续上,不与过期赛跑;
// leader 连续三拍失败会让位,接任者通常在过期前跑完第一拍并续上;没有任何新版 leader 在跑
// (一次性回滚到旧版、对账被关掉)时,标记最迟 3.5 拍后自然过期,下一任新版 leader 从补建开始。
func (r *sessionReconciler) readyLease() time.Duration {
	return staleSuccessIntervals*r.interval + r.interval/2
}

// run 是一个任期:起补建 / 审计循环,本 goroutine 按拍做写者检查。返回 true = 主动让位;false = 任期结束(lctx 取消)。
// 返回前取消并等补建循环退出:同一进程再次当选时不会有两个补建循环并发,前任在途的写也不会落在新任之后。
func (r *sessionReconciler) run(lctx context.Context) bool {
	// 每个任期从 0 开始:leader 专属的 gauge 非零只可能来自现任。启动预置与上一任期收尾时已清过一次,
	// 这里再清一次,不依赖那两处一定生效过。
	resetReconcileLeaderGauges()
	rctx, cancel := context.WithCancel(lctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
		resetReconcileLeaderGauges()
	}()
	wg.Add(1)
	safego.Go("player_locator.session_reconciler.repair", func() {
		defer wg.Done()
		r.repairLoop(rctx)
	})

	wait, stop := r.deps.newTicker(r.interval)
	defer stop()
	lastSuccess := r.deps.now() // 任期开始作为「上次成功」的基线
	failures := 0
	for {
		reconcileIsLeader.Set(1) // 每拍重申:当选时的那次写可能早于 Prometheus agent 启动而被丢弃
		outcome := r.runWriterRound(rctx)
		if rctx.Err() != nil {
			return false // 丢锁 / 关停造成的取消不计数
		}
		now := r.deps.now()
		reconcileRoundTotal.Inc(reconcileLoopWriters, outcome.result())
		if outcome.err == nil {
			failures = 0
			lastSuccess = now
			reconcileLastSuccessUnix.Set(float64(now.Unix()), reconcileLoopWriters)
		} else {
			failures++
			logx.Errorf("SessionReconciler: writer round failed stage=%s consecutive_failures=%d: %v",
				outcome.stage, failures, outcome.err)
		}
		if failures >= maxConsecutiveRoundFailures || now.Sub(lastSuccess) > staleSuccessIntervals*r.interval {
			logx.Errorf("SessionReconciler: yielding leadership: consecutive_failures=%d since_last_success=%v last_stage=%s",
				failures, now.Sub(lastSuccess), outcome.stage)
			return true
		}
		if err := wait(rctx); err != nil {
			return false
		}
	}
}

// ---------------------------------------------------------------------------
// 写者检查与一拍的公共类型
// ---------------------------------------------------------------------------

// roundStage 是一拍失败所在的阶段(只进日志与 result label)。
type roundStage string

const (
	stageEtcd  roundStage = "etcd"
	stageRedis roundStage = "redis"
	stagePanic roundStage = "panic"
)

type roundOutcome struct {
	stage  roundStage
	err    error
	report indexRoundReport
}

func (o roundOutcome) result() string {
	switch {
	case o.err == nil:
		return reconcileResultOK
	case o.stage == stageEtcd:
		return reconcileResultEtcdError
	case o.stage == stagePanic:
		return reconcileResultPanic
	default:
		return reconcileResultRedisError
	}
}

// indexRoundReport 是一拍的统计(日志与单测都看它;指标后端在单测里不生效)。
// 写者检查填前两项,死 gate 清扫填其余。
type indexRoundReport struct {
	legacyWriters int
	readyRenewed  bool
	deadGates     []string   // 本拍确认死亡的 gate(含本拍已释放的)
	released      []gateSlot // 本拍已空、已移出登记集的死 gate 索引键
	deadIndexKeys int        // 本拍预算用完后,死 gate 还剩的索引键数
	liveIndexKeys int        // 存活 gate 的索引键数(约等于这些 slot 上的在线数)
	actions       map[string]int
	keyErrors     keyErrorTally // 本拍吸收的单键错误(上限 maxKeyTypeErrorsPerRound)
}

// isKeyTypeError 判断错误能否归因于某一个键本身(类型不对),而不是 Redis 实例或连接的故障。
//
// 只认 WRONGTYPE:Redis 7 的脚本错误保留错误码前缀("WRONGTYPE ... script: ..."),Redis 6 与 miniredis 包成
// "ERR Error ... script ...: WRONGTYPE ...",两者都含这个词。READONLY / LOADING / MASTERDOWN / CLUSTERDOWN / TRYAGAIN
// (go-redis 重试用尽后同样以 redis.Error 抛出)、OOM、BUSY 也是服务端回复,但它们是实例级故障:当成单键问题跳过,
// 主从切换或 OOM 期间每个转换都会被跳过而本拍报 ok,last_success 照常推进、不让位,真故障就被掩盖了。
func isKeyTypeError(err error) bool {
	var rerr redis.Error
	return errors.As(err, &rerr) && strings.Contains(rerr.Error(), "WRONGTYPE")
}

// keyErrorTally 统计一拍 / 一遍里被吸收的单键错误。limit > 0 时超过即按实例级故障返回错误(清扫用);
// 完整扫描传 0 不设上限:一个坏掉的索引键会让那个 gate 上的每个候选都报错,设上限会让整遍提前中止、
// 其余键永远扫不到;那一遍改由「有候选被跳过就不设就绪标记」守住(passReport.skippedCandidates)。
type keyErrorTally struct {
	limit  int
	count  int
	logged int
}

// absorb 判断 err 能否按「这一个键的问题」吸收:能则计数、记 action、按上限返回 nil 或错误;不能原样返回 err。
// what 是日志里的定位信息(gate_instance / player),不进指标。
func (t *keyErrorTally) absorb(err error, actions map[string]int, action string, what string) error {
	if !isKeyTypeError(err) {
		return err
	}
	t.count++
	countReconcileAction(actions, action)
	if t.limit > 0 && t.count > t.limit {
		return fmt.Errorf("%d key type errors exceed the per-round limit %d, treating as an instance-level failure: %w",
			t.count, t.limit, err)
	}
	if t.logged < keyErrorLogLimit {
		t.logged++
		logx.Errorf("SessionReconciler: skipped %s: key has the wrong type, left in place for investigation: %v", what, err)
	}
	return nil
}

// countReconcileAction 同时记本拍 / 本遍统计与计数器。
func countReconcileAction(actions map[string]int, action string) {
	actions[action]++
	reconcileSessionsTotal.Inc(action)
}

// runWriterRound 跑一拍写者检查,recover 收在这一拍。
func (r *sessionReconciler) runWriterRound(ctx context.Context) roundOutcome {
	out := roundOutcome{stage: stagePanic, err: errors.New("writer round panicked")}
	safego.Run("player_locator.session_reconciler.writers", func() {
		roundCtx, cancel := context.WithTimeout(ctx, r.interval)
		defer cancel()
		out = r.writerRound(roundCtx)
	})
	return out
}

// writerRound 是 leader 的一拍。etcd 出错只跳过检查本身:不删就绪标记、不动代次 —— fail-closed 针对的是「转不转换会话」,
// 不是「出错就认定有旧写者」;后者会让每次 etcd 抖动都作废补建,稳态退化成永久的全键扫描。这一拍记为失败,计入让位门槛。
func (r *sessionReconciler) writerRound(ctx context.Context) roundOutcome {
	var rep indexRoundReport
	stage, err := r.checkSessionWriters(ctx, &rep)
	if err != nil {
		return roundOutcome{stage: stage, err: err, report: rep}
	}
	return roundOutcome{report: rep}
}

// checkSessionWriters 比对已注册的 locator 与有能力键的 locator:有旧写者 → 递增代次、删就绪标记;
// 零旧写者 → 给已存在的就绪标记续租。只有一次成功的 etcd 读才会改动标记与代次。
func (r *sessionReconciler) checkSessionWriters(ctx context.Context, rep *indexRoundReport) (roundStage, error) {
	all, capable, err := r.deps.view.SessionWriters(ctx)
	if err != nil {
		return stageEtcd, fmt.Errorf("list session writers: %w", err)
	}
	legacy := legacyWriters(all, capable)
	rep.legacyWriters = len(legacy)
	reconcileLegacyWriters.Set(float64(len(legacy)))
	if len(legacy) > 0 {
		if err := markLegacyWritersScript.Run(ctx, r.ctrl, []string{gateIndexGenKey, gateIndexReadyKey}).Err(); err != nil {
			return stageRedis, fmt.Errorf("mark legacy writers: %w", err)
		}
		gateIndexReady.Set(0)
		// 滚动升级期间每拍都会出现,是预期的;长期不消失才告警(PlayerLocatorLegacySessionWriters)。
		logx.Infof("SessionReconciler: %d legacy session writer(s) without gate index capability (first: %s); "+
			"ready marker cleared, gen bumped, backfill keeps running", len(legacy), strings.Join(legacy[:min(3, len(legacy))], ","))
		return "", nil
	}
	renewed, err := renewGateIndexReadyScript.Run(ctx, r.ctrl, []string{gateIndexReadyKey},
		r.readyLease().Milliseconds(), gateIndexReadyValuePrefix).Int()
	if err != nil {
		return stageRedis, fmt.Errorf("renew gate index ready marker: %w", err)
	}
	rep.readyRenewed = renewed == 1
	if rep.readyRenewed {
		gateIndexReady.Set(1)
	} else {
		gateIndexReady.Set(0)
	}
	return "", nil
}

// ---------------------------------------------------------------------------
// 补建 / 审计循环
// ---------------------------------------------------------------------------

// repairLoop 是任期内的补建 / 审计循环:就绪标记缺失时每隔一拍跑一遍补建;标记存在且审计到期时跑审计。
// 每一步各自 recover(safego.Run),ctx 取消即退出。
func (r *sessionReconciler) repairLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if !safego.Run("player_locator.session_reconciler.repair_step", func() { r.repairStep(ctx) }) {
			reconcileRoundTotal.Inc(reconcileLoopRepair, reconcileResultPanic)
		}
		if err := r.deps.sleep(ctx, r.interval); err != nil {
			return
		}
	}
}

// repairStepResult 是一步补建 / 审计的结果(单测看它;指标后端在单测里不生效)。
type repairStepResult struct {
	loop   string // reconcileLoopRepair / reconcileLoopAudit;"" = 本步什么都没跑
	result string // reconcileResult*;"" = 被取消,不计数
	report passReport
}

// repairStep 做一步:读就绪标记;缺失 → 补建一遍;存在且审计到期 → 审计一遍;否则什么都不做。
func (r *sessionReconciler) repairStep(ctx context.Context) repairStepResult {
	ready, err := r.ctrl.Exists(ctx, gateIndexReadyKey).Result()
	if err != nil {
		if ctx.Err() == nil {
			logx.Errorf("SessionReconciler: read gate index ready marker failed, retry next interval: %v", err)
		}
		return repairStepResult{}
	}
	if ready == 0 {
		return r.runRepairPass(ctx)
	}
	if r.cfg.AuditIntervalSeconds < 0 {
		return repairStepResult{}
	}
	due, err := r.auditDue(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logx.Errorf("SessionReconciler: read last audit time failed, retry next interval: %v", err)
		}
		return repairStepResult{}
	}
	if !due {
		return repairStepResult{}
	}
	return r.runAuditPass(ctx)
}

// runRepairPass 跑一遍补建,结束时「代次未变才设就绪标记」:补建期间任何一拍在成功的 etcd 读里看到旧写者,
// 都会递增代次,这一遍就作废(补进来的成员照样有效,只是不能据此宣布就绪),下一拍再来一遍。
func (r *sessionReconciler) runRepairPass(ctx context.Context) repairStepResult {
	out := repairStepResult{loop: reconcileLoopRepair}
	gen, err := readGateIndexGen(ctx, r.ctrl)
	if err != nil {
		return r.finishPass(ctx, out, fmt.Errorf("read gate index gen: %w", err))
	}
	// 先读代次、再查一次写者:此刻已有旧写者时照样补(补进来的成员有效),但这次检查本身就会递增代次,
	// 结束时不会设标记 —— 否则在比一拍还短的补建(小环境)里,标记会在旧写者仍在时被设上、下一拍才被删。
	// etcd 读失败就不开始这一遍,下一拍再来:不能凭一次看不清的读开始一遍可能宣布就绪的补建。
	var writers indexRoundReport
	if stage, werr := r.checkSessionWriters(ctx, &writers); werr != nil {
		if ctx.Err() == nil {
			out.result = reconcileResultRedisError
			if stage == stageEtcd {
				out.result = reconcileResultEtcdError
			}
			reconcileRoundTotal.Inc(out.loop, out.result)
			logx.Errorf("SessionReconciler: backfill pass not started, writer check failed stage=%s: %v", stage, werr)
		}
		return out
	}
	logx.Infof("SessionReconciler: gate index not ready, starting backfill pass gen=%s legacy_writers=%d",
		gen, writers.legacyWriters)
	out.report, err = r.fullPass(ctx, false)
	if err != nil {
		return r.finishPass(ctx, out, err)
	}
	// 有 ONLINE 会话因键类型错误没能补进索引:I1 的「⇐」方向此刻不成立,不能宣布就绪(fail-closed)。
	// 键一直坏着就一直不就绪,由 PlayerLocatorGateIndexNotReady 与 member_error 计数暴露,要人工处理那个键。
	if out.report.skippedCandidates > 0 {
		out.result = reconcileResultIncomplete
		reconcileRoundTotal.Inc(out.loop, out.result)
		logx.Errorf("SessionReconciler: backfill pass done but %d ONLINE session(s) could not be indexed (key type errors); "+
			"ready marker NOT set: %s", out.report.skippedCandidates, out.report)
		return out
	}
	now := r.deps.now()
	set, err := setGateIndexReadyIfGenScript.Run(ctx, r.ctrl,
		[]string{gateIndexGenKey, gateIndexReadyKey, gateIndexLastAuditKey},
		gen, gateIndexReadyValue(now), r.readyLease().Milliseconds(), now.Unix(),
	).Int()
	if err != nil {
		return r.finishPass(ctx, out, fmt.Errorf("set gate index ready marker: %w", err))
	}
	if set != 1 {
		out.result = reconcileResultGenChanged
		reconcileRoundTotal.Inc(out.loop, out.result)
		logx.Infof("SessionReconciler: backfill pass done but gen changed since %s (legacy writers seen); ready marker NOT set: %s",
			gen, out.report)
		return out
	}
	gateIndexReady.Set(1)
	logx.Infof("SessionReconciler: backfill pass done, gate index ready: %s", out.report)
	return r.finishPass(ctx, out, nil)
}

// runAuditPass 跑一遍审计(索引就绪时):补漏成员、清脏成员,完成后记下审计时刻。
// 补进成员说明不变量 I1 被破坏过(非 player_locator 的写者、外部篡改、或旧写者只活在 etcd 故障期间),打 ERROR。
// 失败(含扫描期间拓扑变化)不记审计时刻,并设冷却键(failAudit)。
func (r *sessionReconciler) runAuditPass(ctx context.Context) repairStepResult {
	out := repairStepResult{loop: reconcileLoopAudit}
	var err error
	out.report, err = r.fullPass(ctx, true)
	if err != nil {
		return r.failAudit(ctx, out, err)
	}
	if err := r.ctrl.Set(ctx, gateIndexLastAuditKey, r.deps.now().Unix(), 0).Err(); err != nil {
		return r.failAudit(ctx, out, fmt.Errorf("record last audit time: %w", err))
	}
	switch {
	case out.report.repaired > 0 || out.report.skippedCandidates > 0:
		logx.Errorf("SessionReconciler: audit found ONLINE session(s) missing from the index while it was ready "+
			"(backfilled %d, could not index %d because of key type errors): invariant I1 was broken "+
			"(non-locator writer, tampering, or a legacy writer alive only during an etcd outage), investigate: %s",
			out.report.repaired, out.report.skippedCandidates, out.report)
	default:
		logx.Infof("SessionReconciler: audit pass done: %s", out.report)
	}
	return r.finishPass(ctx, out, nil)
}

// failAudit 记一次审计失败并设冷却键:持续的故障(Redis 一直抖、扫描总被打断)下不让审计每拍重开一遍全键扫描。
// 冷却用 Redis 侧 EX,换主不重置,也不受各 Pod 时钟偏差影响。被取消(丢锁 / 关停)的不设。
func (r *sessionReconciler) failAudit(ctx context.Context, out repairStepResult, err error) repairStepResult {
	out = r.finishPass(ctx, out, err)
	if ctx.Err() != nil {
		return out
	}
	cooldown := r.auditCooldown()
	if serr := r.ctrl.Set(ctx, gateIndexAuditCooldownKey, "1", cooldown).Err(); serr != nil {
		// 设不上冷却只会让下一拍就重试,不影响正确性。
		logx.Errorf("SessionReconciler: set audit cooldown failed, next audit attempt not delayed: %v", serr)
		return out
	}
	logx.Errorf("SessionReconciler: audit pass failed, next attempt in %v", cooldown)
	return out
}

// auditCooldown 是审计失败后的冷却:auditFailureCooldown,审计周期更短时取审计周期。
func (r *sessionReconciler) auditCooldown() time.Duration {
	cooldown := auditFailureCooldown
	if interval := time.Duration(r.cfg.AuditIntervalSeconds) * time.Second; interval > 0 && interval < cooldown {
		cooldown = interval
	}
	return cooldown
}

// finishPass 记一遍补建 / 审计的结果;被取消(丢锁 / 关停)的不计数。
func (r *sessionReconciler) finishPass(ctx context.Context, out repairStepResult, err error) repairStepResult {
	if err != nil {
		if ctx.Err() != nil {
			return out
		}
		out.result = reconcileResultRedisError
		reconcileRoundTotal.Inc(out.loop, out.result)
		logx.Errorf("SessionReconciler: %s pass failed, retry next interval: %v (%s)", out.loop, err, out.report)
		return out
	}
	out.result = reconcileResultOK
	reconcileRoundTotal.Inc(out.loop, out.result)
	reconcileLastSuccessUnix.Set(float64(r.deps.now().Unix()), out.loop)
	return out
}

// auditDue 以控制域里的上次审计时刻为准判断审计是否到期:换主不重置计时。冷却键存在(上次审计失败)时不到期。
// 键不存在或值坏了按「从未审计」处理(到期):多审一次只多一轮限速扫描,不审却会让漏成员一直补不上。
//
// 时钟偏差语义:last_audit 是写入它的那个 leader 的墙钟,与本 leader 的墙钟比较。各 Pod 时钟偏差 δ 小于审计周期时,
// 审计最多提前或推迟 δ,可以接受。比本 leader 时钟超前一个审计周期以上的值不可能来自正常偏差(写入者的时钟坏了,
// 例如 NTP 故障快了几天),当坏值处理(到期并打 ERROR);否则此后所有 leader 都算出负数,审计停摆到各自时钟追上为止。
func (r *sessionReconciler) auditDue(ctx context.Context) (bool, error) {
	cooling, err := r.ctrl.Exists(ctx, gateIndexAuditCooldownKey).Result()
	if err != nil {
		return false, err
	}
	if cooling > 0 {
		return false, nil
	}
	raw, err := r.ctrl.Get(ctx, gateIndexLastAuditKey).Result()
	if err == redis.Nil {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	last, perr := strconv.ParseInt(raw, 10, 64)
	if perr != nil {
		logx.Errorf("SessionReconciler: malformed %s value %q, treating audit as due", gateIndexLastAuditKey, raw)
		return true, nil
	}
	now := r.deps.now().Unix()
	if last > now+r.cfg.AuditIntervalSeconds {
		logx.Errorf("SessionReconciler: %s=%d is more than one audit interval (%ds) ahead of this leader's clock (%d): "+
			"the writer's clock was wrong, treating audit as due", gateIndexLastAuditKey, last, r.cfg.AuditIntervalSeconds, now)
		return true, nil
	}
	return now-last >= r.cfg.AuditIntervalSeconds, nil
}

// passReport 是一遍补建 / 审计的统计。
type passReport struct {
	scannedKeys int
	repaired    int
	gates       int // 扫过的索引键数(slot × gate)
	// skippedCandidates:ONLINE 且 gate 非空、却因键类型错误(索引键或登记集不是 SET 等)没能核对或补进索引的会话数。
	// 补建遍里它非零就不设就绪标记。
	skippedCandidates int
	// pageRetries:本遍单页失败后重试的次数。
	pageRetries int
	actions     map[string]int
	keyErrors   keyErrorTally // 完整扫描不设上限(见 keyErrorTally)
}

func (p passReport) String() string {
	return fmt.Sprintf("scanned_keys=%d repaired=%d index_keys=%d skipped_candidates=%d key_type_errors=%d page_retries=%d actions=%v",
		p.scannedKeys, p.repaired, p.gates, p.skippedCandidates, p.keyErrors.count, p.pageRetries, p.actions)
}

// fullPass 是补建与审计共用的一遍扫描:先限速地逐主节点 SCAN 全部会话键补漏成员,再逐个 slot、逐个登记过的 gate
// SSCAN 索引清脏成员。只补索引、清成员,**不转换会话**:补进来的死 gate 成员由清扫在两拍内处理,转换只有一条路径。
//
// 扫描期间拓扑或节点身份变化时整遍作废(deps.scanSessions fail-closed):游标跨了进程或换了节点,不再有「开始时就在的
// 键都扫到」的保证。补进来的成员照样有效,只是不能据此宣布就绪 / 记审计完成。
func (r *sessionReconciler) fullPass(ctx context.Context, audit bool) (passReport, error) {
	rep := passReport{actions: map[string]int{}}
	if err := r.backfillKeyspace(ctx, audit, &rep); err != nil {
		return rep, err
	}
	if err := r.pruneAllGateIndexes(ctx, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// retryPassPage 对完整扫描的一页做有上限的指数退避重试。
//   - 页内的写都是幂等的字节守卫脚本 / SADD,重做一页不会重复生效;计数器可能把同一个单键错误多计一次,不影响判定。
//   - SSCAN 的游标由调用方固定,重试同一游标;一个 slot 的索引键在同一个节点上,节点换了进程时漏扫的只是脏成员,
//     下一遍审计再清(I3:索引只是提示)。
//   - ctx 结束立即返回;连续 passPageMaxAttempts 次失败才放弃这一遍。
func (r *sessionReconciler) retryPassPage(ctx context.Context, rep *passReport, what func() string, page func() error) error {
	backoff := passPageRetryBase
	for attempt := 1; ; attempt++ {
		err := page()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt >= passPageMaxAttempts {
			return fmt.Errorf("%s: giving up the pass after %d attempts: %w", what(), attempt, err)
		}
		rep.pageRetries++
		logx.Errorf("SessionReconciler: %s failed (attempt %d/%d), retrying in %v: %v",
			what(), attempt, passPageMaxAttempts, backoff, err)
		if serr := r.deps.sleep(ctx, backoff); serr != nil {
			return serr
		}
		backoff = min(backoff*2, passPageRetryMax)
	}
}

// backfillKeyspace 限速地逐主节点 SCAN 会话键(redisslot.SessionKeyPattern),把 ONLINE 且 gate 非空、却不在对应索引里的
// 会话补进去。回调由 ScanEachMaster 串行调用,所以 rep 不用加锁;它在回调里退避时,别的主节点的扫描也随之等待。
func (r *sessionReconciler) backfillKeyspace(ctx context.Context, audit bool, rep *passReport) error {
	return r.deps.scanSessions(ctx, r.player, redisslot.SessionKeyPattern, r.cfg.ScanCount, func(keys []string) error {
		if err := r.scanPacer.Wait(ctx); err != nil {
			return err
		}
		err := r.retryPassPage(ctx, rep, func() string { return fmt.Sprintf("backfill a page of %d session keys", len(keys)) }, func() error {
			return r.backfillPage(ctx, keys, audit, rep)
		})
		if err != nil {
			return err
		}
		rep.scannedKeys += len(keys) // 整页成功才计,重试不重复计
		return nil
	})
}

type backfillCandidate struct {
	pid    uint64
	slot   uint16
	member string
	gate   string
	raw    []byte
}

// backfillPage 处理一页会话键:按 slot 分组 MGET,对候选会话一次 SISMEMBER pipeline,缺失的逐个经守卫脚本补。
func (r *sessionReconciler) backfillPage(ctx context.Context, keys []string, audit bool, rep *passReport) error {
	pids := make([]uint64, 0, len(keys))
	for _, key := range keys {
		pid, ok := redisslot.ParseSessionKey(key)
		if !ok {
			continue // 不是写路径会写的键形(player:session:{<十进制 id>}),与索引无关
		}
		pids = append(pids, pid)
	}
	raws, found, err := mgetSessions(ctx, r.player, pids)
	if err != nil {
		return fmt.Errorf("mget session page: %w", err)
	}
	candidates := make([]backfillCandidate, 0, len(pids))
	for i, pid := range pids {
		if !found[i] {
			continue // 扫描与删除竞态,正常
		}
		session := &pb.PlayerSession{}
		if err := proto.Unmarshal(raws[i], session); err != nil || session.PlayerId != pid {
			// 写路径不会写出这种会话;不补索引(补了也只会被判 malformed 摘掉),留给排查。
			logx.Errorf("SessionReconciler: malformed session at %s skipped by backfill", sessionKey(pid))
			continue
		}
		if gate := indexedGate(session); gate != "" {
			candidates = append(candidates, backfillCandidate{
				pid: pid, slot: redisslot.PlayerSlot(pid), member: strconv.FormatUint(pid, 10), gate: gate, raw: raws[i],
			})
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	pipe := r.player.Pipeline()
	checks := make([]*redis.BoolCmd, len(candidates))
	for i, c := range candidates {
		checks[i] = pipe.SIsMember(ctx, gateSessionIndexKey(c.slot, c.gate), c.member)
	}
	// Exec 只返回第一条失败命令的错误;逐条看,某把索引键类型不对只跳过它上面的候选。
	if _, err := pipe.Exec(ctx); err != nil && !isKeyTypeError(err) {
		return fmt.Errorf("check index membership: %w", err)
	}
	skip := make([]bool, len(candidates))
	for i, c := range candidates {
		err := checks[i].Err()
		if err == nil {
			continue
		}
		if aerr := rep.keyErrors.absorb(err, rep.actions, reconcileActionMemberError,
			fmt.Sprintf("backfill of player=%d: index of gate_instance=%s", c.pid, c.gate)); aerr != nil {
			return fmt.Errorf("check index membership: %w", aerr)
		}
		skip[i] = true
		rep.skippedCandidates++
	}

	if err := r.ensureRegistered(ctx, candidates, checks, skip, rep); err != nil {
		return err
	}

	for i, c := range candidates {
		if skip[i] || checks[i].Val() {
			continue
		}
		added, err := repairGateIndexMemberScript.Run(ctx, r.player, repairGateIndexMemberKeys(c.pid, c.gate),
			c.raw, c.member, c.gate,
		).Int()
		if err != nil {
			if aerr := rep.keyErrors.absorb(err, rep.actions, reconcileActionMemberError,
				fmt.Sprintf("backfill of player=%d into gate_instance=%s index", c.pid, c.gate)); aerr != nil {
				return fmt.Errorf("repair index member player %d: %w", c.pid, aerr)
			}
			rep.skippedCandidates++
			continue
		}
		if added != 1 {
			continue // -1:会话已改写,新写入自己维护过索引;0:并发补上了
		}
		rep.repaired++
		if audit {
			countReconcileAction(rep.actions, reconcileActionAuditRepaired)
		} else {
			countReconcileAction(rep.actions, reconcileActionRepaired)
		}
	}
	return nil
}

// ensureRegistered 确认已是成员的候选,它的 gate 在本 slot 的登记集里(I2;登记集被局部清库删掉时,不补的话这个 gate
// 死了也没人处理)。登记集类型不对时候选算作跳过:它的 gate 死了清扫看不到它。
func (r *sessionReconciler) ensureRegistered(ctx context.Context, candidates []backfillCandidate, checks []*redis.BoolCmd, skip []bool, rep *passReport) error {
	registered := make(map[gateSlot]struct{})
	for i, c := range candidates {
		if !skip[i] && checks[i].Val() {
			registered[gateSlot{slot: c.slot, gate: c.gate}] = struct{}{}
		}
	}
	if len(registered) == 0 {
		return nil
	}
	pipe := r.player.Pipeline()
	cmds := make(map[gateSlot]*redis.IntCmd, len(registered))
	for p := range registered {
		cmds[p] = pipe.SAdd(ctx, gateRegistryKey(p.slot), p.gate)
	}
	if _, err := pipe.Exec(ctx); err != nil && !isKeyTypeError(err) {
		return fmt.Errorf("ensure gate index registry: %w", err)
	}
	for p, cmd := range cmds {
		err := cmd.Err()
		if err == nil {
			continue
		}
		if aerr := rep.keyErrors.absorb(err, rep.actions, reconcileActionGateError,
			fmt.Sprintf("gate registry of slot=%d", p.slot)); aerr != nil {
			return fmt.Errorf("ensure gate index registry: %w", aerr)
		}
		rep.skippedCandidates++
	}
	return nil
}

// pruneAllGateIndexes 逐个 slot 读登记集,逐把索引键按字节守卫摘掉不符的成员(含存活 gate)。
// 不做这一步,升级窗口里旧写者换 gate、或局部清库留下的脏成员会在长期存活的 gate 上一直堆着。
// 真在这个 gate 上 ONLINE 的成员原样保留,死 gate 的转换仍归清扫。单页失败按 retryPassPage 重试。
//
// 限速把 ScanCount 把索引键算一页:目标规模下索引键约等于在线人数,一把键平均一两个成员,逐键限速一遍要走一天以上。
func (r *sessionReconciler) pruneAllGateIndexes(ctx context.Context, rep *passReport) error {
	var visited int64
	for start := 0; start < redisslot.SlotCount; start += gateRegistryBatchSlots {
		chunk := slotRange(start, min(start+gateRegistryBatchSlots, redisslot.SlotCount))
		var pairs []gateSlot
		err := r.retryPassPage(ctx, rep, func() string { return fmt.Sprintf("read gate registries from slot %d", start) }, func() error {
			if err := r.scanPacer.Wait(ctx); err != nil {
				return err
			}
			var err error
			pairs, err = readGateRegistries(ctx, r.player, chunk, rep.actions, &rep.keyErrors)
			return err
		})
		if err != nil {
			return err
		}
		rep.gates += len(pairs)
		for _, p := range pairs {
			if visited%r.cfg.ScanCount == 0 {
				if err := r.scanPacer.Wait(ctx); err != nil {
					return err
				}
			}
			visited++
			if err := r.pruneGateIndex(ctx, p, rep); err != nil {
				return err
			}
		}
	}
	return nil
}

// pruneGateIndex 扫一把索引键,第一页的限速由调用方按键数算,续页各算一页。类型不对只跳过它(计 gate_error)。
// 第一页就扫完且没有成员时顺手把它移出登记集:摘脏成员的路径不收紧登记集,这里兜住,不然空键一直占着登记集,要等 gate 死了才释放。
func (r *sessionReconciler) pruneGateIndex(ctx context.Context, p gateSlot, rep *passReport) error {
	indexKey := gateSessionIndexKey(p.slot, p.gate)
	var cursor uint64
	for {
		var next uint64
		skip := false
		err := r.retryPassPage(ctx, rep, func() string {
			return fmt.Sprintf("sscan gate %s index of slot %d at cursor %d", p.gate, p.slot, cursor)
		}, func() error {
			if cursor != 0 {
				if err := r.scanPacer.Wait(ctx); err != nil {
					return err
				}
			}
			members, n, err := r.player.SScan(ctx, indexKey, cursor, "", r.cfg.ScanCount).Result()
			if err != nil {
				if aerr := rep.keyErrors.absorb(err, rep.actions, reconcileActionGateError,
					fmt.Sprintf("gate_instance=%s slot=%d (SSCAN of its index during a full pass)", p.gate, p.slot)); aerr != nil {
					return aerr
				}
				skip = true
				return nil
			}
			if err := sweepIndexPage(ctx, r.player, p, members, nil, rep.actions, &rep.keyErrors); err != nil {
				return err
			}
			if cursor == 0 && n == 0 && len(members) == 0 {
				if err := releaseGateIndexIfEmptyScript.Run(ctx, r.player, releaseGateIndexKeys(p), p.gate).Err(); err != nil {
					if aerr := rep.keyErrors.absorb(err, rep.actions, reconcileActionGateError,
						fmt.Sprintf("gate_instance=%s slot=%d (release of its empty index)", p.gate, p.slot)); aerr != nil {
						return aerr
					}
				}
			}
			next = n
			return nil
		})
		if err != nil {
			return err
		}
		if skip || next == 0 {
			return nil
		}
		cursor = next
	}
}

// slotRange 返回 [lo, hi) 的 slot 列表。
func slotRange(lo, hi int) []uint16 {
	slots := make([]uint16, 0, hi-lo)
	for s := lo; s < hi; s++ {
		slots = append(slots, uint16(s))
	}
	return slots
}
