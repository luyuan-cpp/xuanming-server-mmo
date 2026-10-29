// Package currencytype 是 Go 侧读币种取值表的唯一入口。取值表的事实源是
// proto/common/base/currency_type.proto,C++ 的 currency.h 由同一份派生。
//
// 为什么单独成包:旧 scene 遇到不认识的币种会判成版本错位(收件箱停用本域、资产通道回 RETRY,
// 不记终态),前提是每个能把 currency_type 送进 scene 的 Go 入口都按完整取值表拒绝写错的值。
// 各服务各抄一份上限(">2"、"<=1")时,旧 scene 分不出"新币种"和"写错的币种",
// 写错的值会让 GM 处置域无限期停用、资产操作永久 RETRY(混版本持久字段保护设计 §12)。
//
// 包名不用 currency:调用方普遍有名为 currency 的局部变量,会遮蔽包名。
//
// 业务白名单(例如帮会捐献只收银两和灵石)只能点名这里的常量,不得写数字:
// 扩白名单时不可能写出表外的值。
package currencytype

import (
	"math"

	basepb "proto/common/base"
)

// 按名字引用的币种。新增币种先在 proto 里加值;需要按名字引用时再在这里加一行。
const (
	Gold        = uint32(basepb.CurrencyType_CURRENCY_TYPE_GOLD)         // 银两
	Diamond     = uint32(basepb.CurrencyType_CURRENCY_TYPE_DIAMOND)      // 灵石
	BindDiamond = uint32(basepb.CurrencyType_CURRENCY_TYPE_BIND_DIAMOND) // 绑定灵石
)

// IsKnown 报告 v 是否在取值表内。线上字段是 uint32,枚举是 int32:先排除转换后会变号的值。
// 取值从 0 连续(由本包测试与 C++ currency_test 守住)时,与 C++ 的 IsKnownCurrencyType 等价。
func IsKnown(v uint32) bool {
	if v > math.MaxInt32 {
		return false
	}
	_, ok := basepb.CurrencyType_name[int32(v)]
	return ok
}
