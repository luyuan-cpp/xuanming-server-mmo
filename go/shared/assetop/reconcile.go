package assetop

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"shared/drainproof"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

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
	//   UPDATE {op} SET last_send_ms=GREATEST(last_send_ms, lease_until_ms), lease_until_ms=?, lease_token=?
	//    WHERE op_id=? AND status=<pending> AND lease_until_ms<?
	// RowsAffected==0 → (Op{}, false, nil):行已被别的副本领走或已终结,跳过即可。
	// RowsAffected==1 → 回读整行并解 payload,同时填 Op.CreatedMs 与 Op.LastSendMs。
	// last_send_ms 那一项是自动放弃的安全依据,必须写在改 lease_until_ms **之前**(读到的是上一个租约),
	// 理由见 abandon.go 文件头「LastSendMs 契约」;不维护它的 Store 只是用不了自动放弃(fail-closed)。
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
	//                   durable=?, last_outcome=?, last_reason=?, updated_ms=?,
	//                   last_send_ms=IF(?, GREATEST(last_send_ms, ?), last_send_ms)   -- (!res.Local, nowMs)
	//    WHERE op_id=? AND status=<pending> AND lease_token=?
	// 带回领取令牌是为了:租约已经被别人抢走时,自己这次的结果不该再写回去。
	// last_send_ms 只在本次确实发出过请求(res.Local 为假)时推进:本地合成的 NOT_HERE 没有发送,
	// 推进它会让停放中的离线行永远「刚发过」,自动放弃就永远不会发生(abandon.go「LastSendMs 契约」)。
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

// LedgerReader 读玩家**已落盘**的账本(zone Redis 里的玩家父键 blob,HEAD 口径)。
// 实现在 B5(data_service 只读 RPC GetPlayerAssetOpLedger,DataServiceLedger)。
// 玩家从未落过盘时返回 (nil, nil)。
//
// LoopConfig.RequireSQLCommit 打开时,终结改认 SQL 提交证据:Ledger 还必须实现 SQLCommitLedgerReader
// (Loop.CheckWiring 在启动时断言),blob 只剩自动放弃的「两份都未见」那一半(abandon.go)。
// LoopConfig.RequireVerifiedRedisCopy 打开时,自动放弃读 blob 改走 VerifiedLedgerReader(abandon.go 的 G5 闸)。
type LedgerReader interface {
	ReadPersistedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error)
}

// SQLCommitLedgerReader 读 go/db 在玩家 SQL 行提交后发布的资产 SQL 提交证据里的账本
// (docs/design/db-write-fencing-mysql.md §6.6)。证据缺失时返回 (nil, nil),调用方继续等,
// 不得退回 Redis blob;读失败返回 error。DataServiceLedger 实现它。
//
// 不并进 LedgerReader:开关关闭的服务(以及各业务的测试替身)只需要 blob 一种读法,
// 并进去会逼所有实现同批改。
type SQLCommitLedgerReader interface {
	ReadSQLCommittedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error)
}

// PendingAgeReader 是 Store 的**可选**能力:报告某条流最老未决行的创建时刻。
// 实现了它,Loop 就会每 30s 刷新 assetop_pending_oldest_age_seconds(卡死行告警靠它)。
type PendingAgeReader interface {
	OldestPendingCreatedMs(ctx context.Context, stream assetpb.AssetOpStream) (uint64, bool, error)
}

// maxWakeOpsPerPlayer 是一次落地唤醒最多处理的行数。来历:每个服务独占两条流(I6),
// 每条流的未决行上限是 DefaultLimits.MaxPending = 16(I5),所以一个玩家在一个服务里最多 32 行未决。
// 超过只可能是守卫失效,多出来的照常等自己的 next_attempt_ms。
const maxWakeOpsPerPlayer = 32

// PlayerPendingLister 是 Store 的**可选**能力:列出某个玩家的 PENDING 行主键,供落地唤醒用
// (docs/design/landing-recovery-sync-digest.md §5.3)。
//
// 契约:
//   - 非加锁一致性读,只回主键,条数 <= limit;走按玩家的二级索引
//     (guild 为 idx_guild_asset_op_2,trade 为 idx_trade_asset_op_1);
//   - **不看** next_attempt_ms 与 lease_until_ms:唤醒本来就是要提前,租约由随后的 Claim CAS 判;
//   - 玩家没有 PENDING 行时回 (nil, nil),不是错误。
//
// 不放进 Store 主接口:那会逼两个实现同批改。没实现它的 Store,唤醒只是空操作。
type PlayerPendingLister interface {
	ListPendingByPlayer(ctx context.Context, playerID uint64, limit int) ([]uint64, error)
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
//
// StatusAbandoned 刻意不在白名单里:放弃只能由 AbandonOnce 在全部条件成立时自动产生(abandon.go),
// 人工判定「放弃」时应按证据选 Aborted(scene 流水里查不到 correlation_id)。
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
	// 理由(docs/design/grpc-muduo-transport.md §15.6):scene 的 gRPC 服务端跑在节点 EventLoop 上,
	// 每条资产 RPC 都在逻辑帧所在的那条 loop 线程上同步执行(durable 探测、Redis 写都在这一次调用里),
	// 另见 docs/design/scene-node-threading-model.md。
	// 不设上限时,一次 Tick 的 100 行会同时压向同一个 scene 节点,在 loop 上逐条执行、挤占逻辑帧;
	// 被拖在后面的不只是资产请求,还有 scene_manager 的 CreateScene、match 的 PrepareBattle
	// —— 玩家会卡在进场和开战上,而根因在一个后台补偿循环里,极难联想。
	// scene 服务端自己的上限(每连接 256 条流、全节点在途 1024)只防过载,不替后台补偿限流,
	// 补偿并发必须远低于它们。
	// 第 4 批之前的理由是「每条在途请求占住 scene 一条 sync server poller 线程,上限默认 8」;
	// poller 与 GRPC_SERVER_MAX_POLLERS 已随第 4 批退役,数值 8 不变(Y-06 golden 不变)。
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
	// Streams 是本服务独占的流(不变量 I6)。用于刷新最老未决行年龄与日对账的按类 gauge;留空则不刷。
	Streams []assetpb.AssetOpStream

	// 以下是落地恢复追加的配置(docs/design/landing-recovery-sync-digest.md §6)。
	// 全部允许零值 = 关闭,这样直接写 LoopConfig 字面量、还没接这批配置的调用方行为不变。

	// OfflineAfterAttempts 是进入离线停放档的已投递次数门槛(decide.go 的 OfflineParking)。
	// 0 = 关闭停放档。默认 5:刚下线、正在过图的玩家几分钟内就回来,前几次仍按普通退避。
	OfflineAfterAttempts uint32
	// OfflineBackoff 是停放档的间隔中值(±20% 抖动)。OfflineAfterAttempts > 0 时必须 >= MaxBackoff
	// (否则「停放」比普通退避还勤,等于没有),且 <= 24 小时(唤醒丢失时的最坏延迟上界)。
	OfflineBackoff time.Duration
	// AbandonAfter 是自动放弃的年龄门槛:PENDING 且 created_ms 早于 now − AbandonAfter 才是候选
	// (abandon.go)。0 = 关闭自动放弃;非 0 时不得小于 30 天 —— 放弃不可逆,门槛太短会把
	// 只是暂时不上线的玩家的东西作废。
	AbandonAfter time.Duration
	// AbandonSafetyGap 是放弃条件 4 的安全间隔:距该行最后一次**可能**的发送不足这么久就不放弃。
	// 自动放弃开启时必须 >= 10 分钟且 >= Lease,理由见 abandon.go「LastSendMs 契约」第 3 条。
	AbandonSafetyGap time.Duration
	// AbandonBatch 是一轮放弃最多处理多少行(候选分页大小),开启时取 [1, 1000]。
	AbandonBatch int
	// AbandonInterval 是 RunMaintenance 两轮放弃之间的间隔;自动放弃开启时必须为正。
	AbandonInterval time.Duration
	// AuditInterval 是 RunMaintenance 两轮日对账之间的间隔;0 = 不跑对账,负数非法。
	AuditInterval time.Duration

	// RequireSQLCommit 是资产 SQL 提交证据的开关(docs/design/db-write-fencing-mysql.md §6.6、§8.7、§14 第 23 条)。
	//
	// false(零值,缺省):HEAD 口径。在线按 scene 回包的 durable 终结,离线读账本与自动放弃读 Redis 父键 blob;
	// Ledger 可选。true:在线 / 离线终结都只认 SQL 提交证据(SQLCommitLedgerReader),Result.SQLCommitted 只在这时为真;
	// 自动放弃要 Redis blob 与 SQL 证据都「确定未见」才放弃;Ledger 必填并实现 SQLCommitLedgerReader(CheckWiring 拒启)。
	//
	// **只有本服务会投递到的 zone 全部已切 Fencing.Mode=dual、已上新版 C++,并观察到证据在持续发布之后才打开**
	// (§8.7 第 ⑤ 步)。打开前要接受:已离线、新链路上线前没有证据的玩家,待办要等他再次存盘才能终结。
	// 回退时先关它,再把 zone 切回 legacy 或回退 C++。
	RequireSQLCommit bool

	// RequireVerifiedRedisCopy 是自动放弃的「Redis 副本已核验」闸(docs/design/zone-redis-regression-guard-20261022.md §4.5,
	// abandon.go 条件 5)。DefaultLoopConfig 取 true。
	//
	// true:Redis 父键 blob 给出「确定未见」时,还要这份副本经过当前实例身份核验(读 blob 的同一段 Lua 里,
	// 实例身份 == 屏障身份戳 verified_identity 且 role=master)才放弃;未核验、身份未知、读失败一律跳过并还租约
	// (redis_unverified)。Ledger 必须实现 VerifiedLedgerReader,否则自动放弃不运行。「已见」照常终结,不受影响。
	// false:闸之前的口径,任何 blob 都可以作证据;Redis 回退之后可能把 SQL 已应用的一笔放弃掉(复制或白扣),
	// 每个进程第一轮放弃时打一次 ERROR。
	//
	// **身份戳只由 login 的核验刷新**:login 的 ParentLoad.VerifyOnRedisIdentityChange 关闭时戳不再更新,
	// Redis 身份一变本闸就挡住全部放弃(只是推迟,不是错放)。运维关 login 核验时要同时知道这一点。
	// 直接写 LoopConfig 字面量的调用方必须显式透传本项:零值 false 等于关闭闸。
	RequireVerifiedRedisCopy bool
}

// DefaultLoopConfig 是规格 §4.37 的取值,加上落地恢复设计 §6 的取值。
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
		OfflineAfterAttempts:  5,
		OfflineBackoff:        30 * time.Minute,
		AbandonAfter:          180 * 24 * time.Hour,
		AbandonSafetyGap:      time.Hour,
		AbandonBatch:          200,
		AbandonInterval:       time.Hour,
		AuditInterval:         24 * time.Hour,

		RequireVerifiedRedisCopy: true,
	}
}

// offlineParking 把停放档的两项配置交给纯函数(decide.go)。
func (c LoopConfig) offlineParking() OfflineParking {
	return OfflineParking{AfterAttempts: c.OfflineAfterAttempts, Backoff: c.OfflineBackoff}
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
	// 停放档:只在开启时校验间隔,关闭时(OfflineAfterAttempts=0)OfflineBackoff 是多少都无所谓。
	if c.OfflineAfterAttempts > 0 {
		if c.OfflineBackoff < c.MaxBackoff {
			return fmt.Errorf("assetop: OfflineBackoff(%v)不得小于 MaxBackoff(%v):停放比普通退避还勤等于没有停放",
				c.OfflineBackoff, c.MaxBackoff)
		}
		if c.OfflineBackoff > maxOfflineBackoff {
			return fmt.Errorf("assetop: OfflineBackoff(%v)不得超过 %v:唤醒丢失时玩家要等满这么久", c.OfflineBackoff, maxOfflineBackoff)
		}
	}
	if c.AuditInterval < 0 {
		return fmt.Errorf("assetop: AuditInterval 不得为负(当前 %v;0 表示不跑对账)", c.AuditInterval)
	}
	return c.validateAbandon()
}

// maxOfflineBackoff 是停放间隔的上限。毫秒 / 秒配错一个量级时,这道闸让服务起不来,
// 而不是让离线行一停就是几十天。
const maxOfflineBackoff = 24 * time.Hour

// Processed 是处理一行的结果,供同步路径(业务写 RPC 提交后立刻投一次)判断该回什么。
type Processed struct {
	Result    Result
	Finalized bool
	Status    Status
}

// Loop 是重投循环。构造后除下面几个可选字段外只读。
type Loop struct {
	// Relay 在 worker、唤醒与同步投递启动前由实际进程 owner 注入。
	Relay   *drainproof.Session
	cfg     LoopConfig
	store   Store
	applier Applier
	metrics *Metrics
	now     func() time.Time

	// Ledger 读已落盘账本。开关关闭(LoopConfig.RequireSQLCommit=false)时可选:能读时长期离线且已记账的行
	// 可以提前终结(规格 §4.37),它也是自动放弃的证据来源,没有它两者都不运行。开关打开时必填,且必须实现
	// SQLCommitLedgerReader:在线 / 离线终结都靠它确认 SQL 已提交。
	// 在 Run / ProcessOne 之前设置,设置完调 CheckWiring;运行期间不允许修改。
	Ledger LedgerReader
	// Manual 可选:人工终结通道(规格 §4.38)。只服务于「进程里已经有 Loop」的调用方;
	// 不连 scene 的工具(assetopfix)直接用包级 ResolveManually,不要为它造一个假 Loop。
	Manual ManualResolver
	// Rand 可选:退避抖动的随机源,单测注入确定值。为 nil 时取中值,不抖动。
	Rand func() float64
	// Locator 可选:自动放弃前重新解析玩家位置(abandon.go 条件 3)。生产与 Caller.Resolver
	// 共用同一个 scenenode.Locator。为 nil 时自动放弃不运行(fail-closed)。在 RunMaintenance 之前设置。
	Locator Resolver

	// slots 是本副本「同时在途的资产 RPC」的唯一预算,容量 = Workers:Tick 的 worker 与 WakePlayer
	// 都要先拿到一个名额才领行(LoopConfig.Workers 的理由对两者同样成立)。业务写 RPC 直接调
	// ProcessOne 的同步投递不经过它,与引入唤醒之前一致。
	slots chan struct{}

	// abandonMu 串行化 AbandonOnce,并保护 abandonCursor(下一轮从哪个 op_id 之后接着翻页)。
	abandonMu     sync.Mutex
	abandonCursor uint64
	// auditMu 串行化 AuditOnce 与 resetAuditGauges,并保护 auditExtra:上一轮报过的、不在
	// LoopConfig.Streams 里的 (流, 类别) 组合,本轮没再出现时要置 0(audit.go)。
	auditMu    sync.Mutex
	auditExtra map[pendingKey]struct{}
	// 「功能没装配齐、不运行」各只打一次 INFO,免得每一轮都刷一遍。
	auditOffOnce   sync.Once
	abandonOffOnce sync.Once
	// abandonUnverifiedOnce:RequireVerifiedRedisCopy 关闭时第一轮放弃打一次 ERROR —— 闸被关掉是要有人知道的事。
	abandonUnverifiedOnce sync.Once
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
	return &Loop{
		cfg:     cfg,
		store:   store,
		applier: applier,
		metrics: m,
		now:     now,
		slots:   make(chan struct{}, cfg.Workers),
	}, nil
}

// CheckWiring 在可选字段(Ledger 等)挂好之后、Run / ProcessOne 之前调用:开关要求的依赖缺了就回错误,
// 装配方据此拒启。开关打开却没有 SQL 证据读取器时,每一笔终结都只能停在 AwaitDurable,
// 这种装配必须在启动时炸,不能留到运行期静默卡住(fence-inflight.md F6 (b))。
func (l *Loop) CheckWiring() error {
	if l.cfg.RequireSQLCommit && l.sqlLedger() == nil {
		return errors.New("assetop: 打开了 RequireSQLCommit 却没有挂实现 SQLCommitLedgerReader 的 Loop.Ledger(检查 DataServiceRpc 配置)")
	}
	return nil
}

// RequireSQLCommit 报告本循环是否打开了资产 SQL 提交证据开关(LoopConfig.RequireSQLCommit),供装配方打日志。
func (l *Loop) RequireSQLCommit() bool { return l.cfg.RequireSQLCommit }

// sqlLedger 返回能读 SQL 提交证据的那份 Ledger;没挂或没实现时为 nil。
func (l *Loop) sqlLedger() SQLCommitLedgerReader {
	if l.Ledger == nil {
		return nil
	}
	reader, _ := l.Ledger.(SQLCommitLedgerReader)
	return reader
}

// verifiedLedger 返回能报告「Redis 副本是否已核验」的那份 Ledger(abandon.go 的 G5 闸);没挂或没实现时为 nil。
func (l *Loop) verifiedLedger() VerifiedLedgerReader {
	if l.Ledger == nil {
		return nil
	}
	reader, _ := l.Ledger.(VerifiedLedgerReader)
	return reader
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

// RunMaintenance 按 AuditInterval 跑日对账(AuditOnce)、按 AbandonInterval 跑自动放弃(AbandonOnce),
// 直到 ctx 结束。调用方用 safego.Go 启动它。
//
// **只该在一个副本上跑**:装配方用 go/shared/leader 选主,当选时用领导权的 ctx 启动、卸任时取消
// (设计 §6.2)。多个副本同时跑不会出错 —— 对账只读,放弃靠 Claim CAS 与令牌互斥 —— 但按类 gauge
// 会有多份、候选扫描会重复。
//
// 启动后先跑一轮对账:按类 gauge 不该在每次部署或换主之后空着 24 小时。放弃不立刻跑,等满一个间隔,
// 免得领导权来回抖动时每次当选都扫一遍。退出时清空按类 gauge(理由见 Metrics.resetPendingClass)。
// 每一轮各自 recover,理由同 Run。
func (l *Loop) RunMaintenance(ctx context.Context) {
	defer l.resetAuditGauges()

	var auditC, abandonC <-chan time.Time
	if l.cfg.AuditInterval > 0 {
		ticker := time.NewTicker(l.cfg.AuditInterval)
		defer ticker.Stop()
		auditC = ticker.C
		safego.Run("assetop.audit", func() {
			l.maintenanceRound(ctx, func(call context.Context) error { return l.AuditOnce(call, l.nowMs()) })
		})
	}
	if l.cfg.AbandonAfter > 0 {
		ticker := time.NewTicker(l.cfg.AbandonInterval)
		defer ticker.Stop()
		abandonC = ticker.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-auditC:
			safego.Run("assetop.audit", func() {
				l.maintenanceRound(ctx, func(call context.Context) error { return l.AuditOnce(call, l.nowMs()) })
			})
		case <-abandonC:
			safego.Run("assetop.abandon", func() {
				l.maintenanceRound(ctx, func(call context.Context) error { _, err := l.AbandonOnce(call, l.nowMs()); return err })
			})
		}
	}
}

// Tick 领一批到期行并逐行处理,返回实际处理了多少行。
//
// 并发严格受 Workers 限制,原因见 LoopConfig.Workers。Tick 等所有 worker 结束才返回,
// 于是「同时在途的资产 RPC」在一个副本内恒 <= Workers,不会因为上一轮没跑完就叠加;
// 每个 worker 处理每一行前还要从 slots 拿名额,与同时进行的 WakePlayer 共用这份预算。
func (l *Loop) Tick(ctx context.Context) int {
	ids, err := l.store.ListDue(ctx, l.nowMs(), l.cfg.Batch)
	if err != nil {
		l.metrics.incStoreError("list")
		logx.Errorf("[AssetOp] Failed to list due rows: %v", err)
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
		if l.claimAndProcess(ctx, opID) {
			processed.Add(1)
		}
	}
}

// claimAndProcess 是退避重投(Tick)与落地唤醒(WakePlayer)**共用**的单行路径:
// 拿并发名额 → Claim CAS → ProcessOne → 还名额。两个入口只在「这一批 op_id 从哪来」上不同,
// 领取与处理一个字都不分叉 —— 这正是唤醒不改变资产通道任何正确性论证的原因(设计 L-4)。
//
// 返回这一行是否被本副本领到并处理过。ProcessOne 出错也算处理过:单行失败只记不抛,
// 下一行照常处理,这一行靠 next_attempt_ms 再来。
//
// 拿到名额之后、领取之前再看一次剩余预算(rowBudgetLeft):等名额可能已经吃掉了大半预算,
// 此时领下来的行会被截在半路 —— 投递超时被记成传输失败、attempts+1 并按退避重排;或者 Claim 已提交、
// 调用方的 ctx 先到期,行带着租约却没人处理。不够一整个 OpBudget 就不领,行原样留给下一次。
// Tick 用的是没有截止时刻的 Run ctx,这一步对它只等于「ctx 还没结束」。
func (l *Loop) claimAndProcess(ctx context.Context, opID uint64) bool {
	if l.Relay != nil {
		finish, err := l.Relay.Enter()
		if err != nil {
			return false
		}
		defer finish()
		defer func() {
			if recovered := recover(); recovered != nil {
				l.Relay.Hold()
				panic(recovered)
			}
		}()
	}
	if !l.acquireSlot(ctx) {
		return false
	}
	defer l.releaseSlot()
	if !l.rowBudgetLeft(ctx) {
		return false
	}

	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.cfg.OpBudget)
	defer cancel()
	op, claimed := l.claim(work, opID)
	if claimed != claimOK {
		return false
	}
	if _, err := l.processOne(work, op); err != nil {
		if l.Relay != nil {
			l.Relay.Hold()
		}
		logx.Errorf("[AssetOp] Failed to process pending row op_id=%d stream=%d seq=%d: %v",
			op.OpID, op.Stream, op.Seq, err)
	}
	return true
}

// acquireSlot 拿一个在途名额;ctx 先结束则返回 false。
func (l *Loop) acquireSlot(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	if l.slots == nil {
		// 只有绕过 NewLoop 造出来的 Loop 会走到这里;不设预算总比永远阻塞在 nil 通道上好。
		return true
	}
	select {
	case l.slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (l *Loop) releaseSlot() {
	if l.slots == nil {
		return
	}
	<-l.slots
}

// rowBudgetLeft 报告 ctx 剩下的时间还够不够**完整**处理一行(领取 + 投递 + 落库 <= OpBudget)。
// ctx 已结束时为假;没有截止时刻时只看是否结束。
//
// 用的是真实时钟(time.Until)而不是 l.now:ctx 的截止时刻本来就是按真实单调时钟设的,
// l.now 在单测里是一个固定时刻,拿它去减截止时刻毫无意义。
func (l *Loop) rowBudgetLeft(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) >= l.cfg.OpBudget
}

// WakePlayer 立即重投某个玩家的全部 PENDING 行,返回实际领到并处理的行数
// (docs/design/landing-recovery-sync-digest.md §5.3)。
//
// 它是落地唤醒(shared/assetopwake)的落点:scene 在玩家进场完成时发布唤醒,本副本抢到去重键后调它。
// 每一行走的都是与 Tick 相同的 claimAndProcess(Claim CAS + ProcessOne),所以:
//   - 不必改 next_attempt_ms 也能领:Claim 只看 status 与租约(Store.Claim 契约),
//     也就避开了 TiDB 下修改 idx_0 列之前必须先加锁的规则;
//   - 结局、重排、终结都与退避重投一致,唤醒没有任何新的状态迁移;玩家仍在加载、战斗或冻结时
//     scene 回 RETRY,行按原有退避重排(设计 L-4);
//   - 并发计入 Workers 预算(与 Tick 共用 slots),一波登录潮不会挤占 scene 的逻辑帧(EventLoop)。
//
// 逐行串行:一个玩家最多 32 行,串行只占一个名额,而且这些请求本来就都落在同一个 scene 节点上。
//
// ctx 是这次唤醒的总预算(assetopwake 默认给 10 秒),它**只决定要不要开始下一行**:剩余时间不够
// 一整个 OpBudget 就不再领取(rowBudgetLeft),已经开始的行总能在自己的 OpBudget 内走完,
// 不会被截在半路。没轮到的行照常等各自的 next_attempt_ms —— 停放中的行最多再等一个 OfflineBackoff。
// 总预算连一行都装不下(WakeTimeout 配得比 OpBudget 还小)时回错误:那样的唤醒永远领不到行,
// 不报出来就会静默退化成「从不唤醒」。
//
// Store 没实现 PlayerPendingLister 时回 (0, nil):唤醒是加速,不是正确性路径。
func (l *Loop) WakePlayer(ctx context.Context, playerID uint64) (int, error) {
	lister, ok := l.store.(PlayerPendingLister)
	if !ok || playerID == 0 {
		l.metrics.IncWake(WakeNone)
		return 0, nil
	}
	if ctx.Err() == nil && !l.rowBudgetLeft(ctx) {
		l.metrics.IncWake(WakeError)
		return 0, fmt.Errorf("assetop: 唤醒预算不足一个 OpBudget(%v),一行也领不了;检查唤醒超时配置 player_id=%d",
			l.cfg.OpBudget, playerID)
	}
	ids, err := lister.ListPendingByPlayer(ctx, playerID, maxWakeOpsPerPlayer)
	if err != nil {
		l.metrics.incStoreError("wake_list")
		l.metrics.IncWake(WakeError)
		return 0, fmt.Errorf("assetop: 列出玩家未决行失败 player_id=%d: %w", playerID, err)
	}
	if len(ids) > maxWakeOpsPerPlayer {
		ids = ids[:maxWakeOpsPerPlayer]
	}

	processed := 0
	for _, opID := range ids {
		// 预算只会越用越少:这一行开始不了,后面的也开始不了。
		if !l.rowBudgetLeft(ctx) {
			break
		}
		if l.claimAndProcess(ctx, opID) {
			processed++
		}
	}
	if processed > 0 {
		l.metrics.IncWake(WakeWoke)
	} else {
		l.metrics.IncWake(WakeNone)
	}
	return processed, nil
}

// claimResult 是领取一行的结果。自动放弃要区分「被别人领走」与「出错」,分别计数。
type claimResult uint8

const (
	// claimOK 领到了,租约在本副本手里。
	claimOK claimResult = iota
	// claimLost 没领到:被别的副本领走、已终结,或是毒行(Store 已推迟它)。
	claimLost
	// claimFailed 存储或令牌生成出错。
	claimFailed
)

// claim 领一行。返回值不是 claimOK 时这一行本轮不处理(被别人领走、已终结、毒行或出错)。
func (l *Loop) claim(ctx context.Context, opID uint64) (Op, claimResult) {
	token, err := newLeaseToken()
	if err != nil {
		l.metrics.incStoreError("claim")
		logx.Errorf("[AssetOp] Failed to generate lease token op_id=%d: %v", opID, err)
		return Op{}, claimFailed
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
		logx.Errorf("[AssetOp] Could not decode pending row payload; postponed for manual handling op_id=%d", opID)
		return Op{}, claimLost
	case err != nil:
		if l.Relay != nil {
			l.Relay.Hold()
		}
		l.metrics.incStoreError("claim")
		logx.Errorf("[AssetOp] Failed to claim pending row op_id=%d: %v", opID, err)
		return Op{}, claimFailed
	case !claimed:
		l.metrics.incClaim("lost")
		return Op{}, claimLost
	}
	l.metrics.incClaim("claimed")
	return op, claimOK
}

// ProcessOne 处理一行:决定方向 → 投一次 → 按 Decide 分支落地。
//
// 业务写 RPC 在提交自己的事务之后可以**同步**调用它,让玩家当场看到结果;
// 那条路径的 ctx 预算是 2500ms(= OpBudget):投递分到 1800ms(CallTimeout 800ms +
// 重查 700ms 仍在内),落库另有 settleBudget 的 700ms,且不受父 ctx 过期/取消影响。
func (l *Loop) ProcessOne(ctx context.Context, op Op) (out Processed, resultErr error) {
	if err := ctx.Err(); err != nil {
		return Processed{}, err
	}
	if l.Relay != nil {
		finish, err := l.Relay.Enter()
		if err != nil {
			return Processed{}, err
		}
		defer finish()
		defer func() {
			if recovered := recover(); recovered != nil {
				l.Relay.Hold()
				panic(recovered)
			}
			if resultErr != nil {
				l.Relay.Hold()
			}
		}()
	}
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.cfg.OpBudget)
	defer cancel()
	return l.processOne(work, op)
}

func (l *Loop) processOne(ctx context.Context, op Op) (Processed, error) {
	nowMs := l.nowMs()

	rpc, ok := l.rpcFor(op, nowMs)
	if !ok {
		// 流号非法 = 坏行。绝不猜方向:猜错就是把扣钱发成发钱。
		l.metrics.incUnknown(op.Stream)
		logx.Errorf("[AssetOp] Invalid stream id in pending row; cannot determine delivery direction op_id=%d stream=%d seq=%d",
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
	// 终结前的证据核对按开关分两种口径(LoopConfig.RequireSQLCommit):打开时只认 SQL 提交证据;
	// 关闭时回到 HEAD 口径(scene 回包的 durable / Redis blob),托管扣出另要求原件与本笔身份一致。
	if err == nil && !res.SQLCommitted && isSceneTerminal(res.Outcome) {
		if l.cfg.RequireSQLCommit {
			res, err = l.verifySQLCommit(applyCtx, op, res)
		} else {
			res = withMatchingReceipt(op, res)
		}
	}

	switch Decide(res, err) {
	case ActionFinalize:
		return l.finalize(ctx, op, rpc, res, nowMs)
	case ActionAwaitDurable:
		if l.Relay != nil {
			l.Relay.Hold()
		}
		next := nowMs + uint64(l.cfg.AwaitDurableDelay/time.Millisecond)
		return l.reschedule(ctx, op, next, res, nowMs, "await_durable")
	case ActionRetry:
		// 长期拿不到位置的行降到停放档(设计 §6.1),重排原因单列 offline,便于看停放了多少。
		park := l.cfg.offlineParking()
		next := NextRetryMs(nowMs, op.Attempts, res, l.cfg.BaseBackoff, l.cfg.MaxBackoff, park, l.Rand)
		reason := "retry"
		if park.Parks(res, op.Attempts) {
			reason = "offline"
		}
		if err != nil {
			if l.Relay != nil {
				l.Relay.Hold()
			}
			return l.rescheduleTransportFailure(ctx, op, next, res, nowMs)
		}
		return l.reschedule(ctx, op, next, res, nowMs, reason)
	default: // ActionAlert
		if l.Relay != nil {
			l.Relay.Hold()
		}
		if errors.Is(err, ErrOutcomeFlip) {
			// 翻转已由 Caller 计过 assetop_outcome_flip_total,这里只留日志证据。
			logx.Errorf("[AssetOp] Outcome changed; not finalizing op_id=%d stream=%d seq=%d: %v",
				op.OpID, op.Stream, op.Seq, err)
		} else {
			l.metrics.incUnknown(op.Stream)
			logx.Errorf("[AssetOp] Scene returned UNKNOWN; not finalizing op_id=%d stream=%d seq=%d epoch=%d reason=%d",
				op.OpID, op.Stream, op.Seq, op.StreamEpoch, res.Reason)
		}
		return l.reschedule(ctx, op, l.alertNextMs(nowMs), res, nowMs, "alert")
	}
}

// verifySQLCommit 将 scene 的结局与 SQL 提交证据核对(只在 LoopConfig.RequireSQLCommit 打开时调用)。
// durable=false 的 scene 回包也可能已在随后提交，反之 Redis durable=true 绝不能单独使跨服务对侧入账。
// 拿不到证据(读失败、缺证据、证据未见本笔)一律按未落盘返回,由 Decide 走 AwaitDurable 重排,不终结。
func (l *Loop) verifySQLCommit(ctx context.Context, op Op, res Result) (Result, error) {
	res.Durable = false
	res.Snapshot = nil // 在线回包不是已提交的托管原件证明。
	if ctx.Err() != nil {
		return res, nil
	}
	reader := l.sqlLedger()
	if reader == nil {
		// CheckWiring 本应在启动时拦下;走到这里说明装配方漏调了它。不静默:每笔都计数并打 ERROR。
		l.metrics.incLedgerRead("unwired")
		logx.Errorf("[AssetOp] RequireSQLCommit is on but no SQL commit ledger reader is attached; operation stays pending op_id=%d stream=%d seq=%d",
			op.OpID, op.Stream, op.Seq)
		return res, nil
	}
	ledger, err := reader.ReadSQLCommittedLedger(ctx, op.PlayerID)
	if err != nil {
		l.metrics.incLedgerRead("error")
		l.metrics.incStoreError("ledger_read")
		logx.Errorf("[AssetOp] Failed to read asset SQL commit evidence; operation stays pending op_id=%d: %v", op.OpID, err)
		return res, nil
	}
	if ledger == nil {
		l.metrics.incLedgerRead("absent")
		return res, nil
	}
	outcome, partial, reason := ClassifyPersisted(ledger, op.Stream, op.StreamEpoch, op.Seq)
	if !isSceneTerminal(outcome) {
		l.metrics.incLedgerRead("unseen")
		return res, nil
	}
	proved := resultWithReceipt(ledger, op, Result{Outcome: outcome, Reason: reason, Durable: true, SQLCommitted: true, Partial: partial})
	if !proved.Terminal() {
		l.metrics.incLedgerRead("unseen")
		return proved, nil
	}
	if outcome != res.Outcome {
		return res, ErrOutcomeFlip
	}
	proved.Partial = proved.Partial || res.Partial
	if proved.Reason == 0 {
		proved.Reason = res.Reason
	}
	return proved, nil
}

func resultWithReceipt(ledger *componentpb.PlayerAssetOpLedgerComp, op Op, res Result) Result {
	res.Snapshot = nil
	receipt := matchingReceipt(ledger, op)
	if receipt == nil {
		return Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN}
	}
	if res.Outcome == assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED && escrowsInstances(op) {
		if !receiptSnapshotMatches(op, receipt.GetSnapshot()) {
			return Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN}
		}
		res.Snapshot = proto.Clone(receipt.GetSnapshot()).(*assetpb.AssetSnapshot)
	} else if snapshotHasInstances(receipt.GetSnapshot()) {
		return Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN}
	}
	return res
}

// withMatchingReceipt 是开关关闭(HEAD 口径)时终结前对托管原件的同一道判据:只有实例扣出才带原件,
// 且快照必须与本笔扣出的实例身份逐一对上,否则丢掉(不让一个空快照把纯货币托管也拖进收据 ACK);
// APPLIED 的实例扣出缺原件时按未落盘处理(AwaitDurable 重排),与 resultWithReceipt 同口径。
// 帮会、发奖等不是托管的流本就不带快照,原样通过。
func withMatchingReceipt(op Op, res Result) Result {
	if res.Snapshot != nil && (!escrowsInstances(op) || !receiptSnapshotMatches(op, res.Snapshot)) {
		res.Snapshot = nil
	}
	if escrowAwaitsReceipt(op, res) {
		res.Durable = false
	}
	return res
}

// escrowsInstances:本笔是实例扣出(按道具 guid 或宝宝 id 扣出已有实例)。
func escrowsInstances(op Op) bool {
	return len(op.Bundle.GetItemUuids()) > 0 || op.Bundle.GetPetId() != 0
}

// escrowAwaitsReceipt:APPLIED 的实例扣出却没有可用原件。这时不能终结,否则托管方拿不到过户所需的完整实例。
func escrowAwaitsReceipt(op Op, res Result) bool {
	return res.Outcome == assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED && escrowsInstances(op) && res.Snapshot == nil
}

func receiptSnapshotMatches(op Op, snapshot *assetpb.AssetSnapshot) bool {
	if op.Stream != assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT || snapshot == nil ||
		len(snapshot.GetItems()) != len(op.Bundle.GetItemUuids()) ||
		(snapshot.GetPet() != nil) != (op.Bundle.GetPetId() != 0) {
		return false
	}
	identities := make(map[uint64]bool, len(op.Bundle.GetItemUuids()))
	for _, id := range op.Bundle.GetItemUuids() {
		identities[id] = true
	}
	for _, item := range snapshot.GetItems() {
		if item == nil || item.GetItemUuid() == 0 || !identities[item.GetItemUuid()] || len(item.GetInstanceProto()) == 0 {
			return false
		}
		delete(identities, item.GetItemUuid())
	}
	return len(identities) == 0 && (snapshot.GetPet() == nil || snapshot.GetPet().GetPetId() == op.Bundle.GetPetId() && len(snapshot.GetPet().GetInstanceProto()) > 0)
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
		logx.Errorf("[AssetOp] Finalization branch could not determine final state (bug) op_id=%d stream=%d seq=%d outcome=%d",
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
			logx.Errorf("[AssetOp] Partial grant finalized without updating counterpart ledger; manual compensation required op_id=%d stream=%d seq=%d corr=%d",
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
			logx.Infof("[AssetOp] Lease taken over; reschedule not applied op_id=%d stream=%d seq=%d: %v",
				op.OpID, op.Stream, op.Seq, err)
			return Processed{Result: res}, nil
		}
		l.metrics.incStoreError("reschedule")
		return Processed{Result: res}, fmt.Errorf("assetop: 重排失败 op_id=%d: %w", op.OpID, err)
	}
	l.metrics.incReschedule(op.Stream, reason)
	return Processed{Result: res}, nil
}

// rescheduleTransportFailure 重排一次「没拿到 scene 任何答复」的投递(传输失败、超时、定位出错)。
//
// 落库的那一份结局写 RETRY(原因码 0),而不是 Caller 回的零值 Result(结局 UNKNOWN):
// last_outcome = UNKNOWN 在列上专指「scene 回了 UNKNOWN / 坏流号 / 毒行」这类要人看的行 ——
// 日对账按它归 alert 类(ClassifyPending),assetopfix 按它判「反复 UNKNOWN」。传输失败若也写 UNKNOWN,
// 对账那一刻 scene 短暂不可达就会让 alert 类误报一整天(gauge 24 小时才刷新一次),assetopfix 也会把
// 「scene 可能已应用、只是回包丢了」的行当成可人工终结的候选。RETRY 的语义正是「结局未定、稍后再投」。
//
// 只改落库的那一份:返回给同步路径的仍是本次真实结果(零值),与以前一致。
// 其余列照常:durable=0;last_reason 仍经 carryPartialReason(27007 粘性不变);
// last_send_ms 照常推进(Result.Local 为假,按「可能发过」保守处理,见 abandon.go「LastSendMs 契约」)。
// 实时信号不受影响:RPC 调用失败仍计 assetop_rpc_total{outcome="error"}(Caller.invoke);
// 一直投不出去的行会在 online_retry 类里变老,由该类的最老年龄告警兜住。
//
// 这时列上的 RETRY + 原因码 0 与 scene 的「物品 guid 号段余量不足」(asset_op_system.cpp 回 RETRY、
// 原因 0)分不开;两者都是「稍后再投」,分类与 assetopfix 的判定对它们一致,不需要分开。
func (l *Loop) rescheduleTransportFailure(ctx context.Context, op Op, nextAttemptMs uint64, res Result, nowMs uint64) (Processed, error) {
	stored := Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY}
	processed, err := l.reschedule(ctx, op, nextAttemptMs, stored, nowMs, "retry")
	processed.Result = res
	return processed, err
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
// 为什么安全:读的是已经落盘的那份数据,而已记账的结局永不改变(不变量 I2),
// 所以玩家在不在线都不影响结论,读到的结局天然就是 durable 的。读哪一份按开关定(readFinalLedger):
// 关闭时是 Redis 父键 blob(HEAD 口径),打开时只认 SQL 提交证据,结果才带 SQLCommitted。
// 读不到结局的行**不**在 Go 侧自行判中止(不变量 I7):scene 没记账就无法保证
// 这个 seq 将来不会被一个晚到的请求应用。
func (l *Loop) tryPersistedLedger(ctx context.Context, op Op, res Result) Result {
	if l.Ledger == nil || op.Attempts < l.cfg.LedgerReadMinAttempts {
		return res
	}

	ledger, err := l.readFinalLedger(ctx, op.PlayerID)
	if err != nil {
		l.metrics.incLedgerRead("error")
		l.metrics.incStoreError("ledger_read")
		logx.Errorf("[AssetOp] Failed to read persisted ledger op_id=%d: %v", op.OpID, err)
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
	proved := resultWithReceipt(ledger, op, Result{Outcome: outcome, Reason: reason, Durable: true, SQLCommitted: l.cfg.RequireSQLCommit, Partial: partial})
	if !proved.Terminal() {
		l.metrics.incLedgerRead("unseen")
		return proved
	}
	l.metrics.incLedgerRead("finalized")
	logx.Infof("[AssetOp] Persisted outcome read offline; finalization possible op_id=%d stream=%d seq=%d outcome=%s partial=%t",
		op.OpID, op.Stream, op.Seq, outcomeLabel(outcome), partial)
	return proved
}

// readFinalLedger 读「终结所认的那份已落盘账本」:开关打开读 SQL 提交证据,关闭读 Redis 父键 blob(HEAD 口径)。
// 开关打开却没有 SQL 读取器时回错误(CheckWiring 本应在启动时拦下),绝不退回读 blob。
func (l *Loop) readFinalLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	if !l.cfg.RequireSQLCommit {
		return l.Ledger.ReadPersistedLedger(ctx, playerID)
	}
	reader := l.sqlLedger()
	if reader == nil {
		return nil, errors.New("assetop: RequireSQLCommit 打开但 Loop.Ledger 没有实现 SQLCommitLedgerReader")
	}
	return reader.ReadSQLCommittedLedger(ctx, playerID)
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
			logx.Errorf("[AssetOp] Failed to read oldest unresolved row stream=%d: %v", stream, err)
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

// maintenanceRound 与重投共用同一角色；清理/放弃的提交失败也不能伪装成已结算。
func (l *Loop) maintenanceRound(ctx context.Context, round func(context.Context) error) {
	if ctx.Err() != nil {
		return
	}
	if l.Relay != nil {
		finish, err := l.Relay.Enter()
		if err != nil {
			return
		}
		defer finish()
		defer func() {
			if recovered := recover(); recovered != nil {
				l.Relay.Hold()
				panic(recovered)
			}
		}()
	}
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := round(work); err != nil && l.Relay != nil {
		l.Relay.Hold()
	}
}
