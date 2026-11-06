package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;
import java.nio.charset.StandardCharsets;
import java.util.*;
import java.util.concurrent.*;
import java.util.concurrent.atomic.AtomicInteger;
import org.junit.jupiter.api.Test;

class CommerceStoreTest {
    private CommerceApi.CreateOrder request(String key, long sku, int quantity) { return new CommerceApi.CreateOrder(key, sku, quantity, "测试账号", "1"); }
    private CommerceApi.Payment payment(CommerceApi.Order order, String transaction) { return new CommerceApi.Payment(order.orderId(), "test", transaction, order.amountMinor(), order.currency()); }
    private static CommerceApi.OwnedOrder owned(CommerceApi.Order order) { return new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1"); }
    /** 渠道退款回执:paymentTransaction 是被退的那笔付款,refundTransaction 是渠道退款流水。 */
    private static CommerceApi.Refund refund(CommerceApi.Order order, String paymentTransaction, String refundTransaction) {
        return new CommerceApi.Refund(order.orderId(), "test", paymentTransaction, refundTransaction, order.amountMinor(), order.currency());
    }
    private static byte[] ascii(String value) { return value.getBytes(StandardCharsets.US_ASCII); }
    private static List<List<String>> refundIntents(CommerceFixture f, String orderId) {
        return f.jdbc.query("SELECT transaction_id,source,status FROM commerce_refund_intent WHERE order_sn=? ORDER BY transaction_id",
            (row, n) -> List.of(row.getString(1), row.getString(2), row.getString(3)), orderId);
    }

    @Test void 支付只读查单保留冻结金额和终态_不修改订单或库存() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 8, 125);
            var first = f.store.create("18446744073709551615", request("payment-view-001", 1, 2));
            f.jdbc.update("UPDATE pms_sku_stock SET price_minor=999 WHERE id=1");
            String orderProjection = "SELECT id,order_sn,buyer_id,sku_id,quantity,pay_amount,currency,status,"
                + "create_time_ms,payment_deadline_ms,payment_time_ms FROM oms_order ORDER BY id";
            var orders = f.jdbc.queryForList(orderProjection);
            var stocks = f.jdbc.queryForList("SELECT * FROM pms_sku_stock ORDER BY id");
            var grants = f.jdbc.queryForList("SELECT * FROM commerce_order_grant ORDER BY order_sn");
            assertEquals(first, f.store.paymentOrderView(first.orderId()));
            assertEquals(orders, f.jdbc.queryForList(orderProjection));
            assertEquals(stocks, f.jdbc.queryForList("SELECT * FROM pms_sku_stock ORDER BY id"));
            assertEquals(grants, f.jdbc.queryForList("SELECT * FROM commerce_order_grant ORDER BY order_sn"));
            var paid = f.store.recordTrustedPayment(payment(first, "payment-view-paid"));
            assertEquals(paid, f.store.paymentOrderView(first.orderId()));
            assertEquals("DELIVERY_PENDING", paid.state());
            assertEquals(1, f.count("commerce_payment_receipt"));
            assertEquals(1, f.count("commerce_delivery_intent"));
        }
    }

    @Test void 支付只读查单拒绝非规范订单号_缺失返回未找到() {
        try (var f = new CommerceFixture()) {
            for (String invalid : new String[] {null, "", " ", "ABCDEF00-0000-0000-0000-000000000001", "x".repeat(257)}) {
                assertEquals(400, assertThrows(CommerceException.class, () -> f.store.paymentOrderView(invalid)).status());
            }
            assertEquals(404, assertThrows(CommerceException.class,
                () -> f.store.paymentOrderView("00000000-0000-0000-0000-000000000001")).status());
            assertEquals(0, f.count("oms_order"));
            assertEquals(0, f.count("commerce_payment_receipt"));
            assertEquals(0, f.count("commerce_delivery_intent"));
        }
    }

    @Test void requestRetryKeepsOriginalPriceAndRejectsParameterChanges() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 5, 125);
            CommerceApi.Order first = f.store.create("18446744073709551615", request("request-001", 1, 2));
            f.jdbc.update("UPDATE pms_sku_stock SET price_minor=999 WHERE id=1");
            assertEquals(first, f.store.create("18446744073709551615", request("request-001", 1, 2)));
            assertEquals(250, first.amountMinor()); assertEquals(2, f.value("pms_sku_stock", "lock_stock"));
            assertEquals(409, assertThrows(CommerceException.class, () -> f.store.create("18446744073709551615", request("request-001", 1, 1))).status());
            assertEquals(404, assertThrows(CommerceException.class, () -> f.store.get("2", new CommerceApi.OwnedOrder(first.orderId(), "测试账号", "1"))).status());
            assertEquals(1_760_000_900_000L, first.paymentDeadlineMs());
        }
    }
    @Test void concurrentRequestsCannotOversell() throws Exception {
        try (var f = new CommerceFixture()) {
            f.sku(1, 2, 100); AtomicInteger success = new AtomicInteger();
            parallel(16, index -> {
                try { f.store.create(Integer.toString(100 + index), request("concurrent-" + index, 1, 1)); success.incrementAndGet(); }
                catch (CommerceException refused) { assertEquals(409, refused.status()); }
            });
            assertEquals(2, success.get()); assertEquals(2, f.count("oms_order")); assertEquals(2, f.value("pms_sku_stock", "lock_stock"));
        }
    }
    @Test void sameRequestAcrossReplicasCreatesOnlyOneOrder() throws Exception {
        try (var f = new CommerceFixture()) {
            f.sku(1, 100, 100); Set<String> ids = ConcurrentHashMap.newKeySet();
            parallel(16, index -> ids.add(f.store.create("1", request("same-key-001", 1, 1)).orderId()));
            assertEquals(1, ids.size()); assertEquals(1, f.count("oms_order_item")); assertEquals(1, f.value("pms_sku_stock", "lock_stock"));
        }
    }
    @Test void sameRequestCannotSwitchSkuUnderConcurrency() throws Exception {
        try (var f = new CommerceFixture()) {
            f.sku(1, 10, 100); f.sku(2, 10, 200); AtomicInteger success = new AtomicInteger();
            parallel(2, index -> {
                try { f.store.create("1", request("different-sku-001", index + 1, 1)); success.incrementAndGet(); }
                catch (CommerceException refused) { assertEquals(409, refused.status()); }
            });
            assertEquals(1, success.get()); assertEquals(1, f.count("oms_order")); assertEquals(1, f.value("pms_sku_stock", "lock_stock"));
        }
    }
    @Test void buyerLimitIsRecheckedUnderBuyerGuard() throws Exception {
        try (var f = new CommerceFixture()) {
            f.sku(1, 100, 100); AtomicInteger success = new AtomicInteger();
            parallel(16, index -> {
                try { f.store.create("1", request("buyer-limit-" + index, 1, 1)); success.incrementAndGet(); }
                catch (CommerceException refused) { assertEquals(429, refused.status()); }
            });
            assertEquals(5, success.get()); assertEquals(5, f.count("oms_order")); assertEquals(5, f.value("pms_sku_stock", "lock_stock"));
        }
    }
    @Test void paymentAndCancelSerializeToOneTerminalDecision() throws Exception {
        try (var f = new CommerceFixture()) {
            f.sku(1, 1, 100); var order = f.store.create("1", request("pay-cancel-001", 1, 1));
            parallel(2, index -> {
                try { if (index == 0) { f.store.cancel("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")); } else { f.store.recordTrustedPayment(payment(order, "pay-race")); } }
                catch (CommerceException refused) { assertEquals(409, refused.status()); }
            });
            String state = f.store.get("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")).state();
            assertEquals(0, f.value("pms_sku_stock", "lock_stock"));
            if (state.equals("REFUND_PENDING")) { assertEquals(1, f.value("pms_sku_stock", "stock")); assertEquals(1, f.count("commerce_payment_receipt")); assertEquals(0, f.count("commerce_delivery_intent"));assertEquals(1,f.count("commerce_refund_intent")); }
            else { assertEquals("DELIVERY_PENDING", state); assertEquals(0, f.value("pms_sku_stock", "stock")); assertEquals(1, f.count("commerce_payment_receipt")); assertEquals(1, f.count("commerce_delivery_intent")); }
        }
    }
    @Test void duplicatePaymentNeverDeductsTwiceAndCannotCompleteWithoutDelivery() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 3, 100); var order = f.store.create("1", request("payment-001", 1, 2));
            var paid = f.store.recordTrustedPayment(payment(order, "paid-001"));
            assertEquals(paid, f.store.recordTrustedPayment(payment(order, "paid-001")));
            assertEquals("DELIVERY_PENDING", paid.state()); assertEquals(1, f.value("pms_sku_stock", "stock"));
            assertEquals(1, f.count("commerce_delivery_intent")); assertEquals(1, f.count("commerce_payment_receipt"));
            assertEquals(409, assertThrows(CommerceException.class, () -> f.store.cancel("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1"))).status());
            // 不同流水不再回 409(第一轮数据审计 §4 第 19 条),但同样不扣第二次库存、不发第二份交付;细节见下面的专门用例。
            assertEquals(paid, f.store.recordTrustedPayment(payment(order, "paid-002")));
            assertEquals(1, f.value("pms_sku_stock", "stock")); assertEquals(1, f.count("commerce_delivery_intent"));
        }
    }
    // 第一轮数据审计 §4 第 19 条:已付订单收到不同流水,旧实现只回 409 —— 第二笔钱已经进了渠道,却既没有记录也没有退款路径。
    @Test void 已付订单再收到另一笔付款_落重复付款与待退款_订单库存交付都不动() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 3, 100); var order = f.store.create("1", request("duplicate-001", 1, 2));
            var paid = f.store.recordTrustedPayment(payment(order, "dup-first"));
            var second = f.store.recordTrustedPayment(payment(order, "dup-second"));
            assertEquals(paid, second, "应答仍是订单视图,状态不变:第二笔不绑定订单");
            assertEquals("DELIVERY_PENDING", second.state());
            assertEquals(1, f.value("pms_sku_stock", "stock"), "库存只扣一次");
            assertEquals(1, f.count("commerce_delivery_intent"), "只交付一次");
            assertEquals("dup-first", f.jdbc.queryForObject("SELECT transaction_id FROM commerce_payment_receipt WHERE order_sn=?", String.class, order.orderId()));
            assertEquals(1, f.jdbc.queryForObject("SELECT COUNT(*) FROM commerce_payment_receipt WHERE order_sn IS NULL", Integer.class),
                "第二笔记成未绑定的付款回执,与绑定的回执共用交易键主键");
            assertEquals(List.of(order.orderId()), f.jdbc.queryForList("SELECT order_sn FROM commerce_duplicate_payment WHERE channel=? AND transaction_id=?",
                String.class, ascii("test"), ascii("dup-second")));
            assertEquals(List.of(List.of("dup-second", "DUPLICATE_PAYMENT", "PENDING")), refundIntents(f, order.orderId()));
            var delivery = new DeliveryStore(f.jdbc, f.manager, f.props);
            assertEquals(List.of(new CommerceApi.Payment(order.orderId(), "test", "dup-second", order.amountMinor(), order.currency())), delivery.refunds(),
                "退款待办列出要退的那一笔,不是订单绑定的那笔");

            assertEquals(paid, f.store.recordTrustedPayment(payment(order, "dup-second")), "同一重复流水重放幂等");
            assertEquals(paid, f.store.recordTrustedPayment(payment(order, "dup-first")), "原付款重放幂等");
            assertEquals(2, f.count("commerce_payment_receipt")); assertEquals(1, f.count("commerce_duplicate_payment")); assertEquals(1, f.count("commerce_refund_intent"));

            assertEquals(paid, f.store.recordTrustedPayment(payment(order, "dup-third")));
            assertEquals(2, f.count("commerce_duplicate_payment")); assertEquals(2, delivery.refunds().size(), "每一笔多付的钱各有一条待退款");
            assertEquals(1, f.value("pms_sku_stock", "stock")); assertEquals(1, f.count("commerce_delivery_intent"));
        }
    }
    @Test void 重复付款的退款回执只终结那一笔_订单与原付款照常交付() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 3, 100); var order = f.store.create("1", request("duplicate-refund-001", 1, 1));
            f.store.recordTrustedPayment(payment(order, "dup-refund-first"));
            f.store.recordTrustedPayment(payment(order, "dup-refund-second"));
            var delivery = new DeliveryStore(f.jdbc, f.manager, f.props);
            assertEquals(404, assertThrows(CommerceException.class, () -> delivery.refunded(refund(order, "dup-refund-first", "refund-of-bound"))).status(),
                "订单绑定的那笔没有退款待办,不能借重复付款的回执退掉");
            assertEquals(409, assertThrows(CommerceException.class, () -> delivery.refunded(new CommerceApi.Refund(order.orderId(), "test",
                "dup-refund-second", "refund-short", order.amountMinor() - 1, order.currency()))).status(), "退款金额必须等于那一笔");
            var receipt = refund(order, "dup-refund-second", "refund-dup-001");
            delivery.refunded(receipt); delivery.refunded(receipt);
            assertEquals(409, assertThrows(CommerceException.class, () -> delivery.refunded(refund(order, "dup-refund-second", "refund-dup-002"))).status(),
                "同一笔付款不能再挂第二条退款流水");
            assertTrue(delivery.refunds().isEmpty());
            assertEquals(List.of(List.of("dup-refund-second", "DUPLICATE_PAYMENT", "REFUNDED")), refundIntents(f, order.orderId()));
            assertEquals(1, f.count("commerce_refund_receipt"));
            assertEquals("DELIVERY_PENDING", f.store.get("1", owned(order)).state(), "退掉重复付款不碰订单");
            delivery.receipt(new DeliveryApi.Receipt(order.orderId(), "123", "DELIVERED", "a".repeat(64)));
            assertEquals("DELIVERED", f.store.get("1", owned(order)).state(), "原付款照常交付");
        }
    }
    @Test void 一笔渠道流水全局只记一次_不能既绑定一单又是别单的重复付款() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 5, 100);
            var first = f.store.create("1", request("txn-owner-001", 1, 1));
            var second = f.store.create("2", request("txn-owner-002", 1, 1));
            var unpaid = f.store.create("3", request("txn-owner-003", 1, 1));
            f.store.recordTrustedPayment(payment(first, "owner-a")); f.store.recordTrustedPayment(payment(second, "owner-b"));
            assertEquals(409, assertThrows(CommerceException.class, () -> f.store.recordTrustedPayment(payment(second, "owner-a"))).status(), "别单绑定的流水");
            f.store.recordTrustedPayment(payment(first, "owner-extra"));
            assertEquals(409, assertThrows(CommerceException.class, () -> f.store.recordTrustedPayment(payment(second, "owner-extra"))).status(), "别单的重复付款流水");
            assertEquals(409, assertThrows(CommerceException.class, () -> f.store.recordTrustedPayment(payment(unpaid, "owner-extra"))).status(),
                "未付单也不能拿别单的重复付款流水来绑定");
            assertEquals("AWAITING_PAYMENT", f.store.get("3", owned(unpaid)).state());
            assertEquals(3, f.count("commerce_payment_receipt")); assertEquals(1, f.count("commerce_duplicate_payment")); assertEquals(1, f.count("commerce_refund_intent"));
            assertEquals(3, f.value("pms_sku_stock", "stock"), "两张已付单各扣一次"); assertEquals(1, f.value("pms_sku_stock", "lock_stock"), "未付单的预占还在");
        }
    }
    @Test void 同一重复流水并发重放只落一笔记录与一条待退款() throws Exception {
        try (var f = new CommerceFixture()) {
            f.sku(1, 3, 100); var order = f.store.create("1", request("duplicate-race-001", 1, 1));
            var paid = f.store.recordTrustedPayment(payment(order, "race-first"));
            parallel(8, index -> assertEquals(paid, f.store.recordTrustedPayment(payment(order, index % 2 == 0 ? "race-second" : "race-third"))));
            assertEquals(2, f.count("commerce_duplicate_payment")); assertEquals(2, f.count("commerce_refund_intent"));
            assertEquals(3, f.count("commerce_payment_receipt")); assertEquals(1, f.count("commerce_delivery_intent"));
            assertEquals(2, f.value("pms_sku_stock", "stock"));
        }
    }
    @Test void 两张已付单并发报同一笔新流水_库的交易键主键只让一张记成重复付款() throws Exception {
        try (var f = new CommerceFixture()) {
            f.sku(1, 5, 100);
            var first = f.store.create("1", request("dup-two-001", 1, 1)); var second = f.store.create("2", request("dup-two-002", 1, 1));
            f.store.recordTrustedPayment(payment(first, "two-a")); f.store.recordTrustedPayment(payment(second, "two-b"));
            AtomicInteger success = new AtomicInteger();
            parallel(2, index -> {
                try { f.store.recordTrustedPayment(payment(index == 0 ? first : second, "two-shared")); success.incrementAndGet(); }
                catch (CommerceException refused) { assertEquals(409, refused.status()); }
            });
            assertEquals(1, success.get());
            assertEquals(1, f.count("commerce_duplicate_payment")); assertEquals(1, f.count("commerce_refund_intent"));
            assertEquals(3, f.count("commerce_payment_receipt"));
        }
    }
    @Test void cancelIsIdempotentAndLatePaymentRequiresRefundHandling() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 2, 100); var order = f.store.create("1", request("cancel-001", 1, 1));
            assertEquals(f.store.cancel("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")), f.store.cancel("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")));
            assertEquals(0, f.value("pms_sku_stock", "lock_stock")); assertEquals(2, f.value("pms_sku_stock", "stock"));
            assertEquals("REFUND_PENDING", f.store.recordTrustedPayment(payment(order, "late-payment")).state());
            assertEquals(1, f.count("commerce_payment_receipt"));
            assertEquals(1,f.count("commerce_refund_intent"));
        }
    }
    @Test void 取消后迟到付款与其后的重复付款各自退款_只有订单那笔推进订单() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 2, 100); var order = f.store.create("1", request("cancel-dup-001", 1, 1));
            f.store.cancel("1", owned(order));
            assertEquals("REFUND_PENDING", f.store.recordTrustedPayment(payment(order, "late-first")).state());
            assertEquals("REFUND_PENDING", f.store.recordTrustedPayment(payment(order, "late-second")).state());
            assertEquals(List.of(List.of("late-first", "CANCELLED_LATE", "PENDING"), List.of("late-second", "DUPLICATE_PAYMENT", "PENDING")),
                refundIntents(f, order.orderId()));
            var delivery = new DeliveryStore(f.jdbc, f.manager, f.props);
            assertEquals(2, delivery.refunds().size());
            delivery.refunded(refund(order, "late-second", "refund-late-second"));
            assertEquals("REFUND_PENDING", f.store.get("1", owned(order)).state(), "退重复付款不推进订单");
            delivery.refunded(refund(order, "late-first", "refund-late-first"));
            assertEquals("REFUNDED", f.store.get("1", owned(order)).state());
            assertTrue(delivery.refunds().isEmpty()); assertEquals(2, f.count("commerce_refund_receipt"));
            assertEquals(2, f.value("pms_sku_stock", "stock"), "取消单的库存一直不扣");
        }
    }
    @Test void wrongAmountAndBrokenReservationRollbackAllPaymentFacts() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 2, 100); var order = f.store.create("1", request("rollback-001", 1, 1));
            assertThrows(CommerceException.class, () -> f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(), "test", "bad-amount", 99, "CNY")));
            assertThrows(CommerceException.class, () -> f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(), "test", "bad-currency", 100, "USD")));
            f.jdbc.update("UPDATE pms_sku_stock SET lock_stock=0 WHERE id=1");
            assertThrows(CommerceException.class, () -> f.store.recordTrustedPayment(payment(order, "broken-reservation")));
            assertEquals(0, f.count("commerce_payment_receipt")); assertEquals(0, f.count("commerce_delivery_intent"));
            assertEquals("AWAITING_PAYMENT", f.store.get("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")).state()); assertEquals(2, f.value("pms_sku_stock", "stock"));
        }
    }
    @Test void concurrentOrdersCannotShareOnePaymentTransaction() throws Exception {
        try (var f = new CommerceFixture()) {
            f.sku(1, 2, 100); var first = f.store.create("1", request("first-order-001", 1, 1)); var second = f.store.create("2", request("second-order-001", 1, 1));
            AtomicInteger success = new AtomicInteger();
            parallel(2, index -> {
                try { f.store.recordTrustedPayment(payment(index == 0 ? first : second, "one-transaction")); success.incrementAndGet(); }
                catch (CommerceException refused) { assertEquals(409, refused.status()); }
            });
            assertEquals(1, success.get()); assertEquals(1, f.count("commerce_payment_receipt")); assertEquals(1, f.count("commerce_delivery_intent"));
            assertEquals(1, f.value("pms_sku_stock", "stock")); assertEquals(1, f.value("pms_sku_stock", "lock_stock"));
        }
    }
    @Test void requestAndPaymentOpaqueIdentifiersAreCaseSensitive() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 10, 100);
            var first = f.store.create("1", request("OpaqueKeyAAA", 1, 1));
            var second = f.store.create("1", request("opaquekeyaaa", 1, 1));
            assertNotEquals(first.orderId(), second.orderId());
            f.store.recordTrustedPayment(payment(first, "PaymentCase")); f.store.recordTrustedPayment(payment(second, "paymentcase"));
            assertEquals(2, f.count("commerce_payment_receipt")); assertEquals(8, f.value("pms_sku_stock", "stock"));
            var third = f.store.create("1", request("third-key-001", 1, 1));
            assertThrows(CommerceException.class, () -> f.store.recordTrustedPayment(payment(third, "PaymentCase")));
            assertEquals("AWAITING_PAYMENT", f.store.get("1", new CommerceApi.OwnedOrder(third.orderId(), "测试账号", "1")).state());
        }
    }
    @Test void unpublishedAndOverflowingProductsNeverReserve() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 10000, Long.MAX_VALUE); assertThrows(CommerceException.class, () -> f.store.create("1", request("overflow-001", 1, 10000)));
            f.jdbc.update("UPDATE pms_sku_stock SET price_minor=100,published=0 WHERE id=1"); assertThrows(CommerceException.class, () -> f.store.create("1", request("unpublished-001", 1, 1)));
            assertEquals(0, f.count("oms_order")); assertEquals(0, f.value("pms_sku_stock", "lock_stock"));
        }
    }
    private static void parallel(int count, ThrowingConsumer action) throws Exception {
        ExecutorService executor = Executors.newFixedThreadPool(8); CountDownLatch start = new CountDownLatch(1);
        try {
            List<Future<?>> results = new ArrayList<>();
            for (int i = 0; i < count; i++) { int index = i; results.add(executor.submit(() -> { start.await(); action.run(index); return null; })); }
            start.countDown(); for (Future<?> result : results) { result.get(20, TimeUnit.SECONDS); }
        } finally { executor.shutdownNow(); assertTrue(executor.awaitTermination(5, TimeUnit.SECONDS)); }
    }
    @FunctionalInterface private interface ThrowingConsumer { void run(int index) throws Exception; }
}
