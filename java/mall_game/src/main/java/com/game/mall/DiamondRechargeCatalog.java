package com.game.mall;

import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Set;

/**
 * 官方通用灵石充值标准 7 档目录(docs/design/payment-recharge.md §4 & docs/planning/spirit-shop-and-starter-packs-v1.md §2.1)。
 * 仅提供目录定义与显式幂等补齐入口，不在启动时自动改库(遵守 spring.sql.init.mode=never)。
 */
public final class DiamondRechargeCatalog {
    /** proto/common/base/currency_type.proto: CURRENCY_TYPE_DIAMOND = 1(真实充值灵石，严禁填绑定灵石 2)。 */
    public static final int CURRENCY_TYPE_DIAMOND = 1;
    /** 充值档位逻辑无限库存(低于 Integer.MAX_VALUE，供 CommerceStore / DeliveryStore 现有库存守卫放行)。 */
    public static final int DEFAULT_RECHARGE_STOCK = 1_000_000_000;
    public static final String CURRENCY_CNY = "CNY";

    public record Tier(long skuId, long productId, String skuCode, long priceMinor, long diamondAmount, String title) {
        public CatalogAdminApi.Create toCreateCommand() {
            var terms = new CatalogAdminApi.Terms(
                productId, skuCode, priceMinor, CURRENCY_CNY, true,
                0, 0, title, CURRENCY_TYPE_DIAMOND, diamondAmount);
            return new CatalogAdminApi.Create("seed-recharge-" + skuCode, skuId, terms, DEFAULT_RECHARGE_STOCK);
        }
    }

    /** 标准 7 档：含 ¥18 = 180 灵石(diamond_180)，与百宝阁中阶提速包/天工秘墨/技能特效卷 1:1 对齐。 */
    public static final List<Tier> STANDARD_TIERS = List.of(
        new Tier(1001L, 1001L, "diamond_60", 600L, 60L, "60灵石"),
        new Tier(1002L, 1002L, "diamond_180", 1800L, 180L, "180灵石"),
        new Tier(1003L, 1003L, "diamond_300", 3000L, 300L, "300灵石"),
        new Tier(1004L, 1004L, "diamond_680", 6800L, 680L, "680灵石"),
        new Tier(1005L, 1005L, "diamond_1280", 12800L, 1280L, "1280灵石"),
        new Tier(1006L, 1006L, "diamond_3280", 32800L, 3280L, "3280灵石"),
        new Tier(1007L, 1007L, "diamond_6480", 64800L, 6480L, "6480灵石")
    );

    private DiamondRechargeCatalog() {}

    /** 按 skuId 增量补齐尚未入库的标准灵石充值档位；已有 SKU 原样保留不覆盖。返回本次新建的 SKU 列表。 */
    public static List<CatalogAdminApi.Sku> seedMissing(CatalogAdminStore store, String actor) {
        Set<Long> existing = new HashSet<>();
        long after = 0L;
        while (true) {
            var page = store.list(new DeliveryApi.SkuPage(after, 100)).skus();
            if (page.isEmpty()) {
                break;
            }
            for (var sku : page) {
                existing.add(sku.skuId());
                after = Math.max(after, sku.skuId());
            }
            if (page.size() < 100) {
                break;
            }
        }
        List<CatalogAdminApi.Sku> created = new ArrayList<>();
        for (var tier : STANDARD_TIERS) {
            if (!existing.contains(tier.skuId())) {
                created.add(store.create(actor, tier.toCreateCommand()));
            }
        }
        return created;
    }
}
