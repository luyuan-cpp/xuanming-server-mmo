// Package constants 是支付服务写进 TipInfoMessage.id 的 tip 码。
//
// 码值一律引用导表器生成的枚举(AGENTS.md §7.5),不许手写数字。支付专用码在 Tip.xlsx 的 //payment_error base=31000 段;
// 「哪个码算服务端故障」由表的 fault 列定:存储失败用 common 段的 kServiceUnavailable(fault=1,挂起排空证明),
// 渠道或商城这一次没答上来用 kPaymentBusy(不是故障:本服务没有结局未知的写入,玩家稍后重试即可,告警看支付服务自己的指标)。
package constants

import (
	"shared/generated/pb/table"
	"shared/serverbase"
)

const (
	ErrInvalidParameter    = uint32(table.CommonError_kInvalidParameter)
	ErrServiceUnavailable  = uint32(table.CommonError_kServiceUnavailable)
	ErrChannelUnavailable  = uint32(table.PaymentError_kPaymentChannelUnavailable)
	ErrOrderNotFound       = uint32(table.PaymentError_kPaymentOrderNotFound)
	ErrOrderNotPayable     = uint32(table.PaymentError_kPaymentOrderNotPayable)
	ErrOrderExpired        = uint32(table.PaymentError_kPaymentOrderExpired)
	ErrChannelMismatch     = uint32(table.PaymentError_kPaymentChannelMismatch)
	ErrCurrencyUnsupported = uint32(table.PaymentError_kPaymentCurrencyUnsupported)
	ErrBusy                = uint32(table.PaymentError_kPaymentBusy)
	ErrLoginRequired       = uint32(table.PaymentError_kPaymentLoginRequired)
)

// TipClassifier 是本服务的 in-band 业务码定性函数,定性全部来自 Tip.xlsx 的 fault 列。
func TipClassifier() serverbase.Classifier { return serverbase.TipVerdict }
