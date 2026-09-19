package assetop

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/zeromicro/go-zero/core/logx"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"

	"shared/safego"
)

// 重投循环(规格 §4.19 reconcile.go、§4.37「领取改两步、有界并发、毒行跳过」、§4.38)。
//
// 它只做三件事:把到期的待办行领出来、投一次、按结论决定终结还是重排。
// 业务副作用(帮会资金、帮贡、退款)全部在业务方的 Store.Finalize 事务里,本包不碰。

// 领取与轮询的固定节律。它们不是业务可调项,所以不放进 LoopConfig。
const (
	// pendingAgeInterval 是「最老未决行年龄」这个 Gauge 的刷新间隔。
	pendingAgeInterval = 30 * time.Second
	// leaseHeadroom 是租约相对单行预算必须留出的余量:领到行之后才开始计时,
	// 处理最多花 OpBudget,再加上这点余量才不会出现「还在处理、租约已过期被别人抢走」。
	leaseHeadroom = 2 * time.Second
	// maxWorkers 是并发上限的上限,见 LoopConfig.Workers 的注释。
	maxWorkers = 64
)

// ErrPoisonRow 表示这一行的 payload 解不开(毒行)。
// Store 负责把它推迟到很久以后,循环跳过它继续下一行 —— 一行坏数据不该让整批停摆。
var ErrPoisonRow = errors.New("assetop: op payload undecodable")

// Store 是业务 outbox 表在本包眼里的样子。实现在业务方(帮会 B5 / 聚宝斋 P2)。
//
// 为什么领取分两步:早先的写法是 `UPDATE … WHERE status AND next_attempt_ms ORDER BY LIMIT`,
// 它在 MySQL 可重复读下按范围加 next-key 锁,会和业务事务里 AllocateSeq 的 FOR UPDATE +
// 插入新行互相等待;被选为死锁牺牲者的往往是**用户请求**。改成「非加锁读一批 id + 主键 CAS
// 逐行领取」之后,重投循环任何时刻只锁一行。
type Store interface {
	// ListDue 非加锁一致性读,走 (status, next_attempt_ms) 索引,只回主键:
	//   SELECT op_id FROM {op}
	//    WHERE status=<pending> AND next_attempt_ms<=? AND lease_until_ms<?
	//    ORDER BY next_attempt_ms LIMIT ?
	ListDue(ctx context.Context, nowMs uint64, limit int) ([]uint64, error)

	// Claim 单行主键 CAS 领取(autocommit),紧挨着处理前调用:
	//   UPDATE {op} SET lease_until_ms=?, lease_token=?
	//    WHERE op_id=? AND status=<pending> AND lease_until_ms<?
	// RowsAffected==0 → (Op{}, false, nil):行已被别的副本领走或已终结,跳过即可。
	// RowsAffected==1 → 回读整行并解 payload。
	// 解码失败时 Store 自己把行推迟(next_attempt_ms=now+PoisonDelay、lease_until_ms=0),
	// 并返回 (Op{}, false, ErrPoisonRow)。
	Claim(ctx context.Context, opID, nowMs, leaseUntilMs, token uint64) (Op, bool, error)

	// Finalize 在**一个事务**里(建议包 WithTxRetry)按业务锁序加锁 →
	//   UPDATE {op} SET status=?, durable=1, last_outcome=?, last_reason=?, updated_ms=?
	//    WHERE op_id=? AND status=<pending>
	// 只有 RowsAffected==1 才做对侧账(入资金 / 退帮贡),返回是否**本次**终结。
	// 这一条是「一次且仅一次」的最后一道闸:重复投递到这里会因为 status 已变而 0 行,
	// 对侧账自然不会做第二遍。
	Finalize(ctx context.Context, op Op, status Status, res Result, nowMs uint64) (bool, error)

	// Reschedule 推进下一次投递时刻:
	//   UPDATE {op} SET attempts=attempts+1, next_attempt_ms=?, lease_until_ms=0,
	//                   durable=?, last_outcome=?, last_reason=?, updated_ms=?
	//    WHERE op_id=? AND status=<pending> AND lease_token=?
	// 带回领取令牌是为了:租约已经被别人抢走时,自己这次的结果不该再写回去。
	//
	// last_reason 照传进来的 res.Reason 写即可,**不要**自行"改回最新原因":
	// 一旦它是 ReasonPartialApplied(27007),本包就不会再往里传别的值
	// (见 carryPartialReason),那个码是部分发放在 partial_seqs 环被挤掉之后的
	// 唯一证据,业务方各写各的会把它抹掉。
	Reschedule(ctx context.Context, op Op, nextAttemptMs uint64, res Result, nowMs uint64) error
}

// Applier 是「把一条指令投给 scene」的能力。生产实现是 *Caller,单测注入假的。
type Applier interface {
	Do(ctx context.Context, rpc RPC, req *assetpb.AssetOpRequest) (Result, error)
}

// LedgerReader 读玩家**已落盘**的账本(zone Redis 里的玩家 blob)。
// 实现在 B5(data_service 新增只读 RPC GetPlayerAssetOpLedger)。
// 玩家从未落过盘时返回 (nil, nil)。
type LedgerReader interface {
	ReadPersistedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error)
}

// PendingAgeReader 是 Store 的**可选**能力:报告某条流最老未决行的创建时刻。
// 实现了它,Loop 就会每 30s 刷新 assetop_pending_oldest_age_seconds(卡死行告警靠它)。
type PendingAgeReader interface {
	OldestPendingCreatedMs(ctx context.Context, stream assetpb.AssetOpStream) (uint64, bool, error)
}

// ManualResolution 是一次人工终结的完整输入。
type ManualResolution struct {
	OpID uint64
	// Final 只能是四个终结状态之一。
	Final Status
	// Operator 操作人,≤64 字符;进日志与 resolved_by 列,便于事后追责。
	Operator string
	// Reason 人工判定依据,≤191 字符(与列宽一致)。
	Reason string
}

// ManualResolver 是业务方提供的人工终结通道(规格 §4.38)。
// 单事务:业务锁序加锁 → 改 status / resolved_by / resolve_reason,
// RowsAffected==1 才做对侧账(Applied 入账;Rejected / Aborted 退款;AppliedPartial 不动)。
type ManualResolver interface {
	ResolveManually(ctx context.Context, r ManualResolution, nowMs uint64) (bool, error)
}

// LoopConfig 是重投循环的节律与预算。零值不可用,请从 DefaultLoopConfig 改。
type LoopConfig struct {
	// Interval 两次 Tick 之间的间隔。
	Interval time.Duration
	// Batch 一次 Tick 最多领多少行。
	Batch int
	// Workers 是**同时在途的资产 RPC 条数上限**,也是本配置里最不能随手调大的一项。
	//
	// 理由:scene 的资产 RPC 是同步 gRPC —— 每条在途请求占住 scene 一条 sync server
	// poller 线程,而整个 scene 进程的 poller 上限默认只有 8
	// (GRPC_SERVER_MAX_POLLERS,cpp/libs/engine/core/node/system/node/node.cpp:632-644,
	// 另见 docs/design/scene-node-threading-model.md)。
	// 不设上限时,一次 Tick 的 100 行会同时压向同一个 scene 节点,把 poller 占满;
	// 排在后面的不只是资产请求,还有 scene_manager 的 CreateScene、match 的 PrepareBattle
	// —— 玩家会卡在进场和开战上,而根因在一个后台补偿循环里,极难联想。
	//
	// 另外这是**每个服务副本**的上限:副本数 × Workers 才是 scene 看见的并发,
	// 扩副本时要连带复核。
	Workers int
	// Lease 领取租约时长。必须 >= OpBudget + leaseHeadroom。
	Lease time.Duration
	// OpBudget 单行的处理时间预算(含 RPC 与 durable 重查)。
	OpBudget time.Duration
	// BaseBackoff 退避基数;帮会取 GuildRule.asset_op_retry_base_ms。
	BaseBackoff time.Duration
	// MaxBackoff 退避封顶,同时也是 Alert 分支的重排间隔。
	MaxBackoff time.Duration
	// AwaitDurableDelay 「结局有了但没落盘」时的短延迟。
	AwaitDurableDelay time.Duration
	// PoisonDelay 只作为给 Store 的约定值:毒行推迟多久再看。本包不直接用它。
	PoisonDelay time.Duration
	// LedgerReadMinAttempts 连续几次拿不到位置之后,才去读已落盘账本。
	// 设这个门槛是为了不给 data_service 增加无谓的读:刚离线的玩家很快会回来。
	LedgerReadMinAttempts uint32
	// Streams 是本服务独占的流(不变量 I6)。只用于刷新最老未决行年龄;留空则不刷。
	Streams []assetpb.AssetOpStream
}

// DefaultLoopConfig 是规格 §4.37 的取值。
func DefaultLoopConfig() LoopConfig {
	return LoopConfig{
		Interval:              2 * time.Second,
		Batch:                 100,
		Workers:               8,
		Lease:                 10 * time.Second,
		OpBudget:              2500 * time.Millisecond,
		BaseBackoff:           time.Second,
		MaxBackoff:            60 * time.Second,
		AwaitDurableDelay:     500 * time.Millisecond,
		PoisonDelay:           time.Hour,
		LedgerReadMinAttempts: 3,
	}
}

func (c LoopConfig) validate() error {
	if c.Interval <= 0 {
		return fmt.Errorf("assetop: Interval 必须为正(当前 %v)", c.Interval)
	}
	if c.Workers < 1 || c.Workers > maxWorkers {
		return fmt.Errorf("assetop: Workers 必须在 [1, %d](当前 %d)", maxWorkers, c.Workers)
	}
	if c.Batch < c.Workers {
		return fmt.Errorf("assetop: Batch(%d)不得小于 Workers(%d)", c.Batch, c.Workers)
	}
	if c.OpBudget <= 0 {
		return fmt.Errorf("assetop: OpBudget 必须为正(当前 %v)", c.OpBudget)
	}
	// 租约必须覆盖「处理耗时 + 余量」,否则一行还在处理中就被另一个副本领走,
	// 同一个 seq 会被两个副本同时投递 —— scene 侧虽然只读答复不会重办,
	// 但两边都会去写同一行 outbox,对侧账的幂等就只剩 Finalize 的 CAS 一道防线了。
	if c.OpBudget+leaseHeadroom > c.Lease {
		return fmt.Errorf("assetop: Lease(%v)必须 >= OpBudget(%v) + %v", c.Lease, c.OpBudget, leaseHeadroom)
	}
	if c.BaseBackoff <= 0 || c.MaxBackoff < c.BaseBackoff {
		return fmt.Errorf("assetop: 退避区间非法(base=%v max=%v)", c.BaseBackoff, c.MaxBackoff)
	}
	if c.AwaitDurableDelay <= 0 {
		return fmt.Errorf("assetop: AwaitDurableDelay 必须为正(当前 %v)", c.AwaitDurableDelay)
	}
	return nil
}

// Processed 是处理一行的结果,供同步路径(业务写 RPC 提交后立刻投一次)判断该回什么。
type Processed struct {
	Result    Result
	Finalized bool
	Status    Status
}

// Loop 是重投循环。构造后除下面几个可选字段外只读。
type Loop struct {
	cfg     LoopConfig
	store   Store
	applier Applier
	metrics *Metrics
	now     func() time.Time

	// Ledger 可选:能读已落盘账本时,长期离线且已记账的行可以提前终结(规格 §4.37)。
	// 在 Run / ProcessOne 之前设置。
	Ledger LedgerReader
	// Manual 可选:人工终结通道(规格 §4.38)。
	Manual ManualResolver
	// Rand 可选:退避抖动的随机源,单测注入确定值。为 nil 时取中值,不抖动。
	Rand func() float64
}

// NewLoop 校验配置并组装循环。校验失败就别启动服务:这些都是一眼能看出的配置错,
// 留到运行时才炸没有任何好处。
func NewLoop(cfg LoopConfig, store Store, applier Applier, m *Metrics, now func() time.Time) (*Loop, error) {
	if store == nil || applier == nil {
		return nil, errors.New("assetop: NewLoop 缺少 store 或 applier")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Loop{cfg: cfg, store: store, applier: applier, metrics: m, now: now}, nil
}

func (l *Loop) nowMs() uint64 { return uint64(l.now().UnixMilli()) }

// Run 按 Interval 反复 Tick,直到 ctx 结束。调用方用 safego.Go 启动它。
//
// 每一轮各自 recover:某一轮炸掉只丢那一轮,循环继续 —— 一个补偿循环静默停摆
// 比单轮出错危险得多。
func (l *Loop) Run(ctx context.Context) {
	ticker := time.NewTicker(l.cfg.Interval)
	defer ticker.Stop()
	ageTicker := time.NewTicker(pendingAgeInterval)
	defer ageTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			safego.Run("assetop.reconcile_tick", func() { l.Tick(ctx) })
		case <-ageTicker.C:
			safego.Run("assetop.pending_age", func() { l.refreshPendingAge(ctx) })
		}
	}
}

// Tick 领一批到期行并逐行处理,返回实际处理了多少行。
//
// 并发严格受 Workers 限制,原因见 LoopConfig.Workers。Tick 等所有 worker 结束才返回,
// 于是「同时在途的资产 RPC」在一个副本内恒 <= Workers,不会因为上一轮没跑完就叠加。
func (l *Loop) Tick(ctx context.Context) int {
	ids, err := l.store.ListDue(ctx, l.nowMs(), l.cfg.Batch)
	if err != nil {
		l.metrics.incStoreError("list")
		logx.Errorf("[AssetOp] 列出到期行失败: %v", err)
		return 0
	}
	if len(ids) == 0 {
		return 0
	}

	queue := make(chan uint64, len(ids))
	for _, id := range ids {
		queue <- id
	}
	close(queue)

	workers := l.cfg.Workers
	if workers > len(ids) {
		workers = len(ids)
	}
	var (
		processed atomic.Int64
		wg        sync.WaitGroup
	)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		safego.Go("assetop.reconcile_worker", func() {
			defer wg.Done()
			l.drain(ctx, queue, &processed)
		})
	}
	wg.Wait()
	return int(processed.Load())
}

// drain 是一条 worker 的主体:逐个领取并处理,直到队列空或 ctx 结束。
func (l *Loop) drain(ctx context.Context, queue <-chan uint64, processed *atomic.Int64) {
	for opID := range queue {
		if ctx.Err() != nil {
			return
		}
		op, ok := l.claim(ctx, opID)
		if !ok {
			continue
		}
		if _, err := l.ProcessOne(ctx, op); err != nil {
			// 单行失败只记不抛:下一行照常处理,这一行靠 next_attempt_ms 再来。
			logx.Errorf("[AssetOp] 处理待办行失败 op_id=%d stream=%d seq=%d: %v",
				op.OpID, op.Stream, op.Seq, err)
		}
		processed.Add(1)
	}
}

// claim 领一行。返回 false 表示这一行本轮不处理(被别人领走、已终结、毒行或出错)。
func (l *Loop) claim(ctx context.Context, opID uint64) (Op, bool) {
	token, err := newLeaseToken()
	if err != nil {
		l.metrics.incStoreError("claim")
		logx.Errorf("[AssetOp] 生成租约令牌失败 op_id=%d: %v", opID, err)
		return Op{}, false
	}
	nowMs := l.nowMs()
	leaseUntilMs := nowMs + uint64(l.cfg.Lease/time.Millisecond)

	op, claimed, err := l.store.Claim(ctx, opID, nowMs, leaseUntilMs, token)
	switch {
	case errors.Is(err, ErrPoisonRow):
		// 毒行:Store 已经把它推远了。这里只计数 + 报位置,人工去看。
		l.metrics.incClaim("poison")
		l.metrics.incStoreError("decode")
		logx.Errorf("[AssetOp] 待办行 payload 解不开,已推迟待人工处理 op_id=%d", opID)
		return Op{}, false
	case err != nil:
		l.metrics.incStoreError("claim")
		logx.Errorf("[AssetOp] 领取待办行失败 op_id=%d: %v", opID, err)
		return Op{}, false
	case !claimed:
		l.metrics.incClaim("lost")
		return Op{}, false
	}
	l.metrics.incClaim("claimed")
	return op, true
}

// ProcessOne 处理一行:决定方向 → 投一次 → 按 Decide 分支落地。
//
// 业务写 RPC 在提交自己的事务之后可以**同步**调用它,让玩家当场看到结果;
// 那条路径的 ctx 预算是 2500ms(= OpBudget),CallTimeout 800ms + 重查 700ms 仍在内。
func (l *Loop) ProcessOne(ctx context.Context, op Op) (Processed, error) {
	nowMs := l.nowMs()

	rpc, ok := l.rpcFor(op, nowMs)
	if !ok {
		// 流号非法 = 坏行。绝不猜方向:猜错就是把扣钱发成发钱。
		l.metrics.incUnknown(op.Stream)
		logx.Errorf("[AssetOp] 待办行的流号非法,无法决定投递方向 op_id=%d stream=%d seq=%d",
			op.OpID, op.Stream, op.Seq)
		return l.reschedule(ctx, op, l.alertNextMs(nowMs), Result{}, nowMs, "alert")
	}

	callCtx, cancel := context.WithTimeout(ctx, l.cfg.OpBudget)
	defer cancel()
	res, err := l.applier.Do(callCtx, rpc, op.Request())

	// 没发出去(玩家不在线)且已经试过几次:去读已落盘账本,已见的结局可以直接终结。
	if err == nil && res.Local {
		res = l.tryPersistedLedger(ctx, op, res)
	}

	switch Decide(res, err) {
	case ActionFinalize:
		return l.finalize(ctx, op, rpc, res, nowMs)
	case ActionAwaitDurable:
		next := nowMs + uint64(l.cfg.AwaitDurableDelay/time.Millisecond)
		return l.reschedule(ctx, op, next, res, nowMs, "await_durable")
	case ActionRetry:
		next := NextAttemptMs(nowMs, op.Attempts, l.cfg.BaseBackoff, l.cfg.MaxBackoff, l.Rand)
		return l.reschedule(ctx, op, next, res, nowMs, "retry")
	default: // ActionAlert
		if errors.Is(err, ErrOutcomeFlip) {
			// 翻转已由 Caller 计过 assetop_outcome_flip_total,这里只留日志证据。
			logx.Errorf("[AssetOp] 结局翻转,不终结 op_id=%d stream=%d seq=%d: %v",
				op.OpID, op.Stream, op.Seq, err)
		} else {
			l.metrics.incUnknown(op.Stream)
			logx.Errorf("[AssetOp] scene 回 UNKNOWN,不终结 op_id=%d stream=%d seq=%d epoch=%d reason=%d",
				op.OpID, op.Stream, op.Seq, op.StreamEpoch, res.Reason)
		}
		return l.reschedule(ctx, op, l.alertNextMs(nowMs), res, nowMs, "alert")
	}
}

// rpcFor 决定这次发哪个方法:过了业务截止时间就改发中止,否则按流的方向发。
func (l *Loop) rpcFor(op Op, nowMs uint64) (RPC, bool) {
	if op.DeadlineMs != 0 && nowMs >= op.DeadlineMs {
		return RPCAbort, true
	}
	return ApplyRPCOf(op.Stream)
}

func (l *Loop) alertNextMs(nowMs uint64) uint64 {
	return nowMs + uint64(l.cfg.MaxBackoff/time.Millisecond)
}

// finalize 终结一行。状态算不出来时**不写库**:那说明 Decide 与 FinalStatus 之间有 bug,
// 按坏行告警比按 Pending 写进 status 列安全得多。
func (l *Loop) finalize(ctx context.Context, op Op, rpc RPC, res Result, nowMs uint64) (Processed, error) {
	status := FinalStatus(rpc, res, op)
	if status == StatusPending {
		l.metrics.incUnknown(op.Stream)
		logx.Errorf("[AssetOp] 终结分支算不出最终状态(bug)op_id=%d stream=%d seq=%d outcome=%d",
			op.OpID, op.Stream, op.Seq, res.Outcome)
		return l.reschedule(ctx, op, l.alertNextMs(nowMs), res, nowMs, "alert")
	}

	finalized, err := l.store.Finalize(ctx, op, status, res, nowMs)
	if err != nil {
		l.metrics.incStoreError("finalize")
		return Processed{Result: res}, fmt.Errorf("assetop: 终结失败 op_id=%d: %w", op.OpID, err)
	}
	if finalized {
		l.metrics.incFinalize(op.Stream, status)
		if status == StatusAppliedPartial {
			// 部分发放不做对侧账,必须留下人工补偿线索(规格 §4.33)。
			logx.Errorf("[AssetOp] 部分发放,已终结但不入对侧账,需人工补偿 op_id=%d stream=%d seq=%d corr=%d",
				op.OpID, op.Stream, op.Seq, op.CorrelationID)
		}
	}
	return Processed{Result: res, Finalized: finalized, Status: status}, nil
}

// reschedule 推进下一次投递时刻。
//
// 写库前过一道 carryPartialReason:last_reason 对 27007 必须是粘性的。
// 返回给调用方的仍是**本次真实答复**(同步路径要按它告诉玩家发生了什么),
// 被改的只有落到 outbox 行上的那一份。
func (l *Loop) reschedule(ctx context.Context, op Op, nextAttemptMs uint64, res Result, nowMs uint64, reason string) (Processed, error) {
	if err := l.store.Reschedule(ctx, op, nextAttemptMs, carryPartialReason(op, res), nowMs); err != nil {
		l.metrics.incStoreError("reschedule")
		return Processed{Result: res}, fmt.Errorf("assetop: 重排失败 op_id=%d: %w", op.OpID, err)
	}
	l.metrics.incReschedule(op.Stream, reason)
	return Processed{Result: res}, nil
}

// carryPartialReason 让 outbox 行上的 last_reason 对「曾见部分发放」保持粘性。
//
// 规格 §4.33 把 op.LastReason 当成 partial_seqs 环被挤掉之后**唯一**的部分发放证据,
// 但它给的机制(Reschedule 写 res.Reason)粘不住:传输失败(caller.go 返回 Result{})、
// 本地 NOT_HERE、坏流号重排(ProcessOne 传 Result{})带的 Reason 都是 0,玩家下线一次
// 或过一次图就能把 27007 抹平;之后 scene 若恰好不再回 partial,FinalStatus 两个分支
// 都取不到证据,这一行会被当成 StatusApplied 终结并做**全额**对侧账。
//
// 代价是这一行之后不再记录更新的拒绝/重试原因(背包满之类)。那只是展示信息,日志与
// assetop_rpc_total 里都还在;而抹掉 27007 造成的半额入账不可逆 —— 按 AGENTS §11.3,
// 玩家资产路径取 fail-closed 的那一侧。
//
// 只作用于 Reschedule:Finalize 那一侧的 FinalStatus 直接读 op.LastReason,状态列不会
// 因为 res.Reason 是 0 而判错,没必要再去动那条路径上的 reason(动了反而会把
// 「Abort 且 reason==0 → StatusAborted」这条判定带偏)。
func carryPartialReason(op Op, res Result) Result {
	if op.LastReason == ReasonPartialApplied && res.Reason != ReasonPartialApplied {
		res.Reason = ReasonPartialApplied
	}
	return res
}

// tryPersistedLedger 在玩家长期不在线时,从已落盘账本里把结局读回来。
//
// 为什么安全:读的是已经写进 Redis 的那份数据,而已记账的结局永不改变(不变量 I2),
// 所以玩家在不在线都不影响结论,读到的结局天然就是 durable 的。
// 读不到结局的行**不**在 Go 侧自行判中止(不变量 I7):scene 没记账就无法保证
// 这个 seq 将来不会被一个晚到的请求应用。
func (l *Loop) tryPersistedLedger(ctx context.Context, op Op, res Result) Result {
	if l.Ledger == nil || op.Attempts < l.cfg.LedgerReadMinAttempts {
		return res
	}

	ledger, err := l.Ledger.ReadPersistedLedger(ctx, op.PlayerID)
	if err != nil {
		l.metrics.incLedgerRead("error")
		l.metrics.incStoreError("ledger_read")
		logx.Errorf("[AssetOp] 读已落盘账本失败 op_id=%d: %v", op.OpID, err)
		return res
	}
	if ledger == nil {
		l.metrics.incLedgerRead("absent")
		return res
	}

	outcome, partial, reason := ClassifyPersisted(ledger, op.Stream, op.StreamEpoch, op.Seq)
	if !isSceneTerminal(outcome) {
		l.metrics.incLedgerRead("unseen")
		return res
	}
	l.metrics.incLedgerRead("finalized")
	logx.Infof("[AssetOp] 离线读到已落盘结局,可终结 op_id=%d stream=%d seq=%d outcome=%s partial=%t",
		op.OpID, op.Stream, op.Seq, outcomeLabel(outcome), partial)
	return Result{Outcome: outcome, Reason: reason, Durable: true, Partial: partial}
}

// refreshPendingAge 刷新「最老未决行年龄」。Store 没实现 PendingAgeReader,
// 或没声明本服务独占哪些流时,静默跳过(这是可选可观测性,不是正确性路径)。
func (l *Loop) refreshPendingAge(ctx context.Context) {
	reader, ok := l.store.(PendingAgeReader)
	if !ok || len(l.cfg.Streams) == 0 {
		return
	}
	nowMs := l.nowMs()
	for _, stream := range l.cfg.Streams {
		createdMs, found, err := reader.OldestPendingCreatedMs(ctx, stream)
		if err != nil {
			l.metrics.incStoreError("pending_age")
			logx.Errorf("[AssetOp] 读最老未决行失败 stream=%d: %v", stream, err)
			continue
		}
		if !found {
			l.metrics.setPendingOldestAge(stream, 0)
			continue
		}
		age := 0.0
		if nowMs > createdMs {
			age = float64(nowMs-createdMs) / 1000
		}
		l.metrics.setPendingOldestAge(stream, age)
	}
}

// ResolveManually 走人工终结通道。它不查证据、不猜结论:调用方(管理 RPC)已经按
// scene 流水 correlation_id 判定过,这里只负责落库 + 留痕 + 计数。
//
// 人工终结会让 next_seq 与 scene 的 max_seq 拉开距离,累计超过 511 之后新 seq 会被
// 判 kJumpTooFar —— 那是 fail-closed 的预期行为,按运维手册抬纪元即可。
func (l *Loop) ResolveManually(ctx context.Context, r ManualResolution) (bool, error) {
	if l.Manual == nil {
		return false, errors.New("assetop: 未配置人工终结通道")
	}
	switch r.Final {
	case StatusApplied, StatusRejected, StatusAborted, StatusAppliedPartial:
	default:
		return false, fmt.Errorf("assetop: 人工终结状态非法(%s)", r.Final)
	}
	if r.Operator == "" || utf8.RuneCountInString(r.Operator) > 64 {
		return false, errors.New("assetop: 人工终结必须带操作人,且不超过 64 字")
	}
	if utf8.RuneCountInString(r.Reason) > 191 {
		return false, errors.New("assetop: 人工终结理由不得超过 191 字")
	}

	resolved, err := l.Manual.ResolveManually(ctx, r, l.nowMs())
	if err != nil {
		l.metrics.incStoreError("manual_resolve")
		return false, fmt.Errorf("assetop: 人工终结失败 op_id=%d: %w", r.OpID, err)
	}
	if resolved {
		l.metrics.incManualResolve(r.Final)
		logx.Infof("[AssetOp] manual op_id=%d final=%s operator=%s reason=%s",
			r.OpID, r.Final, r.Operator, r.Reason)
	}
	return resolved, nil
}

// newLeaseToken 生成一次领取的令牌。
//
// 用 crypto/rand 而不是 math/rand:令牌是「这次领取是不是我」的唯一凭据,
// 可预测的令牌意味着两个副本可能撞上同一个值,Reschedule 的 CAS 就形同虚设。
// 最低位置 1 保证非零 —— 0 在表里表示「没有租约」。
func newLeaseToken() (uint64, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0, fmt.Errorf("assetop: 读随机数失败: %w", err)
	}
	return binary.LittleEndian.Uint64(buf[:]) | 1, nil
}
