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
	// settleBudget 是「结论已经定了、必须写回 outbox」那一步的独立预算。它从 OpBudget
	// 里切走(投递只拿得到 OpBudget-settleBudget),所以单行处理总时长仍 <= OpBudget,
	// leaseHeadroom 的推导不受影响。
	//
	// 为什么非切不可:同步路径的父 ctx 预算恰好就是 OpBudget(帮会同步捐献 2500ms)。
	// 投递一旦跑满预算,落库这一步拿到的必然是已经过期的 ctx —— 此时 scene 那边钱已经
	// 扣了,outbox 行却更新不了,下一轮重投又扣一遍。Finalize 的 CAS 只能保证对侧账不做
	// 第二遍,保不了 scene 不被再投一次。钱的路径按 AGENTS §11.3 取 fail-closed:
	// 宁可少给投递一点时间,也不能让「终局已定」这件事落不了盘。
	//
	// 700ms 的来历:够 Finalize / Reschedule 那一条 CAS(含 WithTxRetry 的两次尝试)落盘。
	//
	// **剩下的 1800ms 装不下最坏情况的投递,这是清醒的取舍,不是算漏了。** 完整路径不是
	// 「800ms + 三次 sleep 100/200/400」那 1500ms —— 每一轮重查除了 sleep,自己还要再发一次
	// RPC(各自上限 CallTimeout 800ms)。最坏是 800 +(100+800)+(200+800)+(400+800)= 3900ms。
	// 所以 1800ms 的实际含义是:**快路径三轮重查全过,慢路径约只容得下一轮**,之后 requery
	// 提前返回非 durable 结果,Decide 走 AwaitDurable、500ms 后重排。钱是安全的(结局没丢,
	// 只是这一轮没拿到 durable 确认),代价是上线后
	// assetop_requery_total{result="timeout"} 与 reschedule_total{reason="await_durable"}
	// 会比切预算之前高。调小 OpBudget 的调用方要自己核这笔账(validate 只保证它 > 本值)。
	settleBudget = 700 * time.Millisecond
)

// ErrPoisonRow 表示这一行的 payload 解不开(毒行)。
// Store 负责把它推迟到很久以后,循环跳过它继续下一行 —— 一行坏数据不该让整批停摆。
var ErrPoisonRow = errors.New("assetop: op payload undecodable")

// ErrLeaseLost 表示 Reschedule 的 CAS 落空(RowsAffected==0):租约在处理期间被另一个
// 副本接管,本次算出来的结果一个字也没写回去。它不是存储故障,循环不重试;但它必须有名字,
// 否则「我的结果被丢弃」在任何地方都看不见(契约见 Store.Reschedule)。
var ErrLeaseLost = errors.New("assetop: lease taken over by another replica")

// FreshAttemptLimit 是 ListDue 第一段「新行」的判据:attempts < FreshAttemptLimit。
// 防饿死的两段读为什么必须这么分,见 Store.ListDue 的契约。
//
// 它放在包里而不是 LoopConfig 里,是因为用它的是 Store 的 SQL,循环既不读也不传;
// 而且它不在运维契约 Y-06 的配置项里,没有「按服务调」这回事。写成常量,实现直接引用,
// 就不会出现「循环一份、实现另抄一份」的两份真相(DRY,AGENTS §11.2)。
const FreshAttemptLimit uint32 = 3

// Store 是业务 outbox 表在本包眼里的样子。实现在业务方(帮会 B5 / 聚宝斋 P2)。
//
// 为什么领取分两步:早先的写法是 `UPDATE … WHERE status AND next_attempt_ms ORDER BY LIMIT`,
// 它在 MySQL 可重复读下按范围加 next-key 锁,会和业务事务里 AllocateSeq 的 FOR UPDATE +
// 插入新行互相等待;被选为死锁牺牲者的往往是**用户请求**。改成「非加锁读一批 id + 主键 CAS
// 逐行领取」之后,重投循环任何时刻只锁一行。
type Store interface {
	// ListDue 非加锁一致性读,走 (status, next_attempt_ms) 索引,只回主键。
	// 它是**两段**读,不是一条 SQL(规格 X-03「新行防饿死」):
	//
	//   第一段 —— 新行,优先占名额:
	//     SELECT op_id FROM {op}
	//      WHERE status=<pending> AND next_attempt_ms<=? AND lease_until_ms<?
	//            AND attempts < FreshAttemptLimit
	//      ORDER BY next_attempt_ms LIMIT ?        -- LIMIT = limit
	//
	//   第二段 —— 老行,只补第一段没用完的缺口(第一段已取满就**不发**这条 SQL):
	//     SELECT op_id FROM {op}
	//      WHERE status=<pending> AND next_attempt_ms<=? AND lease_until_ms<?
	//            AND attempts >= FreshAttemptLimit
	//      ORDER BY next_attempt_ms LIMIT ?        -- LIMIT = limit - 第一段条数
	//
	// 两段都不加锁(理由见本接口开头),按 op_id 去重后合并返回,总条数 <= limit。
	// 返回顺序不承诺:Tick 会把这批 id 打散给 worker,谁先谁后由调度决定。
	//
	// 为什么不能只写一条 SQL:单条 `ORDER BY next_attempt_ms LIMIT ?` 按到期时刻排序,
	// 而长期失败的老行退避到顶也只有 MaxBackoff(60s),它们永远「早就到期」,永远排在最前。
	// 积压一旦超过 Batch,每一轮名额就全被同一批老行吃光,刚提交的新指令一次也轮不上 ——
	// 玩家看到的是捐献半天没动静,而 assetop_* 指标上「一直在处理」,几乎不可能联想到根因。
	// 分两段是给新行留一条独立通道:老行只能拿走新行没用完的名额。
	//
	// 为什么要去重:两段的 attempts 条件互斥,正常不重叠;但它们是两次独立的非锁读,中间
	// 可能有别的副本 Reschedule 把 attempts 从 2 推到 3,同一行就会两段都出现。重复 id
	// 不会导致重复投递(第二次 Claim 必然 RowsAffected==0),但会白占一个名额。
	//
	// 判据出处:90-consistency.md 的 X-03 裁决(「先按 attempts < 3 取一批、不足再取老行」)。
	// 两段的排序都带 op_id 做次级键,让同毫秒到期的行在各副本上顺序一致,少抢同一行。
	//
	// **反向代价,已知并接受**:名额是新行绝对优先,老行没有保底。新行持续满额时,
	// 那批「钱已经在 scene 侧动过」的重试行可以很久排不上号 —— 典型形状是 scene 故障恢复期:
	// 故障期间的行都攒到 attempts>=3 成了老行,而恢复后新提交的指令首投失败仍算新行。
	// 这条按 X-03 接受,靠 assetop_pending_oldest_age_seconds 告警兜底;真要改成保底
	// (例如第一段只占 3/4 名额),得先改 X-03,不能只改实现。
	ListDue(ctx context.Context, nowMs uint64, limit int) ([]uint64, error)

	// Claim 单行主键 CAS 领取(autocommit),紧挨着处理前调用:
	//   UPDATE {op} SET lease_until_ms=?, lease_token=?
	//    WHERE op_id=? AND status=<pending> AND lease_until_ms<?
	// RowsAffected==0 → (Op{}, false, nil):行已被别的副本领走或已终结,跳过即可。
	// RowsAffected==1 → 回读整行并解 payload。
	// 解码失败时 Store 自己把行推迟并返回 (Op{}, false, ErrPoisonRow):
	//   UPDATE {op} SET last_outcome=0, next_attempt_ms=<poisonUntilMs>, lease_until_ms=0
	//    WHERE op_id=? AND lease_token=?
	//
	// poisonUntilMs 是**算好的绝对时刻**(循环按自己的时钟算 now+LoopConfig.PoisonDelay),
	// 和 nowMs / leaseUntilMs 是同一套路:时间全由循环拿主意,Store 只负责把给的数字写下去。
	// 实现里**不要**另写一个「毒行推迟多久」的常量 —— 那样运维契约 Y-06 的 PoisonDelayMs
	// 改了也不会生效,而且两份值分叉时没有任何报错(DRY)。
	Claim(ctx context.Context, opID, nowMs, leaseUntilMs, poisonUntilMs, token uint64) (Op, bool, error)

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
	// RowsAffected==1 → nil。
	// RowsAffected==0 → **必须**返回 ErrLeaseLost(可以 fmt.Errorf("%w: op_id=%d", …) 包一层,
	// 循环用 errors.Is 判)。0 行意味着租约在处理期间被另一个副本接管,本次的
	// next_attempt_ms / last_outcome / attempts 一个字都没落地。返回 nil 等于把
	// 「我的结果被丢弃」伪装成成功 —— AGENTS §11.3 不许静默降级;而租约被抢走通常意味着
	// Lease 配小了或某一行处理超时,是要查的。循环收到它记
	// assetop_reschedule_lost_total{stream},不记 store_errors,也不当作处理失败。
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

// validate 是人工终结的入参校验,放在结构上而不是某一个调用者里:管理 RPC 和
// assetopfix CLI 都要在写库之前做同一套检查,分两份写迟早只改一边。
//
// 长度上限跟列宽一致 —— 超长会被 MySQL 静默截断,而截掉的正是事后追责要看的那几个字。
func (r ManualResolution) validate() error {
	switch r.Final {
	case StatusApplied, StatusRejected, StatusAborted, StatusAppliedPartial:
	default:
		return fmt.Errorf("assetop: 人工终结状态非法(%s)", r.Final)
	}
	if r.Operator == "" || utf8.RuneCountInString(r.Operator) > 64 {
		return errors.New("assetop: 人工终结必须带操作人,且不超过 64 字")
	}
	if utf8.RuneCountInString(r.Reason) > 191 {
		return errors.New("assetop: 人工终结理由不得超过 191 字")
	}
	return nil
}

// ManualResolver 是业务方提供的人工终结通道(规格 §4.38)。
// 单事务:业务锁序加锁 → 改 status / resolved_by / resolve_reason,
// RowsAffected==1 才做对侧账(Applied 入账;Rejected / Aborted 退款;AppliedPartial 不动)。
//
// 入参(终局状态是否合法、操作人与理由的长度)由本包的 ResolveManually 在调用前校验,
// 实现不必也不该再写一遍;nowMs 由调用方的时钟给,实现不要自己取时间。
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
	// OpBudget 单行的处理时间预算,**含落库**:投递(RPC + durable 重查)拿到的是
	// OpBudget-settleBudget,余下的 settleBudget 专留给「终局已定之后写回 outbox」,
	// 理由见 settleBudget。所以必须 > settleBudget,否则投递没有时间可用。
	OpBudget time.Duration
	// BaseBackoff 退避基数;帮会取 GuildRule.asset_op_retry_base_ms。
	BaseBackoff time.Duration
	// MaxBackoff 退避封顶,同时也是 Alert 分支的重排间隔。
	MaxBackoff time.Duration
	// AwaitDurableDelay 「结局有了但没落盘」时的短延迟。
	AwaitDurableDelay time.Duration
	// PoisonDelay 是 payload 解不开的毒行推迟多久再看。循环在 Claim 之前按它算出
	// poison_until_ms 一起传给 Store(见 Store.Claim),所以它是**活的**配置项,
	// 不是给实现抄的参考值:运维契约 Y-06 的 PoisonDelayMs 改了就会生效。
	//
	// 取一小时:足够运维看到 assetop_store_errors_total{op="decode"} 再介入,
	// 又不至于让一行坏数据永远消失。
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
	// 预算要同时装下投递和落库。只够落库(甚至更少)的预算意味着投递的 ctx 一诞生就过期,
	// 循环会空转重试而不报错 —— 这种配置必须在启动时就拒掉。
	if c.OpBudget <= settleBudget {
		return fmt.Errorf("assetop: OpBudget(%v)必须大于落库预留 %v", c.OpBudget, settleBudget)
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
	// 毒行延迟为 0 等于「立刻再来一次」:一行解不开的 payload 会被每一轮 Tick 重新领取、
	// 重新解码失败,白占名额还刷满 decode 计数。配成 0 多半是漏配,不是有意。
	if c.PoisonDelay <= 0 {
		return fmt.Errorf("assetop: PoisonDelay 必须为正(当前 %v)", c.PoisonDelay)
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
	// Manual 可选:人工终结通道(规格 §4.38)。只服务于「进程里已经有 Loop」的调用方;
	// 不连 scene 的工具(assetopfix)直接用包级 ResolveManually,不要为它造一个假 Loop。
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
	// 毒行推到什么时候由这里算:时刻、随机源、超时都归循环管,Store 只写给它的数字。
	poisonUntilMs := nowMs + uint64(l.cfg.PoisonDelay/time.Millisecond)

	op, claimed, err := l.store.Claim(ctx, opID, nowMs, leaseUntilMs, poisonUntilMs, token)
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
// 那条路径的 ctx 预算是 2500ms(= OpBudget):投递分到 1800ms(CallTimeout 800ms +
// 重查 700ms 仍在内),落库另有 settleBudget 的 700ms,且不受父 ctx 过期/取消影响。
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

	// 投递只拿 OpBudget 的一部分,剩下的 settleBudget 是留给落库的,不能让 RPC 吃掉。
	applyCtx, cancel := context.WithTimeout(ctx, l.cfg.OpBudget-settleBudget)
	defer cancel()
	res, err := l.applier.Do(applyCtx, rpc, op.Request())

	// 没发出去(玩家不在线)且已经试过几次:去读已落盘账本,已见的结局可以直接终结。
	// 它走 applyCtx 而不是父 ctx:读账本同样是「定结论」的一步,不该去啃落库的预留;
	// 而且 res.Local 意味着 Do 没发出任何网络请求,投递预算几乎没动过,够它读一次。
	if err == nil && res.Local {
		res = l.tryPersistedLedger(applyCtx, op, res)
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

// settleContext 给「结论已定、必须写回 outbox」的那一步一段**不继承取消**的预算。
//
// 为什么要和父 ctx 脱钩:同步路径的父 ctx 预算就是 OpBudget,投递跑满之后它已经过期;
// 此时 scene 那边钱已经动了,再拿一个死 ctx 去写库必然失败 —— 终局只留在内存里,行还是
// PENDING,下一轮重投等于再投一次(Finalize 的 CAS 只保证不做第二遍对侧账,保不了
// scene 不被再投一次)。所以落库这一步宁可多花 settleBudget 也要写下去。
//
// 代价是关停时每一行最多多等 settleBudget,所以 WithoutCancel 之后必须自带超时:
// 少了这个超时,一个卡住的库就能把 Tick 和整个服务的关停永远钉在那里。
// WithoutCancel 只丢弃取消与截止,ctx 上的值(日志/追踪关联)仍然带着。
func settleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), settleBudget)
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

	settle, cancel := settleContext(ctx)
	defer cancel()
	finalized, err := l.store.Finalize(settle, op, status, res, nowMs)
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
//
// 它和 finalize 一样走 settleContext:重排本身不丢钱(行还在,租约到期后会被重领),
// 但父 ctx 一过期就连 attempts 都推不动,那一行会在原地被反复领取反复投递 —— 同一个
// seq 多投一次的代价由 scene 的只读答复兜着,不该靠它兜。
func (l *Loop) reschedule(ctx context.Context, op Op, nextAttemptMs uint64, res Result, nowMs uint64, reason string) (Processed, error) {
	settle, cancel := settleContext(ctx)
	defer cancel()
	if err := l.store.Reschedule(settle, op, nextAttemptMs, carryPartialReason(op, res), nowMs); err != nil {
		if errors.Is(err, ErrLeaseLost) {
			// 租约被别的副本接管:本次结果没写进去,但那一行已经有人管,既不是故障也无需重试。
			// 只有计数看得见它,所以一定要计(AGENTS §11.3 不许静默降级)。
			l.metrics.incRescheduleLost(op.Stream)
			logx.Infof("[AssetOp] 租约已被接管,本次重排落空 op_id=%d stream=%d seq=%d: %v",
				op.OpID, op.Stream, op.Seq, err)
			return Processed{Result: res}, nil
		}
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

// ResolveManually 走人工终结通道。它不查证据、不猜结论:调用方(管理 RPC / assetopfix
// CLI)已经按 scene 流水 correlation_id 判定过,这里只负责校验 + 落库 + 留痕 + 计数。
//
// 为什么是包级函数而不是 Loop 的方法:D4 拍板的 assetopfix CLI 只连库,不连 scene,
// 给不出 Applier,也没有循环节律可言。挂在 Loop 上就等于逼它先造一个假 Applier 和一份
// 能过 validate 的假 LoopConfig,才能改一行数据 —— 那两样东西一旦被造出来,下一个人
// 很容易拿它去真的跑循环。人工终结真正需要的只有 ManualResolver 和一个时钟。
//
// resolver 为 nil 时报错而不是当无事发生:调用方以为自己终结了一行、实际什么也没做,
// 是钱路径上最坏的一种「成功」。
//
// m 可为 nil(不接指标,CLI 就是这样);now 为 nil 时取 time.Now。
//
// 人工终结会让 next_seq 与 scene 的 max_seq 拉开距离,累计超过 511 之后新 seq 会被
// 判 kJumpTooFar —— 那是 fail-closed 的预期行为,按运维手册抬纪元即可。
func ResolveManually(ctx context.Context, resolver ManualResolver, r ManualResolution, m *Metrics, now func() time.Time) (bool, error) {
	if resolver == nil {
		return false, errors.New("assetop: 未配置人工终结通道")
	}
	if err := r.validate(); err != nil {
		return false, err
	}
	if now == nil {
		now = time.Now
	}

	resolved, err := resolver.ResolveManually(ctx, r, uint64(now().UnixMilli()))
	if err != nil {
		m.incStoreError("manual_resolve")
		return false, fmt.Errorf("assetop: 人工终结失败 op_id=%d: %w", r.OpID, err)
	}
	if resolved {
		m.incManualResolve(r.Final)
		logx.Infof("[AssetOp] manual op_id=%d final=%s operator=%s reason=%s",
			r.OpID, r.Final, r.Operator, r.Reason)
	}
	return resolved, nil
}

// ResolveManually 是包级同名函数的薄委托:服务进程里 Loop 已经拿着人工通道、指标和时钟,
// 管理 RPC 不必再传一遍。行为与包级函数完全一致。
func (l *Loop) ResolveManually(ctx context.Context, r ManualResolution) (bool, error) {
	return ResolveManually(ctx, l.Manual, r, l.metrics, l.now)
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
