package com.game.mall;

import java.util.List;

/** 运维命令只修改未来订单条款；不接受支付、交付状态或玩家资产。 */
public final class CatalogAdminApi {
    private CatalogAdminApi() {}

    /**
     * SKU 条款。发放物二选一:物品(itemConfigId × countPerUnit,每份 1..4096 件)或货币(currencyType × currencyAmount,
     * 例如充值灵石包,每份至多 DeliveryStore.MAX_CURRENCY_GRANT),另一种全为 0。货币种类 0 是银两,有没有货币看数量。
     */
    public record Terms(long productId, String skuCode, long priceMinor, String currency, boolean published,
        int itemConfigId, int countPerUnit, String title, int currencyType, long currencyAmount) {
        /** 物品发放的条款(货币字段为 0)。 */
        public Terms(long productId, String skuCode, long priceMinor, String currency, boolean published,
            int itemConfigId, int countPerUnit, String title) {
            this(productId, skuCode, priceMinor, currency, published, itemConfigId, countPerUnit, title, 0, 0);
        }
    }
    public record Create(String requestId, long skuId, Terms terms, int stock) {}
    public record Update(String requestId, long skuId, long expectedRevision, Terms terms) {}
    public record AdjustStock(String requestId, long skuId, long expectedRevision, int delta) {}
    public record Sku(long skuId, long revision, long productId, String skuCode, long priceMinor, String currency,
        int stock, int lockedStock, boolean published, int itemConfigId, int countPerUnit, String title,
        int currencyType, long currencyAmount) {
        /** 物品发放的 SKU(货币字段为 0)。 */
        public Sku(long skuId, long revision, long productId, String skuCode, long priceMinor, String currency,
            int stock, int lockedStock, boolean published, int itemConfigId, int countPerUnit, String title) {
            this(skuId, revision, productId, skuCode, priceMinor, currency, stock, lockedStock, published, itemConfigId, countPerUnit, title, 0, 0);
        }
        Terms terms() {
            return new Terms(productId, skuCode, priceMinor, currency, published, itemConfigId, countPerUnit, title, currencyType, currencyAmount);
        }
    }
    public record Skus(List<Sku> skus) {}
}
