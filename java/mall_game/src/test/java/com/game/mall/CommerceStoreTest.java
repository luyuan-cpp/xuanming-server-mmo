package com.game.mall;

import static com.game.mall.CommerceApi.RefundReason.AMOUNT_MISMATCH;
import static com.game.mall.CommerceApi.RefundReason.CANCELLED_LATE;
import static com.game.mall.CommerceApi.RefundReason.CURRENCY_MISMATCH;
import static com.game.mall.CommerceApi.RefundReason.DUPLICATE_PAYMENT;
import static org.junit.jupiter.api.Assertions.*;
import java.nio.charset.StandardCharsets;
import java.util.*;
import java.util.concurrent.*;
import java.util.concurrent.atomic.AtomicInteger;
import org.junit.jupiter.api.Test;

class CommerceStoreTest {
    private CommerceApi.CreateOrder request(String key, long sku, int quantity) { return new CommerceApi.CreateOrder(key, sku, quantity, "测试账号", "1"); }
    private CommerceApi.Payment payment(CommerceApi.Order order, String transaction) { return new CommerceApi.Payment(order.orderId(), "test", transaction, order.amountMinor(), order.currency()); }
    /** 渠道实收的金额币种与订单冻结值不同的一笔。 */
    private static CommerceApi.Payment payment(CommerceApi.Order order, String transaction, long amount, String currency) {
        return new CommerceApi.Payment(order.orderId(), "test", transaction, amount, currency);
    }
    private static CommerceApi.OwnedOrder owned(CommerceApi.Order order) { return new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1"); }
    /** 渠道退款回执:paymentTransaction 是被退的那笔付款,refundTransaction 是渠道退款流水。 */
    private static CommerceApi.Refund refund(CommerceApi.Order order, String paymentTransaction, String refundTransaction) {
        return new CommerceApi.Refund(order.orderId(), "test", paymentTransaction, refundTransaction, order.amountMinor(), order.currency());
    }
    private static CommerceApi.PendingRefund pending(CommerceApi.Order order, String transaction, long amount, String currency,
            CommerceApi.RefundReason reason) {
        return new CommerceApi.PendingRefund(order.orderId(), "test", transaction, amount, currency, reason);
    }
    private static byte[] ascii(String value) { return value.getBytes(StandardCharsets.US_ASCII); }
    private static List<List<String>> refundIntents(CommerceFixture f, String orderId) {
        return f.jdbc.query("SELECT transaction_id,source,status FROM commerce_refund_intent WHERE order_sn=? ORDER BY transaction_id",
            (row, n) -> List.of(row.getString(1), row.getString(2), row.getString(3)), orderId);
    }
    private static List<List<String>> abnormalPayments(CommerceFixture f, String orderId) {
        return f.jdbc.query("SELECT transaction_id,reason FROM commerce_abnormal_payment WHERE order_sn=? ORDER BY transaction_id",
            (row, n) -> List.of(row.getString(1), row.getString(2)), orderId);
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
            var paid = f.store.recordTrustedPayment(payment(first, "payment-view-paid")).order();
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
            assertNull(paid.refundReason(), "订单本身那笔不退款");
            assertEquals(paid, f.store.recordTrustedPayment(payment(order, "paid-001")));
            assertEquals("DELIVERY_PENDING", paid.order().state()); assertEquals(1, f.value("pms_sku_stock", "stock"));
            assertEquals(1, f.count("commerce_delivery_intent")); assertEquals(1, f.count("commerce_payment_receipt"));
            assertEquals(409, assertThrows(CommerceException.class, () -> f.store.cancel("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1"))).status());
            // 不同流水不再回 409(第一轮数据审计 §4 第 19 条),但同样不扣第二次库存、不发第二份交付;细节见下面的专门用例。
            assertEquals(new CommerceApi.PaymentOutcome(paid.order(), DUPLICATE_PAYMENT), f.store.recordTrustedPayment(payment(order, "paid-002")));
            assertEquals(1, f.value("pms_sku_stock", "stock")); assertEquals(1, f.count("commerce_delivery_intent"));
        }
    }
    // 第一轮数据审计 §4 第 19 条:已付订单收到不同流水,旧实现只回 409 —— 第二笔钱已经进了渠道,却既没有记录也没有退款路径。
    @Test void 已付订单再收到另一笔付款_落重复付款与待退款_订单库存交付都不动() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 3, 100); var order = f.store.create("1", request("duplicate-001", 1, 2));
            var paid = f.store.recordTrustedPayment(payment(order, "dup-first"));
            var second = f.store.recordTrustedPayment(payment(order, "dup-second"));
            assertEquals(paid.order(), second.order(), "订单视图不变:第二笔不绑定订单");
            assertEquals(DUPLICATE_PAYMENT, second.refundReason(), "适配器靠 refundReason 认出这笔要退、为什么退");
            assertEquals("DELIVERY_PENDING", second.order().state());
            assertEquals(1, f.value("pms_sku_stock", "stock"), "库存只扣一次");
            assertEquals(1, f.count("commerce_delivery_intent"), "只交付一次");
            assertEquals("dup-first", f.jdbc.queryForObject("SELECT transaction_id FROM commerce_payment_receipt WHERE order_sn=?", String.class, order.orderId()));
            assertEquals(1, f.jdbc.queryForObject("SELECT COUNT(*) FROM commerce_payment_receipt WHERE order_sn IS NULL", Integer.class),
                "第二笔记成未绑定的付款回执,与绑定的回执共用交易键主键");
            assertEquals(List.of(List.of("dup-second", "DUPLICATE_PAYMENT")), abnormalPayments(f, order.orderId()));
            assertEquals(List.of(List.of("dup-second", "DUPLICATE_PAYMENT", "PENDING")), refundIntents(f, order.orderId()));
            var delivery = new DeliveryStore(f.jdbc, f.manager, f.props);
            assertEquals(List.of(pending(order, "dup-second", order.amountMinor(), order.currency(), DUPLICATE_PAYMENT)), delivery.refunds(),
                "退款待办列出要退的那一笔与原因,不是订单绑定的那笔");

            assertEquals(second, f.store.recordTrustedPayment(payment(order, "dup-second")), "同一重复流水重放幂等");
            assertEquals(paid, f.store.recordTrustedPayment(payment(order, "dup-first")), "原付款重放幂等");
            assertEquals(2, f.count("commerce_payment_receipt")); assertEquals(1, f.count("commerce_abnormal_payment")); assertEquals(1, f.count("commerce_refund_intent"));

            assertEquals(second, f.store.recordTrustedPayment(payment(order, "dup-third")));
            assertEquals(2, f.count("commerce_abnormal_payment")); assertEquals(2, delivery.refunds().size(), "每一笔多付的钱各有一条待退款");
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
            assertEquals(3, f.count("commerce_payment_receipt")); assertEquals(1, f.count("commerce_abnormal_payment")); assertEquals(1, f.count("commerce_refund_intent"));
            assertEquals(3, f.value("pms_sku_stock", "stock"), "两张已付单各扣一次"); assertEquals(1, f.value("pms_sku_stock", "lock_stock"), "未付单的预占还在");
        }
    }
    @Test void 同一重复流水并发重放只落一笔记录与一条待退款() throws Exception {
        try (var f = new CommerceFixture()) {
            f.sku(1, 3, 100); var order = f.store.create("1", request("duplicate-race-001", 1, 1));
            var paid = f.store.recordTrustedPayment(payment(order, "race-first"));
            var duplicate = new CommerceApi.PaymentOutcome(paid.order(), DUPLICATE_PAYMENT);
            parallel(8, index -> assertEquals(duplicate, f.store.recordTrustedPayment(payment(order, index % 2 == 0 ? "race-second" : "race-third"))));
            assertEquals(2, f.count("commerce_abnormal_payment")); assertEquals(2, f.count("commerce_refund_intent"));
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
            assertEquals(1, f.count("commerce_abnormal_payment")); assertEquals(1, f.count("commerce_refund_intent"));
            assertEquals(3, f.count("commerce_payment_receipt"));
        }
    }
    // 第二轮数据审计评审 R3:金额或币种与订单不符的付款,旧实现回 409、什么都不落 —— 钱已经进了渠道,系统里却没有记录,也没有退款。
    // 现在与重复付款同形:回 200,记成不绑定订单的回执、异常付款与同原因的退款待办,订单、库存、交付都不动。
    @Test void 付款金额与订单不符_落异常付款与待退款_订单库存交付都不动() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 3, 100); var order = f.store.create("1", request("mismatch-amount-001", 1, 2));
            var underpaid = f.store.recordTrustedPayment(payment(order, "amount-short", 199, "CNY"));
            assertEquals(new CommerceApi.PaymentOutcome(order, AMOUNT_MISMATCH), underpaid, "订单视图不变,refundReason 说明这笔要退、为什么退");
            assertEquals(2, f.value("pms_sku_stock", "lock_stock"), "预占不动"); assertEquals(3, f.value("pms_sku_stock", "stock"), "库存不扣");
            assertEquals(0, f.count("commerce_delivery_intent"), "不交付");
            assertEquals(1, f.jdbc.queryForObject("SELECT COUNT(*) FROM commerce_payment_receipt WHERE order_sn IS NULL", Integer.class),
                "记成不绑定订单的回执");
            assertEquals(199L, f.jdbc.queryForObject("SELECT amount_minor FROM commerce_payment_receipt WHERE channel=? AND transaction_id=?",
                Long.class, ascii("test"), ascii("amount-short")), "回执记渠道实收值");
            assertEquals(List.of(List.of("amount-short", "AMOUNT_MISMATCH")), abnormalPayments(f, order.orderId()));
            assertEquals(List.of(List.of("amount-short", "AMOUNT_MISMATCH", "PENDING")), refundIntents(f, order.orderId()));

            assertEquals(AMOUNT_MISMATCH, f.store.recordTrustedPayment(payment(order, "amount-over", 201, "CNY")).refundReason(), "多付同样不收");
            var delivery = new DeliveryStore(f.jdbc, f.manager, f.props);
            assertEquals(List.of(pending(order, "amount-over", 201, "CNY", AMOUNT_MISMATCH), pending(order, "amount-short", 199, "CNY", AMOUNT_MISMATCH)),
                delivery.refunds(), "退的是玩家实付的金额,不是订单价");

            var exact = f.store.recordTrustedPayment(payment(order, "amount-exact"));
            assertNull(exact.refundReason()); assertEquals("DELIVERY_PENDING", exact.order().state(), "不符的钱不占订单,金额对的那笔照常付清");
            assertEquals(1, f.value("pms_sku_stock", "stock")); assertEquals(0, f.value("pms_sku_stock", "lock_stock"));
            assertEquals(1, f.count("commerce_delivery_intent"));

            assertEquals(409, assertThrows(CommerceException.class, () -> delivery.refunded(refund(order, "amount-short", "refund-short-at-price"))).status(),
                "退款金额必须等于那笔实付,不是订单价");
            delivery.refunded(new CommerceApi.Refund(order.orderId(), "test", "amount-short", "refund-short", 199, "CNY"));
            assertEquals("DELIVERY_PENDING", f.store.get("1", owned(order)).state(), "退异常付款不碰订单");
            assertEquals(List.of(pending(order, "amount-over", 201, "CNY", AMOUNT_MISMATCH)), delivery.refunds());
            assertEquals(List.of("amount-exact", "amount-over", "amount-short"), f.jdbc.queryForList(
                "SELECT transaction_id FROM commerce_payment_receipt ORDER BY transaction_id", String.class), "三笔各占一行回执");
        }
    }
    @Test void 付款币种与订单不符_币种先于金额与订单状态判定() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 5, 100);
            var unpaid = f.store.create("1", request("mismatch-currency-001", 1, 1));
            assertEquals(new CommerceApi.PaymentOutcome(unpaid, CURRENCY_MISMATCH), f.store.recordTrustedPayment(payment(unpaid, "currency-usd", 100, "USD")),
                "数字相同也不收:不同币种的金额不可比");
            assertEquals(CURRENCY_MISMATCH, f.store.recordTrustedPayment(payment(unpaid, "currency-and-amount", 99, "USD")).refundReason(),
                "币种和金额都不符时记币种不符");

            var settled = f.store.create("2", request("mismatch-currency-002", 1, 1));
            f.store.recordTrustedPayment(payment(settled, "currency-settled-first"));
            assertEquals(AMOUNT_MISMATCH, f.store.recordTrustedPayment(payment(settled, "currency-settled-short", 99, "CNY")).refundReason(),
                "已付订单收到金额不符的一笔:记金额不符,不是重复付款");

            var cancelled = f.store.create("3", request("mismatch-currency-003", 1, 1));
            f.store.cancel("3", owned(cancelled));
            var stray = f.store.recordTrustedPayment(payment(cancelled, "currency-cancelled", 100, "EUR"));
            assertEquals(CURRENCY_MISMATCH, stray.refundReason());
            assertEquals("CANCELLED", stray.order().state(), "不符的钱不绑定已取消的订单,订单不转退款中");
            var late = f.store.recordTrustedPayment(payment(cancelled, "currency-cancelled-late"));
            assertEquals(CANCELLED_LATE, late.refundReason()); assertEquals("REFUND_PENDING", late.order().state(), "金额币种对的迟到付款照旧绑定原单");

            assertEquals(List.of(List.of("currency-and-amount", "CURRENCY_MISMATCH"), List.of("currency-usd", "CURRENCY_MISMATCH")), abnormalPayments(f, unpaid.orderId()));
            assertEquals(List.of(List.of("currency-settled-short", "AMOUNT_MISMATCH")), abnormalPayments(f, settled.orderId()));
            assertEquals(List.of(List.of("currency-cancelled", "CURRENCY_MISMATCH")), abnormalPayments(f, cancelled.orderId()));
            assertEquals(5, f.count("commerce_refund_intent"), "要退的钱每笔一条待办");
            assertEquals(1, f.count("commerce_delivery_intent"));
            assertEquals(4, f.value("pms_sku_stock", "stock")); assertEquals(1, f.value("pms_sku_stock", "lock_stock"), "未付单的预占还在");

            var delivery = new DeliveryStore(f.jdbc, f.manager, f.props);
            assertEquals(409, assertThrows(CommerceException.class, () -> delivery.refunded(
                new CommerceApi.Refund(unpaid.orderId(), "test", "currency-usd", "refund-usd-as-cny", 100, "CNY"))).status(), "按实付币种退");
            delivery.refunded(new CommerceApi.Refund(unpaid.orderId(), "test", "currency-usd", "refund-usd", 100, "USD"));
            assertEquals("AWAITING_PAYMENT", f.store.get("1", owned(unpaid)).state());
        }
    }
    @Test void 金额不符的同一流水重放幂等_改报金额币种或别单回409() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 5, 100);
            var order = f.store.create("1", request("mismatch-replay-001", 1, 1));
            var other = f.store.create("2", request("mismatch-replay-002", 1, 1));
            var first = f.store.recordTrustedPayment(payment(order, "replay-short", 99, "CNY"));
            assertEquals(first, f.store.recordTrustedPayment(payment(order, "replay-short", 99, "CNY")), "原样重放幂等");
            assertEquals(409, assertThrows(CommerceException.class, () -> f.store.recordTrustedPayment(payment(order, "replay-short"))).status(),
                "同一流水改报成订单价也不能变成订单的付款");
            assertEquals(409, assertThrows(CommerceException.class, () -> f.store.recordTrustedPayment(payment(order, "replay-short", 99, "USD"))).status());
            assertEquals(409, assertThrows(CommerceException.class, () -> f.store.recordTrustedPayment(payment(other, "replay-short", 99, "CNY"))).status(),
                "一笔渠道交易只属于一张订单");
            assertEquals("AWAITING_PAYMENT", f.store.get("1", owned(order)).state()); assertEquals("AWAITING_PAYMENT", f.store.get("2", owned(other)).state());
            assertEquals(1, f.count("commerce_payment_receipt")); assertEquals(1, f.count("commerce_abnormal_payment")); assertEquals(1, f.count("commerce_refund_intent"));
            assertEquals(2, f.value("pms_sku_stock", "lock_stock"));

            // 订单之后被正常付清,这笔不符的流水再来:结论不变,订单视图是当前状态。
            f.store.recordTrustedPayment(payment(order, "replay-exact"));
            var later = f.store.recordTrustedPayment(payment(order, "replay-short", 99, "CNY"));
            assertEquals(AMOUNT_MISMATCH, later.refundReason()); assertEquals("DELIVERY_PENDING", later.order().state());
            assertEquals(1, f.count("commerce_abnormal_payment")); assertEquals(1, f.count("commerce_refund_intent"));
        }
    }
    @Test void 同一笔金额不符的流水并发回调只落一笔记录与一条待退款() throws Exception {
        try (var f = new CommerceFixture()) {
            f.sku(1, 3, 100); var order = f.store.create("1", request("mismatch-race-001", 1, 1));
            var expected = new CommerceApi.PaymentOutcome(order, AMOUNT_MISMATCH);
            parallel(8, index -> assertEquals(expected, f.store.recordTrustedPayment(
                index % 2 == 0 ? payment(order, "race-short", 99, "CNY") : payment(order, "race-over", 101, "CNY"))));
            assertEquals(2, f.count("commerce_payment_receipt")); assertEquals(2, f.count("commerce_abnormal_payment")); assertEquals(2, f.count("commerce_refund_intent"));
            assertEquals("AWAITING_PAYMENT", f.store.get("1", owned(order)).state());
            assertEquals(1, f.value("pms_sku_stock", "lock_stock")); assertEquals(0, f.count("commerce_delivery_intent"));
        }
    }
    @Test void 两张订单并发报同一笔金额不符的流水_交易键主键只让一张记成异常付款() throws Exception {
        try (var f = new CommerceFixture()) {
            f.sku(1, 5, 100);
            var first = f.store.create("1", request("mismatch-two-001", 1, 1)); var second = f.store.create("2", request("mismatch-two-002", 1, 1));
            AtomicInteger success = new AtomicInteger();
            parallel(2, index -> {
                try { f.store.recordTrustedPayment(payment(index == 0 ? first : second, "two-short", 99, "CNY")); success.incrementAndGet(); }
                catch (CommerceException refused) { assertEquals(409, refused.status()); }
            });
            assertEquals(1, success.get());
            assertEquals(1, f.count("commerce_payment_receipt")); assertEquals(1, f.count("commerce_abnormal_payment")); assertEquals(1, f.count("commerce_refund_intent"));
            assertEquals(2, f.value("pms_sku_stock", "lock_stock"), "两张订单都还待付款");
        }
    }
    @Test void 一条渠道退款流水只能了结一笔付款_换一笔复用回409() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 3, 100); var order = f.store.create("1", request("refund-reuse-001", 1, 1));
            f.store.recordTrustedPayment(payment(order, "reuse-short", 99, "CNY"));
            f.store.recordTrustedPayment(payment(order, "reuse-over", 101, "CNY"));
            var delivery = new DeliveryStore(f.jdbc, f.manager, f.props);
            delivery.refunded(new CommerceApi.Refund(order.orderId(), "test", "reuse-short", "refund-reuse", 99, "CNY"));
            assertEquals(409, assertThrows(CommerceException.class, () -> delivery.refunded(
                new CommerceApi.Refund(order.orderId(), "test", "reuse-over", "refund-reuse", 101, "CNY"))).status(),
                "同一条退款流水不能再记到另一笔付款上");
            assertEquals(List.of(pending(order, "reuse-over", 101, "CNY", AMOUNT_MISMATCH)), delivery.refunds(), "另一笔仍待退");
            assertEquals(1, f.count("commerce_refund_receipt"));
        }
    }
    @Test void cancelIsIdempotentAndLatePaymentRequiresRefundHandling() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 2, 100); var order = f.store.create("1", request("cancel-001", 1, 1));
            assertEquals(f.store.cancel("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")), f.store.cancel("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")));
            assertEquals(0, f.value("pms_sku_stock", "lock_stock")); assertEquals(2, f.value("pms_sku_stock", "stock"));
            var late = f.store.recordTrustedPayment(payment(order, "late-payment"));
            assertEquals("REFUND_PENDING", late.order().state());
            assertEquals(CANCELLED_LATE, late.refundReason(), "迟到的那笔仍绑定订单,随订单退");
            assertEquals(1, f.count("commerce_payment_receipt"));
            assertEquals(1,f.count("commerce_refund_intent"));
        }
    }
    @Test void 取消后迟到付款与其后的重复付款各自退款_只有订单那笔推进订单() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 2, 100); var order = f.store.create("1", request("cancel-dup-001", 1, 1));
            f.store.cancel("1", owned(order));
            assertEquals("REFUND_PENDING", f.store.recordTrustedPayment(payment(order, "late-first")).order().state());
            var second = f.store.recordTrustedPayment(payment(order, "late-second"));
            assertEquals("REFUND_PENDING", second.order().state()); assertEquals(DUPLICATE_PAYMENT, second.refundReason());
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
    // 原用例 wrongAmountAndBrokenReservationRollbackAllPaymentFacts 还断言金额币种不符回 409、什么都不落 —— 那正是缺陷行为
    // (钱停在渠道里,见上面几例);这里只留库存预占损坏时整笔回滚。存储损坏回 503:409 留给适配器报错了交易,适配器会把 409 当终局。
    @Test void brokenReservationRollsBackAllPaymentFacts() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 2, 100); var order = f.store.create("1", request("rollback-001", 1, 1));
            f.jdbc.update("UPDATE pms_sku_stock SET lock_stock=0 WHERE id=1");
            assertEquals(503, assertThrows(CommerceException.class, () -> f.store.recordTrustedPayment(payment(order, "broken-reservation"))).status());
            assertEquals(0, f.count("commerce_payment_receipt")); assertEquals(0, f.count("commerce_delivery_intent")); assertEquals(0, f.count("commerce_refund_intent"));
            assertEquals("AWAITING_PAYMENT", f.store.get("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")).state()); assertEquals(2, f.value("pms_sku_stock", "stock"));
        }
    }
    @Test void 存储不自洽时付款回503_不当成适配器的错也不误报已付() {
        try (var f = new CommerceFixture()) {
            f.sku(1, 3, 100); var order = f.store.create("1", request("inconsistent-001", 1, 1));
            // 孤儿退款待办占着交易键:异常付款写到第三行撞键,整笔回滚。交易键并不属于别的订单,不能回 409 让适配器放掉这笔钱。
            f.jdbc.update("INSERT INTO commerce_refund_intent(channel,transaction_id,order_sn,source,status) VALUES(?,?,?,?,'PENDING')",
                ascii("test"), ascii("orphan-short"), order.orderId(), "AMOUNT_MISMATCH");
            assertEquals(503, assertThrows(CommerceException.class, () -> f.store.recordTrustedPayment(payment(order, "orphan-short", 99, "CNY"))).status());
            assertEquals(0, f.count("commerce_payment_receipt")); assertEquals(0, f.count("commerce_abnormal_payment"));

            // 异常付款丢了退款待办:照常重放会回 refundReason=null,被当成订单本身的付款,必须拒绝。
            f.store.recordTrustedPayment(payment(order, "lost-intent", 99, "CNY"));
            f.jdbc.update("DELETE FROM commerce_refund_intent WHERE channel=? AND transaction_id=?", ascii("test"), ascii("lost-intent"));
            assertEquals(503, assertThrows(CommerceException.class, () -> f.store.recordTrustedPayment(payment(order, "lost-intent", 99, "CNY"))).status());
            assertEquals("AWAITING_PAYMENT", f.store.get("1", owned(order)).state());
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
