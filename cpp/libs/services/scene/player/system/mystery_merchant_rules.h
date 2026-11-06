#pragma once

// 神秘商人的纯规则:折后价、时间窗、下一次出现时刻、距离判定、限购记账。
// 设计文档 docs/design/mystery-merchant.md §5。
//
// 这里的每个函数都不碰 ECS、不读配置表、不取时间、不掷随机数 —— 输入全部来自参数。
// 这样「价格怎么算、名额怎么记」只有这一个出处(下发给客户端的价格与结算时扣的价格调的是同一个函数),
// 而且可以脱离任何夹具直接单测(cpp/tests/bag_test/player_mystery_merchant_test.cpp)。
// 编排(读缓存、过闸、扣钱、发物)在 player_mystery_merchant.cpp。

#include <cstdint>
#include <limits>

#include "proto/common/component/player_mystery_merchant_comp.pb.h"
#include "proto/scene/player_mystery_merchant.pb.h"

namespace merchantrules
{

// 单次购买的份数上限。与 MysteryMerchantGoods.limit_count 的上限(99)同值:
// 一次最多把一件商品的整轮名额买完。
inline constexpr uint32_t kMaxBuyCountPerRequest = 99;

// 折扣百分比的合法范围(1..100)。具体有哪几档(60 / 50 / 40)由 scene_manager 掷出,
// 这里只拒绝「不是折扣」的值:0 会让东西白送,大于 100 是涨价。
inline constexpr uint32_t kMinDiscountPercent = 1;
inline constexpr uint32_t kMaxDiscountPercent = 100;

// 原价的上限,与 data/schema/mysterymerchantgoods_table.proto 的 price 列、
// go/scene_manager 的 merchantMaxGoodsPrice 是同一个约定:保证「折后价 × 99 份」远小于 int64 上限。
inline constexpr uint64_t kMaxGoodsPrice = 1'000'000'000ULL;

inline bool IsValidDiscountPercent(uint32_t percent)
{
	return percent >= kMinDiscountPercent && percent <= kMaxDiscountPercent;
}

// 折后价 = ceil(原价 × 百分比 / 100)。向上取整:原价 1 的东西打 4 折仍是 1,任何合法输入都不会算出 0。
// 前置条件:price <= kMaxGoodsPrice 且 IsValidDiscountPercent(percent)(调用方先校验;乘法因此不会溢出)。
inline uint64_t DiscountedPrice(uint64_t price, uint32_t percent)
{
	return (price * percent + 99) / 100;
}

// 一次购买的总价 = 每份折后价 × 份数,写进 out。溢出 int64(货币系统的金额类型)时返回 false。
// 在合法配表下不可能溢出;保留这道检查是为了让「配表被改坏」表现为拒绝交易,而不是扣一个回绕后的数。
inline bool TotalCost(uint64_t unitPrice, uint32_t count, int64_t &out)
{
	const auto limit = static_cast<uint64_t>(std::numeric_limits<int64_t>::max());
	if (count == 0 || unitPrice == 0 || unitPrice > limit / count)
	{
		return false;
	}
	out = static_cast<int64_t>(unitPrice * count);
	return true;
}

// 商人此刻是否在场:nowMs 落在 [appear_at_ms, expire_at_ms)。
// 离开时刻不含:expire_at_ms 那一毫秒起就不能再买。
inline bool IsPresent(const ::MysteryMerchantState &state, uint64_t nowMs)
{
	return nowMs >= state.appear_at_ms() && nowMs < state.expire_at_ms();
}

// 刚过整点的宽限期(毫秒)。新的一轮从整点开始,但 scene 节点要等 scene_manager 排好(每 5 秒一拍)、自己再读到
// (每 5 秒一次)才知道,中间有十来秒手里还是上一轮的记录。这段时间里「下一次出现」应当答「就是这个整点」,
// 而不是再下一个整点 —— 否则恰好这时进场的玩家会看到「59 分 50 秒后出现」,几秒后商人却到了。
inline constexpr uint64_t kNextAppearGraceMs = 30000;

// 下一次出现的时刻(毫秒),按 state 那一轮的出现时刻与间隔往后推:
//   - 通常是严格晚于 nowMs 的第一个整间隔点;
//   - 但 nowMs 刚越过某个整间隔点不到 kNextAppearGraceMs(手里还是上一轮的记录)时,返回刚越过的那个点
//     (它不晚于 nowMs,客户端显示成「即将出现」并在稍后自己重新查询)。
// interval_seconds 为 0(坏记录)时返回 0 = 未知。nowMs 早于 appear_at_ms(时钟回拨)时返回 appear_at_ms。
inline uint64_t NextAppearAtMs(const ::MysteryMerchantState &state, uint64_t nowMs)
{
	const uint64_t intervalMs = static_cast<uint64_t>(state.interval_seconds()) * 1000;
	if (intervalMs == 0)
	{
		return 0;
	}
	if (nowMs < state.appear_at_ms())
	{
		return state.appear_at_ms();
	}
	const uint64_t roundsElapsed = (nowMs - state.appear_at_ms()) / intervalMs;
	const uint64_t lastBoundary = state.appear_at_ms() + roundsElapsed * intervalMs;
	if (roundsElapsed > 0 && nowMs - lastBoundary < kNextAppearGraceMs)
	{
		return lastBoundary;
	}
	return lastBoundary + intervalMs;
}

// 玩家是否站在商人的交互范围内。只比水平面上的距离(服务器坐标的 x / y;z 是高度,
// 同一张图里站位与玩家脚底的高度差不该让人买不了东西)。边界含:恰好等于 range 算在范围内。
inline bool WithinInteractRange(double playerX, double playerY, double merchantX, double merchantY, uint32_t range)
{
	const double dx = playerX - merchantX;
	const double dy = playerY - merchantY;
	const double limit = static_cast<double>(range);
	return dx * dx + dy * dy <= limit * limit;
}

// 在货架上找一件商品;不在架上返回 nullptr。
inline const ::MysteryMerchantShelfEntry *FindShelfEntry(const ::MysteryMerchantState &state, uint32_t goodsId)
{
	for (const auto &entry : state.shelf())
	{
		if (entry.goods_id() == goodsId)
		{
			return &entry;
		}
	}
	return nullptr;
}

// 玩家在 roundId 这一轮已经买了几份 goodsId。记录属于别的轮次时一律是 0(整份记录已作废)。
inline uint32_t BoughtCount(const ::PlayerMysteryMerchantComp &record, uint64_t roundId, uint32_t goodsId)
{
	if (record.round_id() != roundId)
	{
		return 0;
	}
	for (const auto &purchase : record.purchases())
	{
		if (purchase.goods_id() == goodsId)
		{
			return purchase.count();
		}
	}
	return 0;
}

// 再买 count 份会不会超过限购。写成减法是为了不让 bought + count 回绕。
inline bool WouldExceedLimit(uint32_t bought, uint32_t count, uint32_t limit)
{
	return bought >= limit || count > limit - bought;
}

// 把一次成功的购买记进限购记录。记录属于别的轮次时先整份清空再记 —— 这就是「每轮重置」的全部实现。
// 调用方保证已经用 WouldExceedLimit 核过限购(本函数不再核,加法因此不会回绕)。
inline void RecordPurchase(::PlayerMysteryMerchantComp &record, uint64_t roundId, uint32_t goodsId, uint32_t count)
{
	if (record.round_id() != roundId)
	{
		// 换轮是逻辑重置:整条 Clear() 再重设轮次(docs/design/mixed-version-persist-field-protection.md §4.2 L7)。
		// 逐字段清空会漏掉本版本不认识的字段 —— 更新版本加的按轮作废的字段会跨轮存活,把上一轮的状态带进新一轮。
		// 需要跨轮保留的新字段在本版本会被清掉,按破坏性变更处理(同文 R5)。
		record.Clear();
		record.set_round_id(roundId);
	}
	for (auto &purchase : *record.mutable_purchases())
	{
		if (purchase.goods_id() == goodsId)
		{
			purchase.set_count(purchase.count() + count);
			return;
		}
	}
	auto *purchase = record.add_purchases();
	purchase->set_goods_id(goodsId);
	purchase->set_count(count);
}

// 记录的新旧比较键:round_id × 1000 + revision,与 go/scene_manager 写入脚本用的版本号同一个算法
// (mystery_merchant.go 的 mysteryMerchantVersionRoundFactor)。round_id 是那一轮出现时刻的 Unix 秒,
// 所以这个键只跟墙钟走。scene 节点用它判断「这是不是一份没见过的记录」(决定要不要发出现公告),
// 以及丢弃迟到的旧记录(见 ShouldDiscardAsStale)。
inline uint64_t StateVersion(const ::MysteryMerchantState &state)
{
	return state.round_id() * 1000 + state.revision();
}

// 新读到的 incoming 是不是一份该丢弃的旧记录(cached 是手里那份)。
//
// 只有「手里那一轮还没结束」时才丢弃版本更旧的:这道保护防的是 Redis 主从切换丢了最近的写、
// 读回同一轮更早的落位 —— 那时退回去会让同一轮出现两条线。
// 手里那一轮已经结束,它就没有保护价值了,读到什么都采纳。这个出口不能省:scene_manager 在旧记录损坏时会强制覆盖出
// 一份版本号更小的记录,没有出口的话本节点会永久拒收它,只能重启。改了出现间隔不会走到这里:scene_manager 让旧的
// 那一轮卖完,新间隔下只写轮次号比它大的一轮(mystery_merchant.go scheduleMysteryMerchant,第二轮审计 r2-mm#1)。
inline bool ShouldDiscardAsStale(const ::MysteryMerchantState &incoming, const ::MysteryMerchantState &cached,
								 uint64_t nowMs)
{
	return nowMs < cached.expire_at_ms() && StateVersion(incoming) < StateVersion(cached);
}

} // namespace merchantrules
