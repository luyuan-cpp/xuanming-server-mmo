#pragma once

// 官方灵石百宝阁(docs/design/spirit-shop-and-starter-packs-v1.md):
// 承接真钱充值灵石的官方绑定货架(首套 300 份共享限购提速包、定星保底符、天工秘墨、角色形象卡、改名卡)。
// 线程:只在 scene 主 EventLoop 线程调用,无全局可变状态。

#include <cstdint>

#include "entt/src/entt/entity/entity.hpp"

class SpiritShopInfo;
class BuySpiritShopGoodsRequest;
class BuySpiritShopGoodsResponse;

class PlayerSpiritShopSystem
{
public:
	// 启动与表热更后校验 SpiritShopLimitGroup / SpiritShopGoods 配置行。
	// 非法行打英文 ERROR 日志并在运行期拒绝上架与购买(fail-closed,不崩服)。
	static bool ValidateTables();

	// 填充百宝阁全量视图(货架、双余额、个人与共享组限购状态)。只读,绝不创建组件。
	static void FillInfo(entt::entity player, uint64_t nowMs, ::SpiritShopInfo &out);

	// 查询入口(GetSpiritShopInfo):校验实体与持久限购组件合法性,缺失/损坏返回 kSpiritShopUnavailable(对齐 Java 服)。
	static uint32_t GetInfo(entt::entity player, uint64_t nowMs, ::SpiritShopInfo &out);

	// 购买官方灵石百宝阁商品:
	// 幂等查重 → 可写/战斗闸 → 查表与报价防呆 → 限购/共享组预检 → 封禁/余额/背包空间规划 →
	// ExchangeInventoryExact 原子发绑定物并在 beforeCommit 同步扣灵石、记限购、记幂等 → 触发关键进度存盘。
	// 返回 kSuccess 表示成交(含重放);其他错误码保证零扣灵石、零发物、零占名额。
	static uint32_t Buy(entt::entity player, const ::BuySpiritShopGoodsRequest &request, uint64_t nowMs,
						::BuySpiritShopGoodsResponse &response);
};
