package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;

import com.fasterxml.jackson.databind.ObjectMapper;
import java.time.Clock;
import java.time.Instant;
import java.time.ZoneOffset;
import org.junit.jupiter.api.Test;

/** 核对官方灵石充值标准 7 档(含 ¥18=180 灵石 diamond_180)的建档、幂等补齐与下单冻结。 */
class DiamondRechargeCatalogTest {
    private static CatalogAdminStore admin(CommerceFixture f) {
        return new CatalogAdminStore(f.jdbc, f.manager, new ObjectMapper(),
            Clock.fixed(Instant.ofEpochMilli(1_760_000_000_000L), ZoneOffset.UTC));
    }

    @Test void 标准七档含十八元档_可经管理接口幂等补齐并正常下单冻结() {
        try (var f = new CommerceFixture()) {
            var catalog = admin(f);
            var created = DiamondRechargeCatalog.seedMissing(catalog, "mall-admin");
            assertEquals(7, created.size());
            assertTrue(DiamondRechargeCatalog.seedMissing(catalog, "mall-admin").isEmpty(), "重复补齐不改已有SKU");

            var tier180 = created.stream().filter(s -> s.skuId() == 1002L).findFirst().orElseThrow();
            assertEquals("diamond_180", tier180.skuCode());
            assertEquals(1800L, tier180.priceMinor());
            assertEquals("CNY", tier180.currency());
            assertEquals(0, tier180.itemConfigId());
            assertEquals(0, tier180.countPerUnit());
            assertEquals(DiamondRechargeCatalog.CURRENCY_TYPE_DIAMOND, tier180.currencyType());
            assertEquals(180L, tier180.currencyAmount());
            assertTrue(tier180.published());

            var visible = new DeliveryStore(f.jdbc, f.manager, f.props).skus().skus();
            assertEquals(7, visible.size());
            assertTrue(visible.stream().anyMatch(s -> s.skuId() == 1002L && s.priceMinor() == 1800L && "180灵石".equals(s.title())));

            var order = f.store.create("10001", new CommerceApi.CreateOrder("buy-diamond-180-001", 1002L, 1, "测试账号", "1"));
            assertEquals(1800L, order.amountMinor());
            assertEquals("CNY", order.currency());
            var grant = f.jdbc.queryForMap(
                "SELECT item_config_id,count_per_unit,currency_type,currency_amount,title FROM commerce_order_grant WHERE order_sn=?",
                order.orderId());
            assertEquals(0, ((Number) grant.get("item_config_id")).intValue());
            assertEquals(0, ((Number) grant.get("count_per_unit")).intValue());
            assertEquals(1, ((Number) grant.get("currency_type")).intValue());
            assertEquals(180L, ((Number) grant.get("currency_amount")).longValue());
            assertEquals("180灵石", grant.get("title"));
        }
    }

    @Test void 已有旧六档时只增量补齐十八元档且不覆写已有修订() {
        try (var f = new CommerceFixture()) {
            var catalog = admin(f);
            for (var tier : DiamondRechargeCatalog.STANDARD_TIERS) {
                if (tier.skuId() != 1002L) {
                    catalog.create("mall-admin", tier.toCreateCommand());
                }
            }
            var added = DiamondRechargeCatalog.seedMissing(catalog, "mall-admin");
            assertEquals(1, added.size());
            assertEquals(1002L, added.getFirst().skuId());
            assertEquals("diamond_180", added.getFirst().skuCode());
            assertEquals(1800L, added.getFirst().priceMinor());
            assertEquals(180L, added.getFirst().currencyAmount());
        }
    }
}
