package kafka

// D1 第 3 批:go/db 读方门禁(docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §5.1、§5.3、§13.2 第 1–4 条)。
//
// 两层,判据是同一个(shared/placement.RouteReadyReadScript + VerifyRouteReady,规格 §2 M1):
//   - 逐条任务(权威判据,原子):选库与落库后复核用读脚本在同一段 Lua 里取回就绪标记、本实例身份与两把映射键,
//     只有 Trusted 才按读到的值选库(lookupReady);否则返回 errRouteNotReady —— 不可信时值与缺键都不可信,
//     缺键绝不解释为「没有落点」,也绝不回落到本进程 zone 的库。
//   - 进程级就绪闸(调度,routeReadyGate):每 RouteReadyGateIntervalMs 用读脚本(不带映射键)刷新一次 verdict;不是 Trusted
//     时消费循环不从 claim 取新消息、重试循环不分拣也不认领 —— 新任务不 ack、不往 Redis 重试队列推,消息留在 Kafka(lag
//     增长由既有告警覆盖),每次刷新之后复查。闸只决定「取不取新任务」,不替逐条判,已经派进 worker 的任务不再看闸。
//     存盘补投器(saveoutbox)经 RouteGateOpen 看同一把闸:闸关着时整拍跳过、不认领、不计次(r2-rr#5)。
//     走 errRouteNotReady 延后(写不耗预算、读耗预算)的有两类:
//       ① 闸关上那一刻已在 worker 里的任务:每分区 taskCh 1000 条 + SubShardCount×256 条子 shard 缓冲(buildWorker),
//          加上重试循环当拍正在认领的那一条(内层循环每认领一条前看一次闸)。积压追赶期这是通道深度量级(按 etc/db.yaml 的
//          10 分区 × (1000 + 8×256) 约 3 万条),不是「一个刷新间隔内的在途任务」:Kafka 来源的带整条 DBTask(含 Body)经
//          persistBeforeAck 的 WAITAOF 组提交进主 Redis 重试队列,估主 Redis 内存与 AOF 时按这个量级算。要做到这批也不入队,
//          得让 worker 在 handleTask 之前等闸,会改停机排空(drainClaimForShutdown)与 claim 收尾语义,需单独设计,本批不做;
//       ② 闸刚放行、逐条判不可信的竞态,落在一个刷新间隔之内。
//
// Placement.RequireRouteReady=false(缺省)时 placementRouter.ready 与 KeyOrderedKafkaConsumer.routeGate 都是 nil:
// lookup 仍是 MGET 两键,消费循环与重试循环的闸检查恒放行,两个新指标不注册 —— 行为与引入之前相同。
//
// 代价:每条写任务 lookup + confirm 各一次脚本 = 4 次 INFO(§13.2 第 3 条),压测按这个口径估;闸每个间隔再多 2 次。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	db_config "db/internal/config"
	"db/internal/metrics"
	"db/internal/saveoutbox"
	"shared/placement"

	"github.com/IBM/sarama"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// ───────────────────────────── 读脚本 ─────────────────────────────

// routeReadyReader 在同一段只读 Lua 里读就绪标记、本实例身份与调用方给的映射键(placement.RouteReadyReadScript)。
// 实现必须并发安全;返回的错误一律按「查询失败」处理(fail-closed)。生产实现是 redisRouteReadyReader;单测注入假实现
// (miniredis 不支持 INFO 的 server / replication 段,脚本本身只在真 Redis 集成测试里测,见 route_ready_integration_test.go)。
type routeReadyReader interface {
	readRouteReady(ctx context.Context, mappingKeys ...string) (placement.RouteReadyRead, error)
}

// routeReadyReadScript 的 Run 先发 EVALSHA,脚本缓存里没有(NOSCRIPT,例如 Redis 刚重启)时回退 EVAL。
var routeReadyReadScript = redis.NewScript(placement.RouteReadyReadScript)

// redisRouteReadyReader 在落点记录所在的 Redis(Placement.Redis,缺省复用 RedisClient)上执行读脚本。
// 必须与选库读的是同一个客户端:标记里的身份要和映射键出自同一个实例、同一时刻(M1)。
type redisRouteReadyReader struct {
	rc redis.Scripter
}

func (r redisRouteReadyReader) readRouteReady(ctx context.Context, mappingKeys ...string) (placement.RouteReadyRead, error) {
	raw, err := routeReadyReadScript.Run(ctx, r.rc, placement.RouteReadyReadKeys(mappingKeys...)).Result()
	if err != nil {
		return placement.RouteReadyRead{}, fmt.Errorf("route ready read script: %w", err)
	}
	return placement.ParseRouteReadyRead(raw, len(mappingKeys))
}

// routeNotReadyError 是带着 verdict 的 errRouteNotReady:errors.Is 认 errRouteNotReady,errors.As 取 verdict(日志汇总用)。
type routeNotReadyError struct {
	verdict placement.RouteReadyVerdict
}

func (e routeNotReadyError) Error() string {
	return errRouteNotReady.Error() + ": verdict=" + e.verdict.String()
}

func (e routeNotReadyError) Unwrap() error { return errRouteNotReady }

// routeNotReadyVerdict 取错误链里的 verdict 名;链里没有 routeNotReadyError 时回 "unknown"。
func routeNotReadyVerdict(err error) string {
	var e routeNotReadyError
	if errors.As(err, &e) {
		return e.verdict.String()
	}
	return "unknown"
}

// lookupReady 是 Placement.RequireRouteReady=true 时的 lookup(规格 §5.3):一次读脚本取回就绪标记、实例身份与
// player:placement / player:zone 两把键(顺序与 MGET 相同),VerifyRouteReady 判 Trusted 才交给 parsePlacementSnapshot ——
// 选库判定的纯函数(decideWritePlacement / decideReadPlacement / recheckPasses)一格不改。
//
//   - 脚本执行出错 / 回复形状不对:与 MGET 失败同口径,查询失败(fail-closed,耗预算,规格 §5.1 最后一行);
//   - verdict 不是 Trusted:routeNotReadyError(包着 errRouteNotReady),值与缺键都不用;
//   - Trusted 而值畸形:与今天一样按畸形 fail-closed。
func (r *placementRouter) lookupReady(ctx context.Context, key uint64) (placementSnapshot, error) {
	read, err := r.ready.readRouteReady(ctx, placement.Key(key), placement.HomeZoneKey(key))
	if err != nil {
		return placementSnapshot{}, fmt.Errorf("route ready read placement/home_zone for key %d: %w", key, err)
	}
	verdict, _ := placement.VerifyRouteReady(read)
	metrics.CountRouteReadyVerdict(verdict.String())
	if verdict != placement.RouteReadyTrusted {
		return placementSnapshot{}, routeNotReadyError{verdict: verdict}
	}
	snap, err := parsePlacementSnapshot(read.Values)
	if err != nil {
		return placementSnapshot{}, fmt.Errorf("key %d: %w", key, err)
	}
	return snap, nil
}

// ───────────────────────────── 不可信延后的日志汇总 ─────────────────────────────

// 不可信延后的出处(汇总日志的维度之一)。
const (
	routeNotReadyAtLookup  = "lookup"
	routeNotReadyAtRecheck = "recheck"
)

// routeNotReadySummaryEvery:不可信延后逐条只打 DEBUG(与搬库冻结同口径),另外至多每分钟一条 INFO 汇总。
const routeNotReadySummaryEvery = time.Minute

// routeNotReadySummaryFormat 是汇总行的唯一格式(note 与 flush 共用)。预算口径与 retryBudgetExempt 一致:写不耗预算;
// 读在选库时(lookup)被延后耗预算,在落库后复核时(recheck,包在 errPlacementMoved 里)被延后不耗。
const routeNotReadySummaryFormat = "route not ready deferrals (writes keep their retry budget; reads spend it when deferred at lookup, not at recheck): %s"

// routeNotReadySummary 按「op/出处/verdict」累计不可信延后,至多每 every 打一条汇总。并发安全。
// 第一次出现立即打一条(运维不必等满一分钟才知道开始延后);之后的累计在下一次出现或就绪闸下一次刷新(flush)时、
// 距上一条满 every 才打。
type routeNotReadySummary struct {
	mu     sync.Mutex
	now    func() time.Time
	every  time.Duration
	logf   func(format string, args ...any)
	last   time.Time
	counts map[string]int
}

func newRouteNotReadySummary(now func() time.Time, every time.Duration, logf func(format string, args ...any)) *routeNotReadySummary {
	return &routeNotReadySummary{now: now, every: every, logf: logf, counts: map[string]int{}}
}

// routeNotReadyLog 是进程内唯一的汇总器(按原因限速的思路同 store_failure.go 的 storeLogLimiter)。
var routeNotReadyLog = newRouteNotReadySummary(time.Now, routeNotReadySummaryEvery, logx.Infof)

// noteRouteNotReady 记一次不可信延后:op 为 read / write,where 为 lookup / recheck,verdict 取自 err。
func noteRouteNotReady(op, where string, err error) {
	routeNotReadyLog.note(op + "/" + where + "/" + routeNotReadyVerdict(err))
}

func (s *routeNotReadySummary) note(key string) {
	s.mu.Lock()
	s.counts[key]++
	line, ok := s.takeLocked()
	s.mu.Unlock()
	if ok {
		s.logf(routeNotReadySummaryFormat, line)
	}
}

// flush 由就绪闸每次刷新后调用:不可信延后停下之后,最后一段累计也能在一个汇总周期内打出来。
func (s *routeNotReadySummary) flush() {
	s.mu.Lock()
	line, ok := s.takeLocked()
	s.mu.Unlock()
	if ok {
		s.logf(routeNotReadySummaryFormat, line)
	}
}

// takeLocked 在该打汇总时返回汇总行并清零累计。调用方持有 s.mu。
func (s *routeNotReadySummary) takeLocked() (string, bool) {
	if len(s.counts) == 0 {
		return "", false
	}
	now := s.now()
	window := "first occurrence"
	if !s.last.IsZero() {
		if now.Sub(s.last) < s.every {
			return "", false
		}
		window = "last " + now.Sub(s.last).Round(time.Second).String()
	}
	keys := make([]string, 0, len(s.counts))
	for k := range s.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Itoa(s.counts[k]))
	}
	s.counts = map[string]int{}
	s.last = now
	return window + ": " + strings.Join(parts, " "), true
}

// ───────────────────────────── 进程级就绪闸 ─────────────────────────────

const (
	// routeReadyRefreshTimeout / routeReadyProbeTimeout 是就绪闸单次刷新与启动探针的 ctx 时限。落点 Redis 客户端没开
	// ContextTimeoutEnabled,ctx 只约束取连接、拨号与重试间隔;在途的读写由 go-redis 的 ReadTimeout(缺省 3s)约束。
	// 所以 Redis 接受连接却不回包时,一次刷新最长约一个 ReadTimeout,探针约两次,都比这里写的长。停机不受影响:run 在读
	// 返回后先判 ctx、取消之后不再 apply;刷新 goroutine 不在 c.wg 里,Stop 不等它,它最多比 Stop 多活一个 ReadTimeout。
	routeReadyRefreshTimeout = 2 * time.Second
	routeReadyProbeTimeout   = 5 * time.Second
	// routeReadyGateLogEvery 是闸日志的两道限速(apply):
	//   - 翻转(关闸 ERROR / 重开 INFO):距上一条翻转日志不满一个周期的翻转只累加不打,周期到了以后的下一次刷新打一条,
	//     按当时的状态写(关着打 ERROR、开着打 INFO),带 flips=N(距上一条翻转日志的翻转次数)后清零。读脚本时好时坏
	//     (Redis 抖动、超时)时闸可能每次刷新都翻一次,不限速就是每秒数条 ERROR + INFO;限速后至多每分钟一条,
	//     翻转停下之后最后一个状态至多晚一个周期打出来。启动后第一次刷新不占翻转的限速,之后第一次翻转立即打。
	//     状态本身(仪表 db_route_ready_gate、verdict 计数)每次刷新照常更新,不受日志限速影响;
	//   - 闸一直关着(没有翻转):至多每分钟一条 INFO(带关了多久与当前状态),免得长时间全量预热期间无声无息。
	routeReadyGateLogEvery = time.Minute
	// routeReadyGateStateScriptError 是闸刷新时读脚本执行出错(或回复形状不对)的状态名:日志里的 state=,也作为
	// db_route_ready_verdict_total 的一个 verdict 取值计数 —— 与 data_service 的 route_ready_verdict_total、leader 自检
	// route_ready_selfcheck 同名。它不是 placement 的结论,不在 RouteReadyVerdictNames 里,由 newRouteReadyGating 另行预置。
	// 逐条选库 / 复核时脚本出错不计它,计 db_placement_guard_total{outcome="lookup_error"}。取值只在 metrics 定义一处。
	routeReadyGateStateScriptError = metrics.RouteReadyVerdictScriptError
)

// routeReadyGate 是进程级就绪闸(规格 §13.2 第 1 条)。
//
// 状态所有者:open 由刷新 goroutine(run / probe 调 apply)写、任意 goroutine 读(atomic);wake 与日志状态由 mu 保护。
// 每次刷新先写 open、再关闭当前 wake 并换一个新的:等在旧 wake 上的消费循环醒来后读到的一定是新状态。
// 闸关着的初始状态是 closed(open 零值 false),由启动探针给出第一个结论。
//
//   - reader / interval / now:装配后只读;now 只给日志算时长与限速(单测可注入);
//   - waits:消费循环 / 重试循环进入等待的次数(诊断;单测据此同步「已经停在闸前」);
//   - refreshed / closedSince / closedFor / lastLog / lastFlipLog / flips:只给日志用(是否已有结论、本次关闸的时刻、
//     最近一次重开之前关了多久、上一条闸日志的时刻、上一条翻转日志的时刻、距上一条翻转日志的翻转次数),限速见 routeReadyGateLogEvery。
type routeReadyGate struct {
	reader   routeReadyReader
	interval time.Duration
	now      func() time.Time

	open  atomic.Bool
	waits atomic.Int64

	mu          sync.Mutex
	wake        chan struct{}
	refreshed   bool
	closedSince time.Time
	closedFor   time.Duration
	lastLog     time.Time
	lastFlipLog time.Time
	flips       int
}

func newRouteReadyGate(reader routeReadyReader, interval time.Duration) *routeReadyGate {
	return &routeReadyGate{reader: reader, interval: interval, now: time.Now, wake: make(chan struct{})}
}

// newRouteReadyGating 按配置装配读方门禁:Placement.RequireRouteReady=false 时返回 (nil, nil, nil),行为与引入之前相同。
// 打开时逐条读与闸用同一个落点 Redis 客户端,并以 0 预建两个指标的全部序列。间隔正常已由 Config.Validate 拒过越界值,
// 这里再兜一道(直接调用、漏了 Validate 的调用方):越界报错,调用方拒启;0 取缺省值。
func newRouteReadyGating(cfg db_config.PlacementConfig, placementRedis redis.Cmdable) (routeReadyReader, *routeReadyGate, error) {
	if !cfg.RequireRouteReady {
		return nil, nil, nil
	}
	if placementRedis == nil {
		return nil, nil, errors.New("Placement.RequireRouteReady=true needs the placement Redis client")
	}
	ms := cfg.RouteReadyGateIntervalMs
	if ms == 0 {
		ms = db_config.DefaultPlacementRouteReadyGateIntervalMs
	}
	if ms < db_config.MinPlacementRouteReadyGateIntervalMs || ms > db_config.MaxPlacementRouteReadyGateIntervalMs {
		return nil, nil, fmt.Errorf("Placement.RouteReadyGateIntervalMs must be in [%d, %d], got %d",
			db_config.MinPlacementRouteReadyGateIntervalMs, db_config.MaxPlacementRouteReadyGateIntervalMs, ms)
	}
	reader := redisRouteReadyReader{rc: placementRedis}
	metrics.PrimeRouteReady(append(placement.RouteReadyVerdictNames(), routeReadyGateStateScriptError))
	return reader, newRouteReadyGate(reader, time.Duration(ms)*time.Millisecond), nil
}

// opened 报告此刻能不能取新任务。nil 闸(开关关着)恒放行。
func (g *routeReadyGate) opened() bool {
	return g == nil || g.open.Load()
}

// nextRefresh 返回一个在下一次刷新完成时关闭的 channel,不记等待次数:闸放行时的 claim 也要拿它,等待次数(waits)由
// 停在闸前的调用方自己记(gatedMessages / retryClaimsPaused)。nil 闸返回 nil(select 里永不就绪)。
func (g *routeReadyGate) nextRefresh() <-chan struct{} {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.wake
}

// read 执行一次不带映射键的读脚本并判 verdict。err 非 nil 时 verdict 没有意义(取 Absent,防止被误当成 Trusted)。
func (g *routeReadyGate) read(ctx context.Context, timeout time.Duration) (placement.RouteReadyVerdict, error) {
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, err := g.reader.readRouteReady(readCtx)
	if err != nil {
		return placement.RouteReadyAbsent, err
	}
	verdict, _ := placement.VerifyRouteReady(r)
	return verdict, nil
}

// apply 记下一次刷新的结论并唤醒全部等待者。读脚本出错(err 非 nil)按闸关处理(fail-closed),verdict 计 script_error
// (routeReadyGateStateScriptError),让「读不了」与「标记不在」在 db_route_ready_verdict_total 上分得开。
// 日志按 routeReadyGateLogEvery 限速(翻转与「仍关着」各一道);open 与 wake 每次刷新照常更新,不受限速影响。
func (g *routeReadyGate) apply(verdict placement.RouteReadyVerdict, err error) {
	open := err == nil && verdict == placement.RouteReadyTrusted
	state := routeReadyGateStateScriptError
	if err == nil {
		state = verdict.String()
	}
	metrics.CountRouteReadyVerdict(state)
	metrics.SetRouteReadyGate(open)

	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	was, first := g.open.Load(), !g.refreshed
	g.refreshed = true
	g.open.Store(open)
	switch {
	case first && !open:
		g.closedSince = now
	case !first && open != was:
		g.flips++
		if open {
			g.closedFor = now.Sub(g.closedSince)
			g.closedSince = time.Time{}
		} else {
			g.closedSince = now
		}
	}
	switch {
	case first && open:
		g.lastLog = now
		logx.Infof("route ready gate open: mapping Redis replica trusted, consumption proceeds (interval=%v)", g.interval)
	case first:
		g.lastLog = now
		g.logClosed(state, err)
	case g.flips > 0 && now.Sub(g.lastFlipLog) >= routeReadyGateLogEvery:
		// 距上一条翻转日志满一个周期:按此刻的状态打一条,带上期间累计的翻转次数。
		g.lastLog, g.lastFlipLog = now, now
		if open {
			logx.Infof("route ready gate reopened after %v closed: Kafka and retry consumption resume (flips=%d)",
				g.closedFor.Round(time.Millisecond), g.flips)
		} else {
			g.logClosed(state, err)
		}
		g.flips = 0
	case !open && now.Sub(g.lastLog) >= routeReadyGateLogEvery:
		// 走到这里 flips 必为 0:lastLog 不早于 lastFlipLog,翻转日志若被限速,这一格同样不满周期。
		g.lastLog = now
		logx.Infof("route ready gate still closed after %v: state=%s err=%v",
			now.Sub(g.closedSince).Round(time.Second), state, err)
	}
	close(g.wake)
	g.wake = make(chan struct{})
}

// logClosed 打关闸 ERROR。已经派进 worker 的任务不看闸(文件头 ①),这里如实写出它们的去向。调用方持有 g.mu。
func (g *routeReadyGate) logClosed(state string, err error) {
	logx.Errorf("route ready gate closed: Kafka and retry consumption paused until the mapping Redis replica is trusted again; "+
		"new Kafka messages are not taken and their offsets stay uncommitted, while tasks already dispatched to workers are still "+
		"checked one by one and deferred as route_not_ready (Kafka-sourced ones go to the retry queue and their offsets are committed): "+
		"state=%s err=%v interval=%v flips=%d", state, err, g.interval, g.flips)
}

// probe 是启动探针(规格 §13.2 第 2 条):试跑一次读脚本(不带映射键),执行出错(例如脚本里调不了 INFO、Redis 连不上、
// 回复形状不对)返回错误,调用方拒启。verdict 不可信不是错误:闸照样关着,等对账 leader 挂上标记。
func (g *routeReadyGate) probe(ctx context.Context) error {
	verdict, err := g.read(ctx, routeReadyProbeTimeout)
	if err != nil {
		return fmt.Errorf("route ready probe failed (Placement.RequireRouteReady=true needs the read script to run on the placement Redis): %w", err)
	}
	g.apply(verdict, nil)
	return nil
}

// run 每收到一个 tick 刷新一次,ctx 取消或 ticks 关闭时返回。tick 由调用方给出(Start 用 time.Ticker),单测逐拍驱动。
// 停机时被取消的那次读不记成读失败(不关闸、不打日志)。
func (g *routeReadyGate) run(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			verdict, err := g.read(ctx, routeReadyRefreshTimeout)
			if ctx.Err() != nil {
				return
			}
			g.apply(verdict, err)
			routeNotReadyLog.flush()
		}
	}
}

// startRouteReadyGate 在 Start 的第一步调用:开关关着(routeGate 为 nil)什么都不做;打开时先跑启动探针(出错即返回,
// Start 据此拒启),再起按间隔刷新的 goroutine,随 c.ctx(Stop 第④步)退出。消费循环与重试循环的闸检查都在它之后才开始。
// 探针通过、Start 的后续步骤(入队层探针、重试归属)失败时,刷新 goroutine 已经在跑,要等调用方 Stop 取消 c.ctx 才退出
// (db.go 在 Start 之前就 defer 了 Stop)。
//
// 停机接缝(docs/design/db-task-pipeline-bottlenecks.md G6「就绪闸」):闸只在 ConsumeClaim 顶层 select 里替换通道值,
// 与撤拉取、会话 ctx、交接让出、failureCh 同在一个 select;重试循环的闸检查不阻塞。闸不得挪进 Start 的入组循环(被闸住的
// 成员会离组,并和重新入组退避互相绕开),也不得写成 select 之外的阻塞等待(Stop 会被闸卡住)。回归用例见
// route_ready_gate_shutdown_test.go。
func (c *KeyOrderedKafkaConsumer) startRouteReadyGate() error {
	g := c.routeGate
	if g == nil {
		return nil
	}
	if err := g.probe(c.ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(g.interval)
	go func() {
		defer ticker.Stop()
		g.run(c.ctx, ticker.C)
	}()
	return nil
}

// gatedMessages 给 ConsumeClaim 的主循环用,返回本轮 select 的消息通道与「下一次刷新完成」的通道(醒来后主循环重新判一次):
//   - 闸为 nil(开关关着):claim 的消息通道与 nil,行为与引入之前相同;
//   - 闸放行:claim 的消息通道与刷新通道。刷新把闸关上时 select 立即醒来、回到循环顶停在闸前。只返回消息通道的话,空闲分区上
//     等消息的 claim 醒不过来:闸关之后不论隔多久,下一条到达的消息照样被取走,逐条判不可信后写推进重试队列、读耗预算,
//     与「闸关着不取新消息、不推重试队列」不符。代价是每个 claim 每个刷新间隔(≥200ms)空转一圈;
//   - 闸关着:nil 通道(select 里永不就绪 = 不取新消息、不 track、不派发)与刷新通道,并记一次等待(waits)。
//
// 先拿通道、再判状态:两步之间刚好刷新时,拿到的通道已经关闭,不会白等一个间隔。剩下的竞态只有「刷新关闸的那一刻 wake 关闭
// 与消息同时就绪、select 选中了消息」,落在一个刷新间隔之内,由逐条判(lookupReady)兜住。
func (c *KeyOrderedKafkaConsumer) gatedMessages(claim sarama.ConsumerGroupClaim) (<-chan *sarama.ConsumerMessage, <-chan struct{}) {
	g := c.routeGate
	if g == nil {
		return claim.Messages(), nil
	}
	wake := g.nextRefresh()
	if g.opened() {
		return claim.Messages(), wake
	}
	g.waits.Add(1)
	return nil, wake
}

// retryClaimsPaused 报告重试循环本拍是否停在闸前(不分拣、不认领);闸为 nil 时恒为 false。
func (c *KeyOrderedKafkaConsumer) retryClaimsPaused() bool {
	if c.routeGate.opened() {
		return false
	}
	c.routeGate.waits.Add(1)
	return true
}

// 存盘补投器按本实例的就绪闸暂停(saveoutbox.RouteGate,r2-rr#5),装配见 db.go。
var _ saveoutbox.RouteGate = (*KeyOrderedKafkaConsumer)(nil)

// RouteGateOpen 报告本实例的读方就绪闸此刻是否放行新任务,闸为 nil(开关关着)恒为 true。只读、并发安全(routeGate 装配后
// 不再改,open 是 atomic),不记等待次数 —— waits 只给停在闸前的消费循环与重试循环用。
// 给存盘补投器用:闸关着时消费循环不取新消息、重试循环不认领,补投出去的任务只会堆在 Kafka 里挤占保留缓冲,broker 确认后
// 却照常计次;补投器据此整拍跳过、不计次,与重试循环同口径。
func (c *KeyOrderedKafkaConsumer) RouteGateOpen() bool {
	return c.routeGate.opened()
}
