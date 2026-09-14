// Package constants 是 chat 服务写进 TipInfoMessage.id 的 tip 码(契约 §9)。
//
// chat 还没有自己的业务域段:Tip.xlsx 开 chat 段之前,v1 只复用 common 段的既有码
// (与路由服同一做法)。开段后把这里整体换成 table.ChatError_kX,客户端文案随表走。
// 码值一律引用导表器生成的枚举(AGENTS.md §7.5),不许手写数字;
// chat_test.go 的 TestNoHandWrittenTipCodes 机械守住这条。
package constants

import (
	"shared/generated/pb/table"
	"shared/serverbase"
)

const (
	// ErrInvalidParameter:取不到会话(缺 / 坏 x-session-detail-bin)、请求体缺 message、
	// PRIVATE 缺对端或对端是自己、频道枚举值未知、request_id 超长。
	// 无会话刻意不用 kPlayerNotFoundInSession:那是 fault 码,会把"客户端没带身份"
	// 刷成服务端故障告警(契约 §3)。
	ErrInvalidParameter = uint32(table.CommonError_kInvalidParameter)

	// ErrMessageTooLong:内容 trim 后为空,或原始字节数超过 MaxContentBytes。
	// gate 对整包 >1024B 已先拒一道,这里是 chat 侧按"内容字节"的第二道闸。
	ErrMessageTooLong = uint32(table.CommonError_kMessageSizeExceeded)

	// ErrRateLimited:chat:{rl:<pid>} 每秒计数超过 RateLimitPerSecond(gate 每会话
	// 每消息号 3 次/秒是第一道)。
	ErrRateLimited = uint32(table.CommonError_kRateLimitExceeded)

	// ErrChannelUnavailable:TEAM / SYSTEM / UNSPECIFIED 频道 v1 不开放。
	ErrChannelUnavailable = uint32(table.CommonError_kFeatureUnavailable)

	// ErrStorage:ChatRedis 读写失败 / 返回形态异常 / 序列化失败。
	// 这是 Tip.xlsx 里 fault 列标 1 的码,serverbase 会把它记成 rpc_inband_fault 并告警 ——
	// **只用于真故障**,任何业务拒绝都不许复用它,否则告警会被正常流量淹没。
	ErrStorage = uint32(table.CommonError_kServiceUnavailable)
)

// TipClassifier 返回本服务的 in-band 业务码定性函数,供 serverbase.UnaryInterceptor 使用。
//
// chat 复用的 5 个 common 码的定性(哪个算故障)已经在 Tip.xlsx 的 fault 列里,
// 生成到 shared/generated/tip.Faults,serverbase.TipVerdict 直接认得;
// 这里显式交回全局判定只是为了固定本服务的定性接缝(与 friend 同口径),
// **不要**在这里包一层本地 map 去"微调"—— 要改分类,改表。
func TipClassifier() serverbase.Classifier {
	return serverbase.TipVerdict
}
