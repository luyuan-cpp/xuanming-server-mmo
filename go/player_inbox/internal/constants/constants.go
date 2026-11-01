// Package constants 是 player_inbox 写进 TipInfoMessage.id 的 tip 码与输入限值。
//
// 码值一律引用导表器生成的枚举(AGENTS §4.4、§7.5),不许手写数字;constants_test.go 的
// TestNoHandWrittenTipCodes 机械守住这条。本服务只引用两条码轴前缀:
//   - PlayerInboxError_:Tip.xlsx 的 //player_inbox_error base=29000 段(设计 §5.3);
//   - CommonError_:请求字段缺失 / 越界(kInvalidParameter),与其它服务同一口径。
//
// 「哪个码算故障」只由 Tip.xlsx 的 fault 列决定(生成到 shared/generated/tip.Faults):
// 本段只有 kPlayerInboxStoreUnavailable 是故障,其余都是业务拒绝。不要在这里另包故障集合。
package constants

import (
	"time"

	"shared/generated/pb/table"
	"shared/serverbase"
)

const (
	// ErrInvalidParameter:请求字段缺失或越界(player_id / request_id 为 0、域未知、Ack 报告乱序等)。
	ErrInvalidParameter = uint32(table.CommonError_kInvalidParameter)

	// ErrDisabled:类型不在 PlayerInbox.EnqueueTypes 白名单里(入箱开关的唯一来源,设计 §3.3)。
	ErrDisabled = uint32(table.PlayerInboxError_kPlayerInboxDisabled)
	// ErrUnknownType:类型未登记或不是持久类(设计 §3.1:瞬时类应改走尽力推送)。
	ErrUnknownType = uint32(table.PlayerInboxError_kPlayerInboxUnknownType)
	// ErrPlayerNotFound:归属区映射里没有这名玩家(设计 §2.2:输错的 player_id 不留永远 PENDING 的行)。
	ErrPlayerNotFound = uint32(table.PlayerInboxError_kPlayerInboxPlayerNotFound)
	// ErrPayloadInvalid:载荷解码失败或不通过类型校验函数。
	ErrPayloadInvalid = uint32(table.PlayerInboxError_kPlayerInboxPayloadInvalid)
	// ErrPayloadTooLarge:载荷超过类型规格的 max_payload_bytes。
	ErrPayloadTooLarge = uint32(table.PlayerInboxError_kPlayerInboxPayloadTooLarge)
	// ErrTextTooLong:operator / reason / note 超长或不是合法 UTF-8(设计 §3.4)。
	ErrTextTooLong = uint32(table.PlayerInboxError_kPlayerInboxTextTooLong)
	// ErrIssuedOutOfWindow:issued_at_ms 不在 [now-7d, now+5min] 内(设计 §6.2 第 5 步)。
	ErrIssuedOutOfWindow = uint32(table.PlayerInboxError_kPlayerInboxIssuedOutOfWindow)
	// ErrFull:该 (玩家, 域) 未终结条目已达 max_pending(设计 §3.4)。
	ErrFull = uint32(table.PlayerInboxError_kPlayerInboxFull)
	// ErrIdempotencyConflict:同一 (producer_kind, request_id) 已存在、内容摘要不同(IB-2)。
	ErrIdempotencyConflict = uint32(table.PlayerInboxError_kPlayerInboxIdempotencyConflict)
	// ErrStaleOwner:围栏判 STALE(请求的 owner_epoch 小于流上的高水位)。
	// Fetch、Ack 的角色身份这一次核不过(冻结、归属或入场登记变了、目录读不到)也回它,响应的 fence 留空:
	// 都不是故障,scene 退避重试,不能用 ErrStoreUnavailable,那会把排空证明打成 Hold。
	ErrStaleOwner = uint32(table.PlayerInboxError_kPlayerInboxStaleOwner)
	// ErrEpochRequired:请求的 owner_epoch 为 0(fail-closed,设计 §7.8)。
	ErrEpochRequired = uint32(table.PlayerInboxError_kPlayerInboxEpochRequired)
	// ErrEpochAhead:请求的 owner_epoch 超过核心 Redis 的当前值(IB-8,只会来自缺陷或伪造)。
	ErrEpochAhead = uint32(table.PlayerInboxError_kPlayerInboxEpochAhead)
	// ErrItemNotFound:运维接口指定的条目不存在。
	ErrItemNotFound = uint32(table.PlayerInboxError_kPlayerInboxItemNotFound)
	// ErrItemStateConflict:运维接口要改的条目已是终态。
	ErrItemStateConflict = uint32(table.PlayerInboxError_kPlayerInboxItemStateConflict)
	// ErrStoreUnavailable:收件箱库、核心 Redis 或 data_service 不可用。Tip.xlsx fault 列为 1,
	// serverbase 记成 rpc_inband_fault 并告警 —— **只用于真故障**,业务拒绝不许复用。
	ErrStoreUnavailable = uint32(table.PlayerInboxError_kPlayerInboxStoreUnavailable)
)

// TipClassifier 返回本服务的 in-band 业务码定性函数,供 serverbase.UnaryInterceptor 使用。
// 定性全部来自 Tip.xlsx 的 fault 列;**不要**在这里包本地 map 去"微调" —— 要改分类,改表。
func TipClassifier() serverbase.Classifier {
	return serverbase.TipVerdict
}

// 输入限值(设计 §3.4、§5.1、§5.2、§6.2)。字节数按 UTF-8 编码后的长度计。
const (
	// MaxOperatorBytes:operator / gm_operator 的字节上限。
	MaxOperatorBytes = 64
	// MaxReasonBytes:reason / note / 欠款载荷 reason 的字节上限。
	MaxReasonBytes = 256
	// MaxPayloadBytesHardCap:任何类型规格的 max_payload_bytes 都不得超过它(注册表自检)。
	// 远低于单列 256KiB 的预算(global-data-layer-tidb-decision.md)。
	MaxPayloadBytesHardCap = 16 * 1024

	// IssuedAtMaxPast / IssuedAtMaxFuture:issued_at_ms 的可接受窗口 [now-7d, now+5min](D14)。
	// 终态行保留期(30 天)必须大于 IssuedAtMaxPast,config.Validate 守住这条(设计 §6.2 生产方契约)。
	IssuedAtMaxPast   = 7 * 24 * time.Hour
	IssuedAtMaxFuture = 5 * time.Minute

	// FetchDefaultItems / FetchMaxItems:Fetch 的 max_items 为 0 时取 16,上限 16(设计 §7.4;
	// 消费方「环装得下」的论证依赖这个上限,改它要连同 §4.4 的 32 条环一起改)。
	FetchDefaultItems = 16
	FetchMaxItems     = 16
	// AckMaxDecisions:一次 Ack 至多 32 条决定(= 消费方最近结局环的容量)。
	AckMaxDecisions = 32

	// ListDefaultLimit / ListMaxLimit:运维 ListItems 的分页。
	ListDefaultLimit = 50
	ListMaxLimit     = 200

	// DeliveryWarnThreshold:同一条目被投递达到这么多次时记 WARN(D11/D14:投递告警阈值 5;
	// 告警规则在第 4 批的 player-inbox-alerts.yaml,读 player_inbox_max_delivery_count)。
	DeliveryWarnThreshold = 5

	// NotifyTimeout:入箱提交后那次在线提醒的预算(设计 §6.2 第 8 步:尽力而为,最多 200ms)。
	NotifyTimeout = 200 * time.Millisecond
	// HomeZoneLookupTimeout:一次 data_service.GetPlayerHomeZone 的上限(与 guild 同值)。
	HomeZoneLookupTimeout = 1500 * time.Millisecond
	// OwnerEpochReadTimeout:一次核心 Redis GET player:{id}:owner_epoch 的上限。
	OwnerEpochReadTimeout = 500 * time.Millisecond
	// LedgerReadTimeout:对账读一次已落盘账本(data_service.GetPlayerInboxLedger)的上限。
	LedgerReadTimeout = 2 * time.Second
	// StoreOpTimeout:后台循环里单条 MySQL 语句的上限(请求路径用整请求预算)。
	StoreOpTimeout = 2 * time.Second
)
