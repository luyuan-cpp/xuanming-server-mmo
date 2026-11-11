package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;

import java.nio.charset.StandardCharsets;
import java.util.List;
import org.junit.jupiter.api.Test;

/** 验证付款与交付冻结的是原账号/修订；过户与A→B→A不能借同一player_id接收旧资产。 */
class OfficialOrderOwnershipTest {
    private static final String ORIGINAL = " 原账号 ";
    private static final String MAX_REVISION = "18446744073709551615";
    private static CommerceApi.CreateOrder create(String key, String account, String revision) {
        return new CommerceApi.CreateOrder(key, 1, 1, account, revision);
    }
    private static CommerceApi.OwnedOrder identity(String orderId, String account, String revision) {
        return new CommerceApi.OwnedOrder(orderId, account, revision);
    }
    private static byte[] bytes(String text) { return text.getBytes(StandardCharsets.UTF_8); }

    @Test void 相同业务请求只重放完整原身份_过户与ABA不能换原合同() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 10, 100);
            var original = create("official-original-key", ORIGINAL, "7");
            var order = f.store.create("91", original);
            assertEquals(order, f.store.create("91", original));
            for (var changed : List.of(create(original.requestId(), "新账号", "8"),
                    create(original.requestId(), ORIGINAL, "9"), create(original.requestId(), ORIGINAL.trim(), "7"))) {
                assertEquals(409, assertThrows(CommerceException.class, () -> f.store.create("91", changed)).status());
            }
            assertEquals(1, f.count("oms_order")); assertEquals(1, f.count("commerce_official_owner"));
            assertEquals(1, f.value("pms_sku_stock", "lock_stock"));
            var stored = f.jdbc.queryForMap("SELECT buyer_account,buyer_owner_revision FROM commerce_official_owner WHERE order_sn=?", order.orderId());
            assertArrayEquals(bytes(ORIGINAL), (byte[])stored.get("buyer_account"));
            assertArrayEquals(bytes("7"), (byte[])stored.get("buyer_owner_revision"));
        }
    }

    @Test void 原账号历史查询需原修订_角色相同不授权新主或ABA_旧无身份入口拒绝() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 10, 100);
            var order = f.store.create("91", create("official-owned-query", ORIGINAL, "7"));
            var owned = identity(order.orderId(), ORIGINAL, "7");
            assertEquals(order, f.store.get("91", owned));
            for (var foreign : List.of(identity(order.orderId(), "新账号", "8"),
                    identity(order.orderId(), ORIGINAL, "9"), identity(order.orderId(), ORIGINAL.trim(), "7"))) {
                assertEquals(404, assertThrows(CommerceException.class, () -> f.store.get("91", foreign)).status());
                assertEquals(404, assertThrows(CommerceException.class, () -> f.store.cancel("91", foreign)).status());
                assertTrue(f.store.list("91", new DeliveryApi.OrderPage("", 20, foreign.buyerAccount(), foreign.buyerOwnerRevision())).isEmpty());
            }
            assertEquals(404, assertThrows(CommerceException.class, () -> f.store.get("91", order.orderId())).status());
            assertEquals(404, assertThrows(CommerceException.class, () -> f.store.cancel("91", order.orderId())).status());
            assertEquals(404, assertThrows(CommerceException.class, () -> f.store.get("92", owned)).status());
            assertEquals(List.of(order), f.store.list("91", new DeliveryApi.OrderPage("", 20, ORIGINAL, "7")));
            assertEquals("CANCELLED", f.store.cancel("91", owned).state());
            assertEquals("CANCELLED", f.store.cancel("91", owned).state());
            assertEquals(0, f.value("pms_sku_stock", "lock_stock"));
        }
    }

    @Test void 完整uint64修订作为字符串冻结并进入稳定交付原件() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 10, 100);
            var input = create("official-full-uint64", ORIGINAL, MAX_REVISION);
            var order = f.store.create("18446744073709551615", input);
            assertEquals("AWAITING_PAYMENT", order.state());
            var paid = f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(), "test", "official-max-payment", 100, "CNY")).order();
            // 原身份、金额和期限不变；查询及原请求重放应反映已提交支付后的当前状态。
            assertEquals(new CommerceApi.Order(order.orderId(), order.buyerId(), order.skuId(), order.quantity(),
                order.amountMinor(), order.currency(), "DELIVERY_PENDING", order.paymentDeadlineMs()), paid);
            var delivery = new DeliveryStore(f.jdbc, f.manager, f.props);
            var intent = delivery.poll(20).getFirst();
            assertEquals(order.orderId(), intent.orderId()); assertEquals(order.buyerId(), intent.buyerId());
            assertEquals(ORIGINAL, intent.buyerAccount()); assertEquals(MAX_REVISION, intent.buyerOwnerRevision());
            f.jdbc.update("UPDATE commerce_sku_grant SET title='后来标题',item_config_id=99 WHERE sku_id=1");
            assertEquals(intent, delivery.poll(20).getFirst());
            assertEquals(paid, f.store.get(order.buyerId(), identity(order.orderId(), ORIGINAL, MAX_REVISION)));
            assertEquals(paid, f.store.create(order.buyerId(), input));
            assertEquals(1, f.count("oms_order"));
            assertEquals(1, f.count("commerce_payment_receipt"));
            assertEquals(1, f.count("commerce_delivery_intent"));
            assertEquals(9, f.value("pms_sku_stock", "stock"));
            assertEquals(0, f.value("pms_sku_stock", "lock_stock"));
        }
    }

    @Test void 缺失或非法原身份与SKU失败都不能留下半笔订单或预占() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 10, 100);
            for (String revision : new String[]{null, "", "0", "01", "+1", "1 ", " 1", "-1", "18446744073709551616", "1.0"}) {
                assertEquals(400, assertThrows(CommerceException.class, () -> f.store.create("91", create("invalid-owner-key", ORIGINAL, revision))).status());
            }
            for (String account : new String[]{null, "", "x".repeat(192), "\uD800"}) {
                assertEquals(400, assertThrows(CommerceException.class, () -> f.store.create("91", create("invalid-owner-key", account, "7"))).status());
            }
            f.jdbc.update("DELETE FROM commerce_sku_grant WHERE sku_id=1");
            assertEquals(409, assertThrows(CommerceException.class, () -> f.store.create("91", create("missing-grant-key", ORIGINAL, "7"))).status());
            assertEquals(0, f.count("oms_order")); assertEquals(0, f.count("commerce_official_owner"));
            assertEquals(0, f.value("pms_sku_stock", "lock_stock"));
        }
    }

    @Test void 丢失原件时付款和交付拒绝_不从角色当前归属或旧缓存补权() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 10, 100);
            var order = f.store.create("91", create("missing-origin-before-pay", ORIGINAL, "7"));
            f.jdbc.update("DELETE FROM commerce_official_owner WHERE order_sn=?", order.orderId());
            assertEquals(503, assertThrows(CommerceException.class, () -> f.store.recordTrustedPayment(
                new CommerceApi.Payment(order.orderId(), "test", "missing-origin-pay", 100, "CNY"))).status());
            assertEquals(0, f.count("commerce_payment_receipt")); assertEquals(0, f.count("commerce_delivery_intent"));
        }
        try (var f = new CommerceFixture()) {
            f.sku(1, 10, 100);
            var order = f.store.create("91", create("missing-origin-after-pay", ORIGINAL, "7"));
            f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(), "test", "paid-before-origin-loss", 100, "CNY"));
            f.jdbc.update("DELETE FROM commerce_official_owner WHERE order_sn=?", order.orderId());
            var delivery = new DeliveryStore(f.jdbc, f.manager, f.props);
            assertEquals(503, assertThrows(CommerceException.class, () -> delivery.poll(20)).status());
            assertEquals(503, assertThrows(CommerceException.class, () -> delivery.receipt(
                new DeliveryApi.Receipt(order.orderId(), "991", "DELIVERED", "a".repeat(64)))).status());
            assertEquals(0, f.count("commerce_delivery_receipt"));
            assertEquals(1, f.jdbc.queryForObject("SELECT status FROM oms_order WHERE order_sn=?", Integer.class, order.orderId()));
        }
    }

    @Test void 原身份Schema缺表和损坏修订在交付接口明确拒绝() throws Exception {
        try (var f = new CommerceFixture()) {
            f.sku(1, 10, 100);
            var order = f.store.create("91", create("corrupt-origin-revision", ORIGINAL, "7"));
            f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(), "test", "corrupt-origin-pay", 100, "CNY"));
            f.jdbc.update("UPDATE commerce_official_owner SET buyer_owner_revision=? WHERE order_sn=?", bytes("07"), order.orderId());
            assertEquals(503, assertThrows(CommerceException.class, () -> new DeliveryStore(f.jdbc, f.manager, f.props).poll(20)).status());
        }
        try (var f = new CommerceFixture()) {
            f.jdbc.execute("DROP TABLE commerce_official_owner");
            assertThrows(Exception.class, () -> SchemaVerifier.verify(f.jdbc, f.source));
        }
    }
}
