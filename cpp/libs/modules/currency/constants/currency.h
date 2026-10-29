#pragma once

#include <cstdint>

#include "proto/common/base/currency_type.pb.h"

// 币种取值表的唯一事实源是 proto/common/base/currency_type.proto（Go 侧 shared/currencytype 读同一份）。
// 这里只给 C++ 调用方保留短名，数值全部由 proto 派生，不得手写数字。
// 新增币种：先在 proto 里加值；需要按名字引用时，再在这里加一行短名。
enum CurrencyType : uint32_t
{
    kCurrencyGold = static_cast<uint32_t>(common::base::CURRENCY_TYPE_GOLD),
    kCurrencyDiamond = static_cast<uint32_t>(common::base::CURRENCY_TYPE_DIAMOND),
    kCurrencyBindDiamond = static_cast<uint32_t>(common::base::CURRENCY_TYPE_BIND_DIAMOND),
    kCurrencyMax = static_cast<uint32_t>(common::base::CurrencyType_ARRAYSIZE)
};

// CurrencyComp.values 以币种为下标，kCurrencyMax 当上限用，前提是取值从 0 连续。
// 连续性本身由 currency_test 的 CurrencyTypeEnumIsDerivedFromTheProtoTable 对照描述符守住。
static_assert(common::base::CurrencyType_MIN == 0, "currency_type.proto 的取值必须从 0 开始");

// 本二进制是否认识这个币种：唯一判据，所有入口都调它，不得再写本地副本。
// 纯判定、零副作用、不碰 ECS，可以排在任何业务条件之前。
// 外部载荷（收件箱、资产通道）遇到不认识的值时，Go 入口已按同一张取值表拒绝写错的值，
// 所以只可能是更新版本加的币种：那两处按版本错位处理（混版本持久字段保护设计 §12）；
// 内部原语、配置校验、商人仍按非法参数处理。
[[nodiscard]] constexpr bool IsKnownCurrencyType(uint32_t type)
{
    return type < static_cast<uint32_t>(kCurrencyMax);
}
