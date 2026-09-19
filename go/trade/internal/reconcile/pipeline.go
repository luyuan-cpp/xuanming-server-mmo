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
package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"trade/internal/data"

	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	tradepb "proto/trade"

	"shared/assetop"
	"shared/safego"

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

	// CallTimeout 是**单次**资产 RPC 的上限,装配层(internal/svc)建 assetop.Caller 时用它。
	// 与 guild 同值:一次 800ms + 重查 100/200/400ms 仍在 OpBudget(2500ms)内。
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
	// Ops 是 outbox / seq 的存储实现,同时是 assetop.Store。
	Ops *data.AssetOpRepo
	// Caller 一次投递 + durable 重查;必须已带 Signer,否则本包拒绝构造。
	Caller *assetop.Caller
	// OpIDs 发 op_id 的号段客户端(biz_tag = trade_asset_op)。没有回退:发不出号就不受理上架。
	OpIDs IDSource
	// Metrics assetop 的低基数指标;可为 nil。
	Metrics *assetop.Metrics
	// Now 注入时钟,单测可控;nil 时用 time.Now。
	Now func() time.Time
}

// IDSource 是 op_id 的来源(shared/idsegment.Client 满足它)。抽成接口只为单测能注入假发号器。
type IDSource interface {
	Next(ctx context.Context) (uint64, error)
}

// Pipeline 是本包的门面:入队(生产者)与重投循环(消费者)。
type Pipeline struct {
	ops     *data.AssetOpRepo
	loop    *assetop.Loop
	ids     IDSource
	now     func() time.Time
	metrics *assetop.Metrics
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
	loop, err := assetop.NewLoop(assetop.LoopConfig{
		Interval:              loopInterval,
		Batch:                 loopBatch,
		Workers:               loopWorkers,
		Lease:                 loopLease,
		OpBudget:              loopOpBudget,
		BaseBackoff:           loopBaseBackoff,
		MaxBackoff:            loopMaxBackoff,
		AwaitDurableDelay:     loopAwaitDurableDelay,
		PoisonDelay:           data.PoisonDelay,
		LedgerReadMinAttempts: loopLedgerMinAttempts,
		// 只报 trade 独占的两条流(不变量 I6):循环每 30s 按它们刷
		// assetop_pending_oldest_age_seconds。漏写 = 积压告警永远没有序列。
		Streams: TradeStreams(),
	}, d.Ops, d.Caller, d.Metrics, now)
	if err != nil {
		return nil, fmt.Errorf("trade: 资产重投循环参数非法: %w", err)
	}
	// 人工终结通道(§4.38):UNKNOWN 每 60s 重排、玩家长期离线的行不会自己走到终局。
	// 不挂它的话 Loop.ResolveManually 只会回"未配置人工终结通道",卡死的行无路可走。
	// Manual 是 Loop 的可选字段(不在 LoopConfig 里),必须在 Run / ProcessOne 之前挂上。
	loop.Manual = d.Ops
	return &Pipeline{ops: d.Ops, loop: loop, ids: d.OpIDs, now: now, metrics: d.Metrics}, nil
}

// Start 起后台重投循环。ctx 取消即退出;调用方负责在停机收尾里取消它。
//
// 空接收者直接返回:密钥缺失时 svc 把 Pipeline 留成 nil,启动路径照常调到这里。
func (p *Pipeline) Start(ctx context.Context) {
	if p == nil {
		logx.Error("[trade] 资产通道未启用(密钥缺失),不起重投循环")
		return
	}
	safego.Go("trade.assetop.loop", func() { p.loop.Run(ctx) })
	logx.Infof("[trade] 资产重投循环已启动: interval=%v batch=%d workers=%d lease=%v",
		loopInterval, loopBatch, loopWorkers, loopLease)
}

// EnqueueEscrowDebit 写入一行"上架托管"的 outbox 并立刻投一次。
//
// 步骤(顺序固定):
//  1. 号段发 op_id —— 发不出就当场失败,绝不自造 id;
//  2. **事务外**建 seq 行(并发首次建行在事务内会死锁,§4.19);
//  3. 一个事务里:分配 seq(FOR UPDATE + I5 未决数守卫)→ 插 outbox 行。P3 的商品状态迁移
//     必须并进这同一个事务,否则会出现"扣了但商品没进 ESCROWING";
//  4. 提交后同步投一次(复用重投循环的同一个 ProcessOne,不另写一条路径)。
//
// 第 4 步失败不回滚第 3 步:行已在 outbox,循环会接着投。只有 1–3 步失败才算本次上架失败。
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
	nowMs := p.nowMs()
	if err := p.ops.EnsureSeqRow(ctx, req.SellerPlayerID, stream, nowMs); err != nil {
		return EscrowResult{}, err
	}

	var alloc assetop.Alloc
	txErr := assetop.WithTxRetry(ctx, p.ops.DB(), 2, data.IsRetryableTxError, func(tx *sql.Tx) error {
		// 每次重试都重新分配:上一次尝试的 seq 随事务一起回滚了,复用会写出空洞。
		a, err := assetop.AllocateSeq(ctx, tx, p.ops.Tables(), req.SellerPlayerID, stream, assetop.DefaultLimits, p.nowMs())
		if err != nil {
			return err
		}
		alloc = a
		return p.ops.InsertOp(ctx, tx, p.newEscrowRecord(req, opID, a, payload))
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
		logx.Errorf("[trade] 托管首次投递未完成,交给重投循环: op_id=%d listing=%d player=%d seq=%d: %v",
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
//     currency_type 的上界(C++ kCurrencyMax)Go 侧没有事实源,留给 scene 判。
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
