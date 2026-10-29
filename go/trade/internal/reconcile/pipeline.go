// Package reconcile 是聚宝斋的资产指令管线:把"要对某个玩家做的资产改动"写进 outbox
// (trade_asset_op),再由重投循环反复投递到玩家当前所在的 scene,直到拿到**已落盘的终局**。
//
// 协议与不变量的唯一权威是 docs/design/guild-phase2/04-asset-channel.md §S4;本包只做接线:
// seq 分配、outbox 写入、提交后的第一次同步投递、以及后台重投循环的启停。
// 跨进程的正确性全部由 scene 侧的 seq 账本保证(同 seq 重复投递只会读到同一个结局),
// 所以多副本同时跑循环是安全的,行级租约只为省流量。
//
// 本批(聚宝斋 P2)只落**上架托管**这一条生产者路径;交付 / 回退(TRADE_CREDIT 流)与订单、
// 支付属于 P3,位置在 data.AssetOpRepo.Finalize 的对侧账注释处。
//
// 落地恢复(docs/design/landing-recovery-sync-digest.md §5.3、§6)与 guild 同步补齐:离线读已落盘账本
// (Deps.Ledger)、落地唤醒(WakePlayer,由 svc 的 assetopwake 订阅调用)、离线停放档、日对账与自动放弃
// (RunMaintenance,由 svc 在 leader 选主下运行)。参数见 Recovery。
package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"shared/drainproof"
	"time"

	"trade/internal/data"

	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	tradepb "proto/trade"

	"shared/assetop"
	"shared/currencytype"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 重投循环的参数。全部取 §4.37 给的值;它们与 scene 侧 1024 的 seq 窗口、租约、预算是一条
// 正确性证明,不是可调业务数值,所以写在代码里而不是配置里(改之前先改证明)。
const (
	loopInterval          = 2 * time.Second
	loopBatch             = 100
	loopWorkers           = 8
	loopLease             = 10 * time.Second
	loopOpBudget          = 2500 * time.Millisecond
	loopBaseBackoff       = time.Second
	loopMaxBackoff        = 60 * time.Second
	loopAwaitDurableDelay = 500 * time.Millisecond
	loopLedgerMinAttempts = 3

	// 毒行(payload 解不开)的推迟时长。**唯一事实源就是这里**:循环据它算出绝对时刻,
	// 经 Store.Claim 的 poisonUntilMs 传给 data 层,data 层不许再写一份常量。
	loopPoisonDelay = time.Hour

	// CallTimeout 是**单次**资产 RPC 的上限,装配层(internal/svc)建 assetop.Caller 时用它。
	// 与 guild 同值。
	//
	// 注意**投递拿不到整个 OpBudget**:assetop 从 OpBudget 里切走 settleBudget 留给落库
	// (否则 RPC 跑满预算后 Finalize 必定拿到已过期的 ctx —— scene 已经扣了钱、outbox 行
	// 却更新不了)。所以投递侧实际是 1800ms:够快路径跑完"一次 800ms + 重查 100/200/400ms",
	// 慢路径(每轮重查自己还要再发一次 RPC)约只容得下一轮,之后转 AwaitDurable 重排。
	// 放在本包而不是 svc:它与下面这组循环参数是同一条预算,改一个要连着看另一个。
	CallTimeout = 800 * time.Millisecond

	// commitDeliveryDelay:新行的 next_attempt_ms / lease_until_ms 相对提交时刻的偏移。
	// 提交后业务线程会**立刻**自己投一次;让重投循环在这段时间内看不到这一行,两边就不会
	// 抢同一行(§4.19 seq.go 末段)。与 loopLease 同值。
	commitDeliveryDelay = loopLease
)

// TradeStreams 返回 trade 独占的两条资产流(不变量 I6:每条流只有一个服务分配 seq)。
// 每次返回新切片,调用方改了也影响不到别人。
func TradeStreams() []assetpb.AssetOpStream {
	return []assetpb.AssetOpStream{
		assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT,
		assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT,
	}
}

// EscrowRequest 是一次"上架托管"(把资产从卖家身上扣出)的入参。
//
// Bundle 在 v1 **只支持游戏币:currencies 恰一条、items 为空**(§4.9 第 8 步的 Debit 约束)。
//
// `AssetBundle.item_uuids`(按 guid 扣装备)与 `pet_id`(扣宝宝)虽然已经在 proto 里,
// 但**还没进签名串** —— §4.32 的 canonical 第 10 行只写 `c=…;i=…`。两边都没做完之前
// 本包一律拒收带这两个字段的包,理由见 validateDebitBundle。
//
// 角色交易(CHARACTER_LOCK)不走这条路,见聚宝斋设计 §6.3。
type EscrowRequest struct {
	// SellerPlayerID 资产的主人。
	SellerPlayerID uint64
	// ListingID 商品 id,只作业务外键进 outbox 的 ref_id。
	// 请求的 correlation_id 取 op_id(§4.38 的人工终结以它定位唯一一行)。
	ListingID uint64
	// Bundle 要扣出的那一份资产。
	Bundle *assetpb.AssetBundle
	// DeadlineMs 托管的业务截止时刻(Unix 毫秒);到点后重投循环改发 AssetAbortDebit,
	// 确定性收口成"没扣成"。0 = 永不中止(不建议用于托管:卖家离线时商品会一直挂在 ESCROWING)。
	DeadlineMs uint64
}

// EscrowResult 是一次托管入队 + 首次投递的结果。
//
// Finalized=false **不是失败**:行已经在 outbox 里,重投循环会继续投到拿到终局为止。
// 调用方(P3 的 CreateListing)据此把商品留在 ESCROWING 并告诉玩家"处理中"。
type EscrowResult struct {
	OpID      uint64
	Seq       uint64
	Epoch     uint64
	Finalized bool
	Status    assetop.Status
	Result    assetop.Result
}

// Deps 是本包的显式依赖。全部由调用方装配(AGENTS §11.2 显式依赖)。
type Deps struct {
	// Ops 是 outbox / seq 的存储实现,同时是 assetop.Store(以及落地恢复的三个可选能力)。
	Ops *data.AssetOpRepo
	// Caller 一次投递 + durable 重查;必须已带 Signer,否则本包拒绝构造。
	Caller *assetop.Caller
	// OpIDs 发 op_id 的号段客户端(biz_tag = trade_asset_op)。没有回退:发不出号就不受理上架。
	OpIDs IDSource
	// Metrics assetop 的低基数指标;可为 nil。
	Metrics *assetop.Metrics
	// Now 注入时钟,单测可控;nil 时用 time.Now。
	Now func() time.Time
	// Ledger 读玩家已落盘的资产账本(生产是 assetop.DataServiceLedger)。RequireSQLCommit 关闭时可选:挂上之后,
	// 长期离线且已记账的行可以提前终结;它也是自动放弃"账本确定未见"那条证据的唯一来源 —— 没有它自动放弃不运行
	// (fail-closed)。RequireSQLCommit 打开时必填且必须能读 SQL 提交证据,否则 New 拒绝构造(Loop.CheckWiring)。
	Ledger assetop.LedgerReader
	// RequireSQLCommit 是资产 SQL 提交证据开关(config.AssetOpConf.RequireSQLCommit,原样进 assetop.LoopConfig;
	// docs/design/db-write-fencing-mysql.md §6.6)。零值 = HEAD 口径。
	RequireSQLCommit bool
	// Locator 可选:自动放弃前重新解析玩家位置(生产与 Caller.Resolver 是同一个 scenenode.Locator)。
	// 为 nil 时自动放弃不运行(fail-closed)。
	Locator assetop.Resolver
	// Recovery 是落地恢复的循环参数(停放档 / 自动放弃 / 日对账);零值 = 全部关闭。
	Recovery Recovery
}

// Recovery 是落地恢复(docs/design/landing-recovery-sync-digest.md §6)在重投循环上的参数,
// 一一对应 assetop.LoopConfig 的同名字段,零值语义也相同(0 = 关闭)。默认值的唯一事实源是
// assetop.DefaultLoopConfig();trade 的配置项(config.AssetOpConf)与 guild 同名同默认,换算在 svc。
//
// 与本文件那组循环常量不同,这几项是**运维可调**的业务节律(停多久、放弃门槛、对账频率),不参与
// "seq 窗口 / 租约 / 预算"那条正确性证明;它们各自的硬边界由 assetop.NewLoop 校验(放弃门槛 ≥ 30 天、
// 安全间隔 ≥ max(10 分钟, Lease) 等),配错即拒启。
type Recovery struct {
	// OfflineAfterAttempts 连续投递几次之后,"玩家确实没有位置"的行进入停放档。0 = 关闭停放档。
	OfflineAfterAttempts uint32
	// OfflineBackoff 是停放档的间隔中值(±20% 抖动)。
	OfflineBackoff time.Duration
	// AbandonAfter 是自动放弃的年龄门槛。0 = 关闭自动放弃。
	AbandonAfter time.Duration
	// AbandonSafetyGap 是距最后一次可能的发送必须超过的安全间隔。
	AbandonSafetyGap time.Duration
	// AbandonBatch 是一轮放弃最多处理多少行。
	AbandonBatch int
	// AbandonInterval 是两轮放弃之间的间隔。
	AbandonInterval time.Duration
	// AuditInterval 是两轮日对账之间的间隔。0 = 不跑对账。
	AuditInterval time.Duration
}

// maintenanceEnabled 报告 RunMaintenance 有没有活可干(对账或放弃至少开了一个)。
func (r Recovery) maintenanceEnabled() bool {
	return r.AuditInterval > 0 || r.AbandonAfter > 0
}

// IDSource 是 op_id 的来源(shared/idsegment.Client 满足它)。抽成接口只为单测能注入假发号器。
type IDSource interface {
	Next(ctx context.Context) (uint64, error)
}

// Pipeline 是本包的门面:入队(生产者)与重投循环(消费者),外加落地恢复的两个入口
// (WakePlayer 给唤醒订阅、RunMaintenance 给选主后的维护任务)。
type Pipeline struct {
	ops      *data.AssetOpRepo
	loop     *assetop.Loop
	ids      IDSource
	now      func() time.Time
	metrics  *assetop.Metrics
	recovery Recovery
}

// ErrSignerMissing:没有配 MMORPG_ASSET_OP_SECRET_TRADE(或密钥太短)。
// 资产路径一律 fail-closed,不做 dev 放行(§4.32),所以整条管线直接不构造。
//
// 它同时是**空管线**的错误:svc 在密钥缺失时把 Pipeline 留成 nil,业务侧拿着这个 nil
// 调进来时返回它,而不是 panic —— 降级形态必须以错误的形式被看见。
var ErrSignerMissing = errors.New("trade: 资产通道未配置调用方密钥,管线不启动")

// New 构造管线。Caller 为 nil(密钥缺失时 svc 不会构造它)或它没带 Signer,都返回 ErrSignerMissing。
//
// 为什么连"Caller 非 nil 但 Signer 为 nil"也要拒:assetop.Caller.Do 在那种情况下每次都回
// ErrNoSigner,decide.go 把它判成 ActionRetry,于是每一行都按退避无限重投、永不终结。
// 结果是一条"收得下托管、写得进 outbox、就是投不出去"的管线,比当场拒绝难查得多。
func New(d Deps) (*Pipeline, error) {
	if d.Ops == nil {
		return nil, errors.New("trade: 资产管线缺 outbox 存储")
	}
	if d.Caller == nil || d.Caller.Signer == nil {
		return nil, ErrSignerMissing
	}
	if d.OpIDs == nil {
		return nil, errors.New("trade: 资产管线缺 op_id 号段客户端")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	cfg := loopConfig(d.Recovery)
	cfg.RequireSQLCommit = d.RequireSQLCommit
	loop, err := assetop.NewLoop(cfg, d.Ops, d.Caller, d.Metrics, now)
	if err != nil {
		return nil, fmt.Errorf("trade: 资产重投循环参数非法: %w", err)
	}
	// 人工终结通道(§4.38):UNKNOWN 每 60s 重排、玩家长期离线的行不会自己走到终局。
	// 不挂它的话 Loop.ResolveManually 只会回"未配置人工终结通道",卡死的行无路可走。
	// Manual 是 Loop 的可选字段(不在 LoopConfig 里),必须在 Run / ProcessOne 之前挂上。
	loop.Manual = d.Ops
	// Ledger / Locator 同为 Loop 的无锁可选字段,同样只能在这里(任何 goroutine 读它们之前)挂上。
	// 只在非 nil 时赋值:把一个装着 nil 指针的接口挂上去,"功能没装配齐"的判断就会被骗过。
	if d.Ledger != nil {
		loop.Ledger = d.Ledger
	}
	if d.Locator != nil {
		loop.Locator = d.Locator
	}
	// 开关打开却没有能读 SQL 证据的 Ledger:每笔终结都只能停在 AwaitDurable,必须在构造时拒绝。
	if err := loop.CheckWiring(); err != nil {
		return nil, fmt.Errorf("trade: %w", err)
	}
	return &Pipeline{ops: d.Ops, loop: loop, ids: d.OpIDs, now: now, metrics: d.Metrics, recovery: d.Recovery}, nil
}

// loopConfig 拼重投循环的完整参数:正确性证明那一组取本文件的常量,落地恢复那一组取 Recovery。
func loopConfig(r Recovery) assetop.LoopConfig {
	return assetop.LoopConfig{
		Interval:              loopInterval,
		Batch:                 loopBatch,
		Workers:               loopWorkers,
		Lease:                 loopLease,
		OpBudget:              loopOpBudget,
		BaseBackoff:           loopBaseBackoff,
		MaxBackoff:            loopMaxBackoff,
		AwaitDurableDelay:     loopAwaitDurableDelay,
		PoisonDelay:           loopPoisonDelay,
		LedgerReadMinAttempts: loopLedgerMinAttempts,
		// 只报 trade 独占的两条流(不变量 I6):循环每 30s 按它们刷
		// assetop_pending_oldest_age_seconds,日对账按它们给每一类置值。漏写 = 积压告警永远没有序列。
		Streams: TradeStreams(),

		OfflineAfterAttempts: r.OfflineAfterAttempts,
		OfflineBackoff:       r.OfflineBackoff,
		AbandonAfter:         r.AbandonAfter,
		AbandonSafetyGap:     r.AbandonSafetyGap,
		AbandonBatch:         r.AbandonBatch,
		AbandonInterval:      r.AbandonInterval,
		AuditInterval:        r.AuditInterval,
	}
}

// Run **阻塞**运行后台重投循环,直到 ctx 结束才返回。调用方(svc.AssetChannel.Start)用 safego.Go 起它,
// 并在 WaitGroup 里记下这个 goroutine:停机时必须等 Run 返回之后才能关 scene 连接缓存、共享 Redis 与 MySQL。
//
// 为什么必须等(与 guild 的 AssetPipeline.Stop 同一理由):assetop.Loop.Run 在 ctx 取消后要等当前这一轮 Tick 的 worker
// 全部结束才返回,而每一行落库那一步用的是不继承取消的 settleContext(assetop)—— 取消 ctx 拦不住它。不等就去关连接,
// 终局写不回 outbox,行留在 PENDING、下一轮再投一次:钱不丢(scene 对同一 seq 只读答复,不变量 I2),但 Finalize 报错、白投一轮。
// 所以本方法不自己起 goroutine:"起"与"等"都归装配层,两者才对得上同一个 WaitGroup。
//
// 空接收者打 ERROR 后立刻返回:密钥缺失时 svc 把 Pipeline 留成 nil,启动路径照常调到这里。
func (p *Pipeline) Run(ctx context.Context) {
	if p == nil {
		logx.Error("[trade] Asset channel disabled (missing key); redelivery loop not started")
		return
	}
	logx.Infof("[trade] Asset redelivery loop started: interval=%v batch=%d workers=%d lease=%v "+
		"offline_after_attempts=%d offline_backoff=%v abandon_after=%v audit_interval=%v ledger=%t require_sql_commit=%t",
		loopInterval, loopBatch, loopWorkers, loopLease,
		p.recovery.OfflineAfterAttempts, p.recovery.OfflineBackoff, p.recovery.AbandonAfter, p.recovery.AuditInterval,
		p.loop.Ledger != nil, p.loop.RequireSQLCommit())
	p.loop.Run(ctx)
	logx.Info("[trade] Asset redelivery loop stopped")
}

// WakePlayer 立即重投某个玩家的全部 PENDING 行(落地唤醒,docs/design/landing-recovery-sync-digest.md §5.3),
// 返回实际领到并处理的行数。它满足 shared/assetopwake.Waker,由 svc 的唤醒订阅调用。
//
// 每一行走的是与退避重投完全相同的 Claim CAS + ProcessOne(assetop.Loop.WakePlayer),并发计入同一份
// Workers 预算 —— 唤醒只加速,不引入任何新的状态迁移(设计 L-4)。ctx 是这次唤醒的总预算。
// 空接收者回 ErrSignerMissing(与 EnqueueEscrowDebit 同一口径),不 panic。
func (p *Pipeline) WakePlayer(ctx context.Context, playerID uint64) (int, error) {
	if p == nil {
		return 0, ErrSignerMissing
	}
	return p.loop.WakePlayer(ctx, playerID)
}

// MaintenanceEnabled 报告日对账与自动放弃是否至少开了一个。都关着时装配层不必为 RunMaintenance 去竞选领导权。
func (p *Pipeline) MaintenanceEnabled() bool {
	return p != nil && p.recovery.maintenanceEnabled()
}

// RunMaintenance 阻塞运行日对账(AuditOnce)与自动放弃(AbandonOnce),直到 ctx 结束。
//
// **只该在一个副本上跑**:svc 用 shared/leader 选主,当选时以领导权的 ctx 调用、卸任时取消
// (设计 §6.2;多副本同时跑不会出错,只是按类 gauge 有多份、候选扫描重复)。
// 自动放弃另需 Deps.Ledger 与 Deps.Locator 都挂上,缺一个 assetop 只打一次 INFO、不运行(fail-closed)。
// 空接收者直接返回。
func (p *Pipeline) RunMaintenance(ctx context.Context) {
	if p == nil {
		return
	}
	p.loop.RunMaintenance(ctx)
}

// EnqueueEscrowDebit 写入一行"上架托管"的 outbox 并立刻投一次。
//
// 步骤(顺序固定):
//  1. 号段发 op_id —— 发不出就当场失败,绝不自造 id;
//  2. **建 seq 行的短事务(全序里的"事务 A")**:EnsureSeqRows 自己开一个 RC 事务,
//     普通读探针 →(仅缺行时)锁哨兵守卫行 → 按 (player_id, stream) 升序 INSERT IGNORE → **提交**。
//     稳态下探针全命中,它连事务都不开;
//  3. **业务事务("事务 B")**:AllocateSeqsTx(普通读确认行在 → 按升序逐个 FOR UPDATE 分配 seq,
//     含 I5 未决数守卫)→ 插 outbox 行。P3 的商品状态迁移必须并进这同一个事务(否则会出现
//     "扣了但商品没进 ESCROWING"),且商品 / 订单行的锁要排在 AllocateSeqsTx **之前**;
//  4. 提交后同步投一次(复用重投循环的同一个 ProcessOne,不另写一条路径)。
//
// 第 2 步为什么是独立的短事务(2026-09-29):把它并进业务事务的话,全局哨兵行的 X 要一直持到业务
// 事务提交 —— 中间还夹着分配 seq 与插 outbox 行。开服 / 新区首日几乎每一次上架都是首次建行,
// 那段窗口里全服托管入队全局串行。拆开之后持锁时间只剩一次点查 + 几条 INSERT IGNORE,而"消环"
// 的性质不受影响:它来自"所有首次建行者排在同一行已提交、永不删除的记录锁上",与是否同事务无关。
// 完整论证(含"短事务提交后 seq 行为什么不可能消失")见 data.AssetOpRepo 上方的守卫一节。
//
// 第 2 步失败 = 本次上架失败,**不降级成在业务事务里建行**:那会把"持业务锁再拿哨兵锁"的反向边
// 画回来。第 4 步失败不回滚第 3 步:行已在 outbox,循环会接着投。只有 1–3 步失败才算本次上架失败。
func (p *Pipeline) EnqueueEscrowDebit(ctx context.Context, req EscrowRequest) (EscrowResult, error) {
	// 空接收者 = 降级形态(密钥缺失,svc 没建管线)。以错误返回而不是 panic:
	// 调用方本来就要处理"托管失败",多一种 panic 只会让整个请求线程炸掉。
	if p == nil {
		return EscrowResult{}, ErrSignerMissing
	}
	if req.SellerPlayerID == 0 || req.ListingID == 0 {
		return EscrowResult{}, errors.New("trade: 托管入队缺 seller_player_id / listing_id")
	}
	if err := validateDebitBundle(req.Bundle); err != nil {
		return EscrowResult{}, err
	}
	payload, err := proto.Marshal(req.Bundle)
	if err != nil {
		return EscrowResult{}, fmt.Errorf("trade: 序列化托管资产包失败: %w", err)
	}

	opID, err := p.ids.Next(ctx)
	if err != nil {
		return EscrowResult{}, fmt.Errorf("trade: 发 op_id 失败: %w", err)
	}

	const stream = assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT
	sellerKey := data.SeqKey{PlayerID: req.SellerPlayerID, Stream: stream}

	// 事务 A:建行。本事务要用到的**全部** (player, stream) 一次传进去 —— 本批只有卖家一条 DEBIT 流,
	// P3 的交付会是买家 + 卖家两个 key,仍然一次传进来,由 data 层按升序处理。
	// 纪元取当前毫秒(建行时刻)。它一旦写下就不再变,所以这里不需要像旧写法那样"每次重试重新取":
	// 建行不再随业务事务回滚,重试的是业务事务,不是建行。
	if err := p.ops.EnsureSeqRows(ctx, p.nowMs(), sellerKey); err != nil {
		return EscrowResult{}, fmt.Errorf("trade: 建 seq 行失败(listing=%d player=%d): %w",
			req.ListingID, req.SellerPlayerID, err)
	}

	var alloc assetop.Alloc
	// 事务 B:业务写。它只对 seq 表做普通读确认 + 升序 FOR UPDATE,一行都不建,也不碰哨兵行。
	txErr := assetop.WithTxRetry(ctx, p.ops.DB(), 2, data.IsRetryableTxError, func(tx *sql.Tx) error {
		// 每次重试都重新分配:上一次尝试的 seq 随事务一起回滚了,复用会写出空洞。
		allocs, err := p.ops.AllocateSeqsTx(ctx, tx, p.nowMs(), sellerKey)
		if err != nil {
			return err
		}
		a, ok := allocs[sellerKey]
		if !ok || a.Epoch == 0 {
			// 不可达的防御:AllocateSeqsTx 给每个传进去的 key 都填结果,纪元为 0 时它自己就报错了。
			// 但 map 取值失手拿到零值是**静默**的,而零值会写出一行 seq=0 / stream_epoch=0 的 outbox,
			// scene 侧只会把它判成坏行并吃掉一次投递 —— 宁可在这里响亮地失败。
			return fmt.Errorf("trade: seq 分配结果缺 (player=%d stream=%d),不写 outbox",
				sellerKey.PlayerID, int32(sellerKey.Stream))
		}
		alloc = a
		return p.ops.InsertOp(ctx, tx, p.newEscrowRecord(req, opID, alloc, payload))
	})
	if txErr != nil {
		return EscrowResult{}, fmt.Errorf("trade: 托管入队失败(listing=%d player=%d): %w",
			req.ListingID, req.SellerPlayerID, txErr)
	}

	op := assetop.Op{
		OpID:        opID,
		PlayerID:    req.SellerPlayerID,
		Stream:      stream,
		Seq:         alloc.Seq,
		StreamEpoch: alloc.Epoch,
		// correlation_id 取 op_id,不是 listing_id:§4.38 的人工终结以 scene 流水的
		// correlation_id 定位唯一一行 outbox。取 listing_id 的话,同一件商品的托管与
		// 回退两条 op 会落成同一个 correlation_id,取证失效。ref_id 仍是 listing_id。
		// 必须与 data.AssetOpRepo.Claim 里的取法一致(重投换 correlation 同样不可取证)。
		CorrelationID: opID,
		TxType:        uint32(rollbackpb.TransactionType_TX_AUCTION_SELL),
		Bundle:        req.Bundle,
		DeadlineMs:    req.DeadlineMs,
		LeaseToken:    p.leaseTokenOf(opID),
	}
	out := EscrowResult{OpID: opID, Seq: alloc.Seq, Epoch: alloc.Epoch}
	processed, err := p.loop.ProcessOne(ctx, op)
	if err != nil {
		// 不算失败:行在 outbox 里,循环会接着投。只记日志,让调用方按"处理中"回给玩家。
		logx.Errorf("[trade] Initial escrow delivery incomplete; delegating to redelivery loop: op_id=%d listing=%d player=%d seq=%d: %v",
			opID, req.ListingID, req.SellerPlayerID, alloc.Seq, err)
		return out, nil
	}
	out.Finalized = processed.Finalized
	out.Status = processed.Status
	out.Result = processed.Result
	return out, nil
}

// validateDebitBundle 在**碰号段和数据库之前**把确定性非法的托管包挡回去。
//
// 两件不同的事,合在一处做:
//
//  1. §4.9 第 8 步的 Debit(v1) 形状:恰好 1 条货币、0 件物品、amount ∈ [1, INT64_MAX]。
//     这类包送到 scene 会被记成**终局 REJECTED**(§4.10 该行"记账=是")并吃掉一个 seq ——
//     一次注定失败的往返,还在账本里留下一条永远翻不了案的记录。本地判等价且免费。
//     currency_type 按 proto 同源取值表(shared/currencytype)判:写错的值必须在这里拒,
//     scene 才能把不认识的币种判成版本错位(回 RETRY 不记账),而不是对一个笔误永久重投。
//
//  2. `item_uuids` / `pet_id` **一律拒收**,因为 v1 的 scene 还没实现按 guid 扣装备 /
//     扣宝宝(那是 P3 托管的活)。这两个字段已经在 proto/common/asset/asset_op.proto 里
//     (字段 3 / 4),签名也已经覆盖到它们(2026-09-19 起 canonical 扩成
//     `…;i=…;u=<item_uuid,…>;p=<pet_id>`),所以**不再有安全理由**,只剩"送过去也做不了"。
//     送过去的后果是 scene 记一条终局 REJECTED 并吃掉一个 seq —— 一次注定失败的往返,
//     外加账本里一条永远翻不了案的记录。与上面第 1 条同理,本地判等价且免费。
//
// 解除条件:P3 在 scene 侧实现按 guid 扣物 / 扣宝宝(删掉 asset_op_system.cpp 的
// HasUnsupportedP2Fields 与它在两个 Validate*Bundle 里的调用),本函数这一条随之删除。
func validateDebitBundle(b *assetpb.AssetBundle) error {
	if b == nil {
		return errors.New("trade: 托管入队缺资产包")
	}
	if len(b.GetItemUuids()) > 0 || b.GetPetId() != 0 {
		return fmt.Errorf("trade: 托管资产包含 item_uuids(%d 个)/ pet_id(%d),"+
			"scene v1 尚未实现按 guid 扣物 / 扣宝宝(P3),送过去只会换回一条终局 REJECTED "+
			"并白吃一个 seq",
			len(b.GetItemUuids()), b.GetPetId())
	}
	if len(b.GetItems()) != 0 {
		return fmt.Errorf("trade: 托管资产包带了 %d 件可叠加物品,Debit v1 只收货币(§4.9 第 8 步)", len(b.GetItems()))
	}
	if len(b.GetCurrencies()) != 1 {
		return fmt.Errorf("trade: 托管资产包有 %d 条货币,Debit v1 恰收 1 条(§4.9 第 8 步)", len(b.GetCurrencies()))
	}
	if currency := b.GetCurrencies()[0].GetCurrencyType(); !currencytype.IsKnown(currency) {
		return fmt.Errorf("trade: 托管币种 %d 不在取值表内(proto/common/base/currency_type.proto)", currency)
	}
	amount := b.GetCurrencies()[0].GetAmount()
	if amount < 1 || amount > math.MaxInt64 {
		return fmt.Errorf("trade: 托管金额 %d 越界,须在 [1, %d](§4.9 第 8 步)", amount, int64(math.MaxInt64))
	}
	return nil
}

// ResolveManually 把一行卡死的 outbox 按人工判定落成终局(§4.38)。
//
// 调用方(管理入口)必须**先**按 scene 流水 `correlation_id = op_id` 判定过真实结局:
// 本方法不查证据、不猜结论。状态合法性与 operator / reason 的长度由 assetop 校验。
//
// 返回 false 表示这一行已经不在 PENDING(被循环抢先终结,或已被人工终结过),
// 不是错误,调用方不得重复入账。
func (p *Pipeline) ResolveManually(ctx context.Context, m assetop.ManualResolution) (bool, error) {
	if p == nil {
		return false, ErrSignerMissing
	}
	return p.loop.ResolveManually(ctx, m)
}

// newEscrowRecord 拼一行待办 outbox。
//
// lease_until_ms / next_attempt_ms 都取 now + commitDeliveryDelay:提交后业务线程立刻自己投,
// 这段时间里重投循环既领不到(租约未到期)也看不到(未到重投时刻)。
//
// last_send_ms 写插行时刻(LastSendMs 契约第 1 条):插行即持租约,提交后的同步投递紧接着就可能发出。
// data.AssetOpRepo.InsertOp 另有兜底(不小于 created_ms),这里显式写出只为让记录与落库的值一致。
func (p *Pipeline) newEscrowRecord(req EscrowRequest, opID uint64, alloc assetop.Alloc, payload []byte) *tradepb.TradeAssetOpRecord {
	nowMs := p.nowMs()
	dueMs := nowMs + uint64(commitDeliveryDelay/time.Millisecond)
	return &tradepb.TradeAssetOpRecord{
		OpId:          opID,
		PlayerId:      req.SellerPlayerID,
		Stream:        uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT),
		StreamEpoch:   alloc.Epoch,
		Seq:           alloc.Seq,
		Kind:          tradepb.TradeAssetOpKind_TRADE_ASSET_OP_KIND_ESCROW_DEBIT,
		Status:        tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING,
		Durable:       false,
		Attempts:      0,
		NextAttemptMs: dueMs,
		DeadlineMs:    req.DeadlineMs,
		LeaseUntilMs:  dueMs,
		LeaseToken:    p.leaseTokenOf(opID),
		TxType:        uint32(rollbackpb.TransactionType_TX_AUCTION_SELL),
		RefKind:       tradepb.TradeAssetOpRefKind_TRADE_ASSET_OP_REF_KIND_LISTING,
		RefId:         req.ListingID,
		Payload:       payload,
		CreatedMs:     nowMs,
		UpdatedMs:     nowMs,
		LastSendMs:    nowMs,
	}
}

// leaseTokenOf 是"提交令牌":插行与提交后那一次投递必须用同一个值,Reschedule 的
// CAS 才对得上。用 op_id 派生而不是随机数,免得在两处之间传一个额外的字段;
// op_id 来自号段、全局唯一,足够把本次提交与后续任何一次循环领取区分开。
func (p *Pipeline) leaseTokenOf(opID uint64) uint64 {
	// 高位打散,避免与循环里的随机令牌落在同一段数值区间时肉眼难分。
	token := opID ^ 0x7472_6164_6500_0000
	if token == 0 {
		// 0 与"没有租约"同形:Reschedule 的 CAS 会匹配上所有未被领取的行。
		// 只在 op_id 恰好等于那个掩码时发生,但代价只有一次比较。
		return 1
	}
	return token
}

func (p *Pipeline) nowMs() uint64 {
	ms := p.now().UnixMilli()
	if ms < 0 {
		return 0
	}
	return uint64(ms)
}

// 重投循环领取用的随机令牌由 shared/assetop 自己生成(Loop.claim → newLeaseToken),
// 本包不再提供第二份实现:两处各生成一份令牌只会让"这一行现在归谁"多一种说法。

// BindRelayAdmission 仅在 Start/同步业务入口之前装配，同一私有 Loop 覆盖重投、维护、唤醒和首次投递。
func (p *Pipeline) BindRelayAdmission(relay *drainproof.Session) error {
	if p == nil || p.loop == nil || relay == nil || p.loop.Relay != nil {
		return drainproof.ErrHold
	}
	p.loop.Relay = relay
	return nil
}
