package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;

import java.time.Clock;
import java.time.Instant;
import java.time.ZoneOffset;
import java.util.List;
import org.junit.jupiter.api.Test;

/** 付款修订、扣回待办与回执、退款转人工(docs/design/payment-recharge.md §22.6)。 */
class PaymentRevisionStoreTest {
    private static final String PROOF_A = "a".repeat(64), PROOF_B = "b".repeat(64);

    private static PaymentRevisionStore revisions(CommerceFixture f) {
        return new PaymentRevisionStore(f.jdbc, f.manager, Clock.fixed(Instant.ofEpochMilli(1_760_000_100_000L), ZoneOffset.UTC), f.props);
    }
    private static CommerceApi.Order paidOrder(CommerceFixture f, String request, String transaction) {
        var order = f.store.create("1", new CommerceApi.CreateOrder(request, 1, 1, "测试账号", "1"));
        f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(), "test", transaction, order.amountMinor(), order.currency()));
        return order;
    }
    private static String state(CommerceFixture f, CommerceApi.Order order) {
        return f.store.get("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")).state();
    }
    private static List<String> refundIntent(CommerceFixture f, String transaction) {
        return f.jdbc.query("SELECT status,close_reason FROM commerce_refund_intent WHERE transaction_id=?",
            (r, n) -> java.util.Arrays.asList(r.getString(1), r.getString(2)), transaction.getBytes(java.nio.charset.StandardCharsets.US_ASCII)).getFirst();
    }

    @Test void 撤销已绑定付款_订单转扣回中_交付回执只了结待办_扣回回执推进订单() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 10, 100);
            var order = paidOrder(f, "revoke-order-001", "revoke-pay-001");
            var store = revisions(f);
            var delivery = new DeliveryStore(f.jdbc, f.manager, f.props);
            var deliveryIntent = delivery.poll(new DeliveryApi.Poll(20, "")).entries().getFirst();
            assertEquals(0, deliveryIntent.paymentRevision());

            var outcome = store.record(new CommerceApi.Revision(order.orderId(), "test", "revoke-pay-001", 1, "REVOKED"));
            assertEquals(new CommerceApi.RevisionOutcome(1, "REVOKED", "REVERSING"), outcome);
            assertEquals(outcome, store.record(new CommerceApi.Revision(order.orderId(), "test", "revoke-pay-001", 1, "REVOKED")), "同一修订号重放回当前值");
            assertEquals("REVERSING", state(f, order));

            var page = delivery.reversals(new DeliveryApi.Poll(20, ""));
            assertEquals(1, page.entries().size());
            var entry = page.entries().getFirst();
            assertEquals(1, entry.revision());
            assertFalse(entry.entitled(), "奇数修订号 = 买家不该持有");
            assertEquals(deliveryIntent.intent(), entry.intent(), "扣回待办与交付待办给 trade 同一份订单条款");
            assertEquals(1, delivery.poll(new DeliveryApi.Poll(20, "")).entries().getFirst().paymentRevision(), "交付待办带上最新修订号");

            delivery.receipt(new DeliveryApi.Receipt(order.orderId(), "123", "VOIDED", PROOF_A));
            assertEquals("REVERSING", state(f, order), "有扣回待办时交付回执不推进订单");
            assertEquals("VOIDED", f.jdbc.queryForObject("SELECT status FROM commerce_delivery_intent WHERE order_sn=?", String.class, order.orderId()));

            assertEquals(400, assertThrows(CommerceException.class, () -> delivery.reversalReceipt(
                new DeliveryApi.ReversalReceipt(order.orderId(), 1, "123", "DELIVERED", PROOF_B))).status(), "撤销的修订号不能回执已交付");
            var receipt = new DeliveryApi.ReversalReceipt(order.orderId(), 1, "123", "REVERSED", PROOF_B);
            delivery.reversalReceipt(receipt);
            delivery.reversalReceipt(receipt);
            assertEquals(409, assertThrows(CommerceException.class, () -> delivery.reversalReceipt(
                new DeliveryApi.ReversalReceipt(order.orderId(), 1, "123", "MANUAL", PROOF_B))).status(), "同一修订号换结论");
            assertEquals("REVERSED", state(f, order));
            assertTrue(delivery.reversals(new DeliveryApi.Poll(20, "")).entries().isEmpty());
            assertEquals(new CommerceApi.RevisionOutcome(1, "REVOKED", "REVERSED"),
                store.record(new CommerceApi.Revision(order.orderId(), "test", "revoke-pay-001", 1, "REVOKED")));
        }
    }

    @Test void 撤销又撤回_旧修订回执只留档_最新修订回执推进() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 10, 100);
            var order = paidOrder(f, "reinstate-order-001", "reinstate-pay-001");
            var store = revisions(f);
            var delivery = new DeliveryStore(f.jdbc, f.manager, f.props);
            delivery.receipt(new DeliveryApi.Receipt(order.orderId(), "123", "DELIVERED", PROOF_A));
            assertEquals("DELIVERED", state(f, order));

            store.record(new CommerceApi.Revision(order.orderId(), "test", "reinstate-pay-001", 1, "REVOKED"));
            assertEquals(new CommerceApi.RevisionOutcome(2, "PAID", "REVERSING"),
                store.record(new CommerceApi.Revision(order.orderId(), "test", "reinstate-pay-001", 2, "PAID")));
            delivery.reversalReceipt(new DeliveryApi.ReversalReceipt(order.orderId(), 1, "123", "REVERSED", PROOF_A));
            assertEquals("REVERSING", state(f, order), "旧修订号的回执只留档");
            var entry = delivery.reversals(new DeliveryApi.Poll(20, "")).entries().getFirst();
            assertEquals(2, entry.revision());
            assertTrue(entry.entitled());
            assertEquals(409, assertThrows(CommerceException.class, () -> delivery.reversalReceipt(
                new DeliveryApi.ReversalReceipt(order.orderId(), 3, "123", "REVERSED", PROOF_B))).status(), "不能回执商城没发出的修订号");
            delivery.reversalReceipt(new DeliveryApi.ReversalReceipt(order.orderId(), 2, "123", "DELIVERED", PROOF_B));
            assertEquals("DELIVERED", state(f, order));
            assertEquals(2, f.count("commerce_reversal_receipt"));
        }
    }

    @Test void 修订参数与归属校验() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 10, 100);
            var order = paidOrder(f, "check-order-001", "check-pay-001");
            var other = paidOrder(f, "check-order-002", "check-pay-002");
            var store = revisions(f);
            assertEquals(400, assertThrows(CommerceException.class, () -> store.record(
                new CommerceApi.Revision(order.orderId(), "test", "check-pay-001", 0, "PAID"))).status(), "修订号 0 是原始付款");
            assertEquals(400, assertThrows(CommerceException.class, () -> store.record(
                new CommerceApi.Revision(order.orderId(), "test", "check-pay-001", 1, "PAID"))).status(), "奇数修订号必须是撤销");
            assertEquals(409, assertThrows(CommerceException.class, () -> store.record(
                new CommerceApi.Revision(other.orderId(), "test", "check-pay-001", 1, "REVOKED"))).status(), "付款不属于这张订单");
            assertEquals(404, assertThrows(CommerceException.class, () -> store.record(
                new CommerceApi.Revision(order.orderId(), "test", "check-pay-unknown", 1, "REVOKED"))).status());
            assertEquals(new CommerceApi.RevisionOutcome(2, "PAID", "DELIVERY_PENDING"),
                store.record(new CommerceApi.Revision(order.orderId(), "test", "check-pay-001", 2, "PAID")),
                "付款 → 付款:中间的撤销与撤回商城没看见,发放不用变");
            assertEquals(0, f.count("commerce_reversal_intent"));
            assertEquals(new CommerceApi.RevisionOutcome(2, "PAID", "DELIVERY_PENDING"),
                store.record(new CommerceApi.Revision(order.orderId(), "test", "check-pay-001", 1, "REVOKED")), "过时的修订号盖不住新的");
        }
    }

    @Test void 取消后迟到的付款_撤销即不用再退_撤回撤销转人工() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 10, 100);
            var order = f.store.create("1", new CommerceApi.CreateOrder("late-order-001", 1, 1, "测试账号", "1"));
            f.store.cancel("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1"));
            f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(), "test", "late-pay-001", order.amountMinor(), order.currency()));
            assertEquals("REFUND_PENDING", state(f, order));
            var store = revisions(f);
            var delivery = new DeliveryStore(f.jdbc, f.manager, f.props);

            assertEquals(new CommerceApi.RevisionOutcome(1, "REVOKED", "REFUNDED"),
                store.record(new CommerceApi.Revision(order.orderId(), "test", "late-pay-001", 1, "REVOKED")));
            assertEquals(List.of("REVOKED", "CHANNEL_REVOKED"), refundIntent(f, "late-pay-001"));
            assertTrue(delivery.refunds().isEmpty(), "渠道已退给用户,不再列在待退款里");
            assertEquals(0, f.count("commerce_reversal_intent"), "有退款待办的付款不走扣回");

            assertEquals(new CommerceApi.RevisionOutcome(2, "PAID", "MANUAL"),
                store.record(new CommerceApi.Revision(order.orderId(), "test", "late-pay-001", 2, "PAID")));
            assertEquals(List.of("MANUAL", "REVOCATION_REVERSED"), refundIntent(f, "late-pay-001"), "钱又回到商户,由人决定再退一次");
        }
    }

    @Test void 重复付款被撤销_关闭它的退款待办_不动订单() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 10, 100);
            var order = paidOrder(f, "dup-order-001", "dup-pay-first");
            f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(), "test", "dup-pay-second", order.amountMinor(), order.currency()));
            var store = revisions(f);
            assertEquals(new CommerceApi.RevisionOutcome(1, "REVOKED", "DELIVERY_PENDING"),
                store.record(new CommerceApi.Revision(order.orderId(), "test", "dup-pay-second", 1, "REVOKED")));
            assertEquals(List.of("REVOKED", "CHANNEL_REVOKED"), refundIntent(f, "dup-pay-second"));
            assertEquals(0, f.count("commerce_reversal_intent"));
            assertEquals("DELIVERY_PENDING", state(f, order));
        }
    }

    @Test void 渠道退不了的退款转人工_订单本身那笔订单转人工() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 10, 100);
            var order = paidOrder(f, "close-order-001", "close-pay-first");
            f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(), "test", "close-pay-second", order.amountMinor(), order.currency()));
            var delivery = new DeliveryStore(f.jdbc, f.manager, f.props);
            var closure = new CommerceApi.RefundClosure(order.orderId(), "test", "close-pay-second", "NO_REFUND_API");
            delivery.closeRefund(closure);
            delivery.closeRefund(closure);
            assertEquals(List.of("MANUAL", "NO_REFUND_API"), refundIntent(f, "close-pay-second"));
            assertTrue(delivery.refunds().isEmpty());
            assertEquals("DELIVERY_PENDING", state(f, order), "异常付款转人工不碰订单");
            assertEquals(400, assertThrows(CommerceException.class, () -> delivery.closeRefund(
                new CommerceApi.RefundClosure(order.orderId(), "test", "close-pay-second", "bad reason"))).status());
            assertEquals(404, assertThrows(CommerceException.class, () -> delivery.closeRefund(
                new CommerceApi.RefundClosure(order.orderId(), "test", "close-pay-first", "NO_REFUND_API"))).status(), "绑定的那笔没有退款待办");

            var late = f.store.create("1", new CommerceApi.CreateOrder("close-order-002", 1, 1, "测试账号", "1"));
            f.store.cancel("1", new CommerceApi.OwnedOrder(late.orderId(), "测试账号", "1"));
            f.store.recordTrustedPayment(new CommerceApi.Payment(late.orderId(), "test", "close-pay-late", late.amountMinor(), late.currency()));
            delivery.closeRefund(new CommerceApi.RefundClosure(late.orderId(), "test", "close-pay-late", "CHANNEL_REFUND_FAILED"));
            assertEquals("MANUAL", state(f, late));
        }
    }

    @Test void 货币发放的SKU冻结进订单_交付待办带乘过数量的货币() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 10, 600);
            f.jdbc.update("UPDATE commerce_sku_grant SET item_config_id=0,count_per_unit=0,currency_type=2,currency_amount=60 WHERE sku_id=1");
            var order = f.store.create("1", new CommerceApi.CreateOrder("currency-order-001", 1, 3, "测试账号", "1"));
            f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(), "test", "currency-pay-001", order.amountMinor(), order.currency()));
            f.jdbc.update("UPDATE commerce_sku_grant SET currency_amount=1 WHERE sku_id=1");
            var intent = new DeliveryStore(f.jdbc, f.manager, f.props).poll(new DeliveryApi.Poll(20, "")).entries().getFirst().intent();
            assertEquals(0, intent.itemConfigId());
            assertEquals(0, intent.itemCount());
            assertEquals(2, intent.currencyType());
            assertEquals(180, intent.currencyAmount(), "下单时冻结,之后改 SKU 条款不影响已付订单");

            f.jdbc.update("UPDATE commerce_sku_grant SET currency_amount=? WHERE sku_id=1", DeliveryStore.MAX_CURRENCY_GRANT);
            assertEquals(409, assertThrows(CommerceException.class, () -> f.store.create("1",
                new CommerceApi.CreateOrder("currency-order-002", 1, 2, "测试账号", "1"))).status(), "乘上数量越过单笔上限");
        }
    }
}
