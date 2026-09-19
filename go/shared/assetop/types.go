// Package assetop 实现「通用资产通道」的 Go 侧调用方原语。
//
// 帮会 / 聚宝斋这类 Go 服务要让**在线玩家身上**的货币与物品发生一次且仅一次的变化时:
// 先在自己的业务事务里写一行待办(outbox),并拿到「该玩家该流」的递增 seq 与流纪元;
// 再按 player:{id}:location 找到玩家所在 scene 节点,调 SceneNodeGrpc 的
// AssetDebit / AssetCredit / AssetAbortDebit;最后**只在**答复为
// (APPLIED | REJECTED) 且 durable=true 时才把这一行从 PENDING 改走。
//
// 类比:像银行柜台办业务要「流水号 + 回执」。流水号(stream_epoch + seq)由 Go 发;
// 柜台(scene)见过的流水号直接把原回执再给你一遍,绝不重办;回执要「盖了入库章」
// (durable,已写进玩家所在 zone 的 Redis)才算数。
//
// 权威规格:docs/design/guild-phase2/04-asset-channel.md
// §4.17–4.20(Go 侧本体与测试)、§4.29(流纪元)、§4.32(鉴权)、§4.36–4.38(第二轮修订)。
// 与该文档冲突时以该文档为准;本包注释只解释「为什么」,不复述条文。
//
// # 边界(信息隐藏)
//
//   - 本包只负责「怎么发、怎么判、怎么重投」,**不碰任何业务表**:落库的 SQL 形状由业务方
//     实现 Store,状态列的数值映射也在业务方(见 Status 的注释)。
//   - 不引 MySQL 驱动、不引 etcd / Redis 客户端:玩家定位交给 shared/scenenode,
//     事务重试的错误分类由调用方注入(seq.go 的 WithTxRetry)。
//   - 时间、随机数、RPC 目标全部显式注入,单测不依赖真实墙钟,也不依赖真实 scene。
//
// # 线程模型
//
// Caller 与 Loop 构造完成后即只读,可被多 goroutine 共享;Loop.Tick 内部的有界并发
// 与它为什么必须有界,见 reconcile.go 的 LoopConfig.Workers 注释。
//
// 全部代码未编译,待 Codex 验证(AGENTS §10.1)。
package assetop

import (
	"errors"
	"fmt"

	assetpb "proto/common/asset"

	"shared/generated/pb/table"
)

// Status 是一条资产指令在业务 outbox 表里的**语义**状态。
//
// 它刻意**不绑定数据库数值**:S1 的表定义取 1 基、S5 偏差 #1 提议 0 基,两节冲突待主设计
// 裁决(规格偏差 #23)。业务方的 Store 负责把这里的语义值映射成自己表里的列值,
// 分配 seq 时用到的「未决」数值经 SeqTables.PendingStatus 传入。
// 改这里的数值不会影响任何一张表,但会影响指标 label,改名前先搜 finalize_total。
type Status uint32

const (
	// StatusPending 待投递 / 重投中。只有它允许被 Finalize 或 Reschedule 改写。
	StatusPending Status = iota + 1
	// StatusApplied scene 已应用(全额)。
	StatusApplied
	// StatusRejected scene 判拒(余额不足、包非法、被封禁等),结局固定。
	StatusRejected
	// StatusAborted 中止占位成功:Abort 打在一个 scene 从未见过的 seq 上,
	// 从此该 seq 永远是 REJECTED(reason==0),对应的业务副作用要退还。
	StatusAborted
	// StatusAppliedPartial scene 应用了**一部分**。对侧账一律不做,转人工补偿
	// (规格 §4.33:错误不得伪装成功)。
	StatusAppliedPartial

	// statusCount 只用于「枚举与名字表不脱节」的编译期检查,不是合法状态。
	statusCount
)

// statusNames 是 Status 的平铺名字表,下标 0 空出来对应「零值不是合法状态」。
// 它同时是指标 label 的唯一出处(AGENTS §11.2:平铺常量表 + 编译期长度检查,
// 不用宏 / 代码生成)。
var statusNames = [...]string{"invalid", "pending", "applied", "rejected", "aborted", "applied_partial"}

// 编译期断言:名字表长度必须等于枚举项个数。不等时这行会报 index out of range,
// 等价于 C++ 侧的 static_assert(数组长度 == kCount)。
var _ = [1]struct{}{}[len(statusNames)-int(statusCount)]

// String 返回低基数的稳定名字;越界值回 "invalid",不 panic 也不泄漏原值。
func (s Status) String() string {
	if s >= statusCount {
		return statusNames[0]
	}
	return statusNames[s]
}

// RPC 是本通道的三个 scene 方法。它的 String() 同时是签名 canonical 串里的 <rpc> 字段,
// 所以**改名就是改协议**:C++ 侧 AssetOpCanonical 用的是同一组字面量。
type RPC uint8

const (
	// RPCDebit 扣除(AssetDebit)。
	RPCDebit RPC = iota + 1
	// RPCCredit 发放(AssetCredit)。
	RPCCredit
	// RPCAbort 中止占位(AssetAbortDebit):把一个未见过的 seq 永久钉成 REJECTED。
	// 允许用于任何流,包括 *_CREDIT(商店超时退帮贡要用),见规格偏差 #3。
	RPCAbort

	rpcCount
)

// rpcNames 与 canonical 串一字不差(规格 §4.32)。下标 0 空出来。
var rpcNames = [...]string{"invalid", "debit", "credit", "abort_debit"}

var _ = [1]struct{}{}[len(rpcNames)-int(rpcCount)]

// String 返回 canonical 串与指标 label 共用的名字。
func (r RPC) String() string {
	if r >= rpcCount {
		return rpcNames[0]
	}
	return rpcNames[r]
}

// ApplyRPCOf 给出某条流的「正常投递」方法:*_DEBIT 用 Debit,*_CREDIT 用 Credit。
// 第二个返回值为 false 表示流号非法(未指定或超出已知范围),调用方必须当成坏行处理,
// 不得猜一个默认值 —— 猜错方向等于把扣钱发成发钱。
//
// 注意 SYSTEM_CREDIT 也会返回 Credit:v1 里 scene 对它一律验签失败回 UNKNOWN
// (规格 §4.32 白名单),拒绝发生在 scene,不在这里静默吞掉。
func ApplyRPCOf(stream assetpb.AssetOpStream) (RPC, bool) {
	switch stream {
	case assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT,
		assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT:
		return RPCDebit, true
	case assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT,
		assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT,
		assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT:
		return RPCCredit, true
	default:
		return 0, false
	}
}

// Reason* 是 asset_error tip 段(27000 起)在 Go 侧的唯一引用点。
// 码由 data/tip/Tip.xlsx 导出(AGENTS §4.4),这里只取名字,不另建一份含义表。
const (
	ReasonCurrencyInsufficient = uint32(table.AssetError_kAssetCurrencyInsufficient)
	ReasonBagFull              = uint32(table.AssetError_kAssetBagFull)
	ReasonInBattle             = uint32(table.AssetError_kAssetInBattle)
	ReasonFrozen               = uint32(table.AssetError_kAssetFrozen)
	ReasonInvalidBundle        = uint32(table.AssetError_kAssetInvalidBundle)
	ReasonBlocked              = uint32(table.AssetError_kAssetBlocked)
	ReasonPlayerNotHere        = uint32(table.AssetError_kAssetPlayerNotHere)
	// ReasonPartialApplied 既会出现在本次答复里(Result.Partial),也可能是**上一次**
	// 答复留在行上的 last_reason。这两处不是同一份证据的两份拷贝,而是**接力**:
	//
	//   - scene 的只读答复只在 seq 仍留在账本的 partial_seqs 环里时才带 partial
	//     (规格 §4.9 第 4 步经 IsAssetOpPartial 查环)。那个环上限 64 条、超了删最小,
	//     watermark 右移时还会裁掉 seq <= 新 watermark 的条目(§4.4 Record 第 1、2 步)
	//     —— 所以它**会**丢,丢了之后重查只能得到一个普通的 APPLIED。
	//   - 环丢了之后,行上的 last_reason 是「这一笔曾经只发了一半」的唯一证据(§4.33)。
	//     漏掉它就是把半额发放当成全额成功入对侧账,那笔差额没有任何线索可追。
	//
	// 因此这个码在行上必须是**粘性**的。规格原话是「Reschedule 把 last_reason 写成
	// res.Reason,于是『曾见 partial』是粘性的」,但只靠这一句粘不住:传输失败、本地
	// NOT_HERE、坏流号重排带的都是 Result{}(Reason=0),一次就能抹掉。粘性由本包在
	// 写库前补齐,见 reconcile.go 的 carryPartialReason。FinalStatus 同时看两处,见 decide.go。
	ReasonPartialApplied = uint32(table.AssetError_kAssetPartialApplied)
	ReasonAuthFailed     = uint32(table.AssetError_kAssetAuthFailed)
)

// Result 是一次资产 RPC 的结果(已剥掉 gRPC 层)。
type Result struct {
	// Outcome 是 scene 给出的结局。Local==true 时它是本地合成的 NOT_HERE。
	Outcome assetpb.AssetOpOutcome
	// Reason 是 asset_error 段的 tip 码;无原因时为 0。
	Reason uint32
	// Durable 表示该 seq 的结局已经在「最后一次成功写入 Redis 的玩家数据」里。
	// 只有它为真,终结才是安全的(不变量 I4)。
	Durable bool
	// Partial 表示 APPLIED 但只发放了一部分。对侧账不能做,转人工补偿(规格 §4.33)。
	Partial bool
	// Local 表示这次根本没发出 RPC(玩家无位置 / 节点未注册),NOT_HERE 是本地合成的。
	// 离线已落盘结局读取(reconcile.go)只在 Local 时才有意义。
	Local bool
}

// Terminal 报告这条结果是否足以终结 outbox 行:结局固定 **且** 已落盘。
// 比契约 §3.3 更严 —— REJECTED 也要 durable,理由见规格第 2 部分 C9:
// 未落盘的拒绝可能被 scene 崩溃抹掉,之后同一个 seq 又被真的应用。
func (r Result) Terminal() bool {
	if !r.Durable {
		return false
	}
	return r.Outcome == assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED ||
		r.Outcome == assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED
}

// Op 是一行待办资产指令在内存里的形状,由业务方的 Store 从自己的表解出来。
// 本包只读它,不改它。
type Op struct {
	// OpID 是业务表主键,同时是 correlation_id 的常见取值(帮会 = op_id)。
	OpID uint64
	// PlayerID 是资产的主人。它是高基数值,**不得**进任何指标 label(AGENTS §11.3)。
	PlayerID uint64
	// Stream 决定用哪个 RPC、由哪个服务独占分配 seq(不变量 I6)。
	Stream assetpb.AssetOpStream
	// Seq 是本流内的递增流水号,从 1 起;0 非法。
	Seq uint64
	// StreamEpoch 是建 seq 行时写下的流纪元(毫秒)。库重建 / 恢复后它会变大,
	// 让旧 seq 不会被 scene 误认成同一笔业务(规格 §4.29)。
	StreamEpoch uint64
	// CorrelationID 进 scene 的 transaction_log.correlation_id,是人工对账的唯一线索。
	CorrelationID uint64
	// TxType 是 TransactionType 数值,scene 按流白名单校验。
	TxType uint32
	// Bundle 是要变动的资产。Caller 在签名前会克隆它,不会就地改。
	Bundle *assetpb.AssetBundle
	// Attempts 是已投递次数,决定退避时长与是否去读已落盘账本。
	Attempts uint32
	// DeadlineMs 到期后改发 Abort;0 表示永不中止。
	DeadlineMs uint64
	// LeaseToken 是本次领取的令牌,Reschedule 要带回去做 CAS,防止租约已被别人抢走后
	// 还把自己的结果写回去。
	LeaseToken uint64
	// LastReason 是上一次答复留下的 tip 码。它唯一的用途见 ReasonPartialApplied 的注释。
	LastReason uint32
}

// Request 把一行待办拼成 scene 请求。**不带 Auth**:签名由 Caller 在每次实际发包前
// 以当前时间现签(规格 §4.32),这样重查也不会因为时间窗过期而被拒。
func (o Op) Request() *assetpb.AssetOpRequest {
	return &assetpb.AssetOpRequest{
		PlayerId:      o.PlayerID,
		Stream:        o.Stream,
		Seq:           o.Seq,
		CorrelationId: o.CorrelationID,
		TxType:        o.TxType,
		Bundle:        o.Bundle,
		StreamEpoch:   o.StreamEpoch,
	}
}

// Limits 是发起新操作前的守卫。
//
// 它和 scene 端 1024 位的账本窗口是**一条正确性证明**,不是可调业务数值
// (规格 §4.4 的证明与偏差 #6):未决行数 ≤ MaxPending 且 next_seq − 最小未决 seq < MaxSpan,
// 才能保证任何仍未决的 seq 永远落在 scene 的窗口内、重查时拿得到真实结局。
// 改这两个数之前先改那份证明。
type Limits struct {
	MaxPending uint32
	MaxSpan    uint64
}

// DefaultLimits 是上述证明对应的取值。
var DefaultLimits = Limits{MaxPending: 16, MaxSpan: 512}

func (l Limits) validate() error {
	if l.MaxPending == 0 || l.MaxSpan == 0 {
		return fmt.Errorf("assetop: 守卫上限必须为正(MaxPending=%d MaxSpan=%d)", l.MaxPending, l.MaxSpan)
	}
	return nil
}

var (
	// ErrTooManyPending 该玩家该流的未决行太多或跨度太大,禁止再发起新操作。
	// 帮会侧应回 GuildAssetPending,让玩家稍后再试 —— 这是 fail-closed,不是故障。
	ErrTooManyPending = errors.New("assetop: too many pending ops for player stream")
	// ErrSeqRowMissing seq 行不存在,调用方必须先 EnsureSeqRow。
	ErrSeqRowMissing = errors.New("assetop: seq row missing, call EnsureSeqRow first")
	// ErrSeqRowCorrupt seq 行的纪元为 0(只可能来自 bug 或人工改库)。
	// 继续分配会让 scene 无法区分纪元,所以这里直接失败。
	ErrSeqRowCorrupt = errors.New("assetop: seq row epoch is zero")
	// ErrOutcomeFlip 同一个 seq 在两次查询之间给出了不同的终结结局,违反不变量 I2。
	// 这必定是 bug 或数据损坏,只能告警 + 人工,绝不能自动终结。
	ErrOutcomeFlip = errors.New("assetop: scene outcome changed between queries")
)
