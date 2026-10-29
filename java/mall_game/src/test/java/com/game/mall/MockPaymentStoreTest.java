package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.*;

import java.nio.charset.StandardCharsets;
import java.time.Clock;
import java.time.Instant;
import java.time.ZoneOffset;
import java.util.ArrayList;
import java.util.List;
import java.util.Set;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.function.Executable;

/** 每例自有随机库；显式commerce.it.tidb时沿用Fixture的loopback:4000随机TiDB库，未配置时只证明H2契约。 */
class MockPaymentStoreTest {
    private static final Clock CLOCK = Clock.fixed(Instant.ofEpochMilli(1_760_000_000_000L), ZoneOffset.UTC);

    @Test void 默认渠道为空_mock必须显式开且prod拒绝_旧渠道须显式允许() {
        var props = new CommerceProperties();
        props.setGatewaySecret("mock-policy-test-gateway-key-000001");
        props.setPaymentSecret("mock-policy-test-payment-key-000002");
        assertTrue(props.getAllowedPaymentChannels().isEmpty());
        rejects(403, () -> props.requirePaymentChannel("real"));
        rejects(503, props::requireMockPayment);
        props.setRunMode("test");
        rejects(503, props::requireMockPayment);
        props.setMockPaymentEnabled(true);
        assertDoesNotThrow(props::validate);
        assertDoesNotThrow(props::requireMockPayment);
        props.setRunMode("prod");
        assertThrows(IllegalStateException.class, props::validate);
        rejects(503, props::requireMockPayment);
        props.setRunMode("test");
        props.setAllowedPaymentChannels(Set.of("mock"));
        assertThrows(IllegalStateException.class, props::validate);
        props.setAllowedPaymentChannels(Set.of("test"));
        assertDoesNotThrow(props::validate);
        assertDoesNotThrow(() -> props.requirePaymentChannel("test"));
        try (var f = fixture()) {
            f.sku(1, 5, 125);
            var order = create(f, 1, "channel-order-001");
            rejects(503, () -> f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(), "mock", "fake-payment", 125, "CNY")));
            rejects(403, () -> f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(), "real", "fake-real", 125, "CNY")));
            assertEquals(0, f.count("commerce_payment_receipt"));
            assertEquals(0, f.count("commerce_mock_transaction"));
        }
    }

    @Test void 同请求并发与重启只扣一次_请求与订单双向绑定_冻结金额和grant不变() throws Exception {
        try (var f = fixture()) {
            f.sku(1, 10, 125);
            var order = f.store.create("1", new CommerceApi.CreateOrder("mock-order-001", 1, 2, "测试账号", "1"));
            var frozen = f.jdbc.queryForMap("SELECT * FROM commerce_order_grant WHERE order_sn=?", order.orderId());
            f.jdbc.update("UPDATE pms_sku_stock SET price_minor=999,currency='USD' WHERE id=1");
            f.jdbc.update("UPDATE commerce_sku_grant SET item_config_id=20,count_per_unit=3,title='新条款' WHERE sku_id=1");
            var worker = worker(f);
            var command = new MockPaymentApi.Pay(order.orderId(), "mock-request-001");
            var results = parallel(() -> worker.pay(command), () -> worker(f).pay(command));
            assertEquals(results.get(0), results.get(1));
            assertEquals(results.getFirst(), worker(f).pay(command));
            assertEquals(250, results.getFirst().amountMinor());
            assertEquals("CNY", results.getFirst().currency());
            assertEquals(1, f.count("commerce_mock_transaction"));
            assertEquals(1, f.count("commerce_payment_receipt"));
            assertEquals(1, f.count("commerce_delivery_intent"));
            assertEquals(8, f.value("pms_sku_stock", "stock"));
            assertEquals(0, f.value("pms_sku_stock", "lock_stock"));
            assertEquals(frozen, f.jdbc.queryForMap("SELECT * FROM commerce_order_grant WHERE order_sn=?", order.orderId()));
            assertEquals(10, ((Number)frozen.get("item_config_id")).intValue());
            assertEquals(125, f.jdbc.queryForObject("SELECT product_price_minor FROM oms_order_item WHERE order_sn=?", Long.class, order.orderId()));
            var fact = new PaymentPolicy(f.jdbc, f.props).required("PAYMENT", order.orderId());
            assertEquals(250, fact.amountMinor()); assertEquals("CNY", fact.currency()); assertEquals(1, fact.applied());
            rejects(409, () -> worker.pay(new MockPaymentApi.Pay(order.orderId(), "different-request-001")));
            var another = create(f, 2, "mock-order-002");
            rejects(409, () -> worker.pay(new MockPaymentApi.Pay(another.orderId(), command.requestId())));
            assertEquals(1, f.count("commerce_mock_transaction"));
            assertEquals("AWAITING_PAYMENT", f.store.get("2", new CommerceApi.OwnedOrder(another.orderId(), "测试账号", "1")).state());
        }
    }

    @Test void 提供方原件先commit_OMS失败后重启恢复同一交易() {
        try (var f = fixture()) {
            f.sku(1, 5, 100);
            var order = create(f, 1, "crash-source-order");
            var broken = mock(CommerceStore.class);
            when(broken.recordTrustedPayment(any())).thenThrow(new CommerceException(503, "本例OMS边界失败"));
            rejects(503, () -> worker(f, broken, delivery(f), System::nanoTime).pay(new MockPaymentApi.Pay(order.orderId(), "source-crash-request")));
            var original = new PaymentPolicy(f.jdbc, f.props).required("PAYMENT", order.orderId());
            assertEquals(0, original.applied());
            assertEquals(0, f.count("commerce_payment_receipt"));
            assertEquals(1, f.value("pms_sku_stock", "lock_stock"));
            var restored = worker(f).reconcile();
            assertEquals(new MockPaymentApi.Reconcile(1, 1, 0, 0, false, false), restored);
            var after = new PaymentPolicy(f.jdbc, f.props).required("PAYMENT", order.orderId());
            assertEquals(original.transactionId(), after.transactionId());
            assertEquals(original.requestId(), after.requestId());
            assertEquals(1, after.applied());
            assertEquals(1, f.count("commerce_payment_receipt"));
            assertEquals(4, f.value("pms_sku_stock", "stock"));
            assertEquals(0, worker(f).reconcile().attempted());
        }
    }

    @Test void OMS已提交但applied未提交_新worker仍只投递原回执() {
        try (var f = fixture()) {
            f.sku(1, 5, 100);
            var order = create(f, 1, "crash-applied-order");
            var nanos = new AtomicLong();
            var boundary = mock(CommerceStore.class);
            when(boundary.recordTrustedPayment(any())).thenAnswer(call -> {
                var result = f.store.recordTrustedPayment(call.getArgument(0));
                nanos.set(TimeUnit.SECONDS.toNanos(15));
                return result;
            });
            rejects(503, () -> worker(f, boundary, delivery(f), nanos::get).pay(new MockPaymentApi.Pay(order.orderId(), "applied-crash-request")));
            assertEquals(0, new PaymentPolicy(f.jdbc, f.props).required("PAYMENT", order.orderId()).applied());
            assertEquals(1, f.count("commerce_payment_receipt"));
            assertEquals("DELIVERY_PENDING", f.store.get("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")).state());
            assertEquals(1, worker(f).reconcile().succeeded());
            assertEquals(1, f.count("commerce_mock_transaction"));
            assertEquals(1, f.count("commerce_payment_receipt"));
            assertEquals(1, f.count("commerce_delivery_intent"));
            assertEquals(4, f.value("pms_sku_stock", "stock"));
        }
    }

    @Test void 晚付款退款先有独立退款事实_OMS失败恢复_不触碰非mock渠道() {
        try (var f = fixture()) {
            f.sku(1, 10, 100);
            var original = create(f, 1, "refund-source-order");
            f.store.cancel("1", new CommerceApi.OwnedOrder(original.orderId(), "测试账号", "1"));
            assertEquals("REFUND_PENDING", worker(f).pay(new MockPaymentApi.Pay(original.orderId(), "refund-source-request")).state());
            var foreign = create(f, 2, "foreign-refund-order");
            f.store.cancel("2", new CommerceApi.OwnedOrder(foreign.orderId(), "测试账号", "1"));
            f.store.recordTrustedPayment(new CommerceApi.Payment(foreign.orderId(), "test", "foreign-test-payment", 100, "CNY"));
            var broken = mock(DeliveryStore.class);
            doThrow(new CommerceException(503, "本例退款OMS边界失败")).when(broken).refunded(any());
            var failed = worker(f, f.store, broken, System::nanoTime).reconcile();
            assertEquals(1, failed.attempted()); assertEquals(0, failed.succeeded()); assertEquals(1, failed.failed());
            var refund = new PaymentPolicy(f.jdbc, f.props).required("REFUND", original.orderId());
            var payment = new PaymentPolicy(f.jdbc, f.props).required("PAYMENT", original.orderId());
            assertEquals(payment.transactionId(), refund.paymentTransactionId());
            assertEquals(0, refund.applied());
            assertEquals(0, f.count("commerce_refund_receipt"));
            assertEquals(1, worker(f).reconcile().succeeded());
            assertEquals("REFUNDED", f.store.get("1", new CommerceApi.OwnedOrder(original.orderId(), "测试账号", "1")).state());
            assertEquals("REFUND_PENDING", f.store.get("2", new CommerceApi.OwnedOrder(foreign.orderId(), "测试账号", "1")).state());
            assertNull(new PaymentPolicy(f.jdbc, f.props).find("REFUND", foreign.orderId(), false));
            assertEquals(refund.transactionId(), new PaymentPolicy(f.jdbc, f.props).required("REFUND", original.orderId()).transactionId());
            assertEquals(1, f.count("commerce_refund_receipt"));
            assertEquals(10, f.value("pms_sku_stock", "stock"));
            assertEquals(0, f.value("pms_sku_stock", "lock_stock"));
            assertEquals(0, worker(f).reconcile().attempted());
        }
    }

    @Test void 退款OMS已提交后标记失败_终结意图仍由未applied原件恢复() {
        try (var f = fixture()) {
            f.sku(1, 5, 100);
            var order = create(f, 1, "refund-mark-order");
            worker(f).pay(new MockPaymentApi.Pay(order.orderId(), "refund-mark-request"));
            var realDelivery = delivery(f);
            realDelivery.receipt(new DeliveryApi.Receipt(order.orderId(), "123", "REFUND_PENDING", "a".repeat(64)));
            var nanos = new AtomicLong();
            var boundary = mock(DeliveryStore.class);
            doAnswer(call -> {
                realDelivery.refunded(call.getArgument(0));
                nanos.set(TimeUnit.SECONDS.toNanos(5));
                return null;
            }).when(boundary).refunded(any());
            var result = worker(f, f.store, boundary, nanos::get).reconcile();
            assertEquals(1, result.attempted()); assertEquals(1, result.failed()); assertTrue(result.budgetExhausted());
            assertEquals("REFUNDED", f.store.get("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")).state());
            assertEquals("REFUNDED", f.jdbc.queryForObject("SELECT status FROM commerce_refund_intent WHERE order_sn=?", String.class, order.orderId()));
            assertEquals(0, new PaymentPolicy(f.jdbc, f.props).required("REFUND", order.orderId()).applied());
            assertEquals(1, worker(f).reconcile().succeeded());
            assertEquals(1, f.count("commerce_refund_receipt"));
            assertEquals(2, f.count("commerce_mock_transaction"));
            assertEquals(4, f.value("pms_sku_stock", "stock"), "已交付失败退款不能再扣或补SKU库存");
        }
    }

    @Test void 二十五项前二十坏原件仍放行尾五退款_顺序新worker不制造第二退款() {
        try (var f = fixture()) {
            f.sku(1, 100, 100);
            var worker = worker(f);
            for (int i = 1; i <= 25; i++) {
                var order = create(f, i, "fair-refund-order-" + i);
                f.store.cancel(Integer.toString(i), new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1"));
                worker.pay(new MockPaymentApi.Pay(order.orderId(), "fair-refund-request-" + i));
            }
            var ids = f.jdbc.queryForList("SELECT order_sn FROM commerce_mock_transaction WHERE kind='PAYMENT' ORDER BY order_sn", String.class);
            for (int i = 0; i < 20; i++) {
                assertEquals(1, f.jdbc.update("UPDATE commerce_mock_transaction SET request_id=? WHERE kind='PAYMENT' AND order_sn=?", ascii("bad!" + i), ids.get(i)));
            }
            var first = worker.reconcile();
            assertEquals(25, first.attempted()); assertEquals(5, first.succeeded()); assertEquals(20, first.failed());
            assertEquals(ids.subList(20, 25), f.jdbc.queryForList("SELECT order_sn FROM commerce_refund_receipt ORDER BY order_sn", String.class));
            assertEquals(20, f.jdbc.queryForObject("SELECT COUNT(*) FROM commerce_refund_intent WHERE status='PENDING'", Integer.class));
            assertEquals(5, f.jdbc.queryForObject("SELECT COUNT(*) FROM oms_order WHERE status=6", Integer.class));
            var original = f.jdbc.query("SELECT order_sn,transaction_id,payment_transaction_id FROM commerce_mock_transaction WHERE kind='REFUND' ORDER BY order_sn",
                (row, n) -> List.of(row.getString(1), row.getString(2), row.getString(3)));
            assertEquals(20, worker(f).reconcile().failed());
            assertEquals(20, worker(f).reconcile().failed());
            assertEquals(original, f.jdbc.query("SELECT order_sn,transaction_id,payment_transaction_id FROM commerce_mock_transaction WHERE kind='REFUND' ORDER BY order_sn",
                (row, n) -> List.of(row.getString(1), row.getString(2), row.getString(3))));
            assertEquals(25, f.count("commerce_payment_receipt"));
            assertEquals(5, f.count("commerce_refund_receipt"));
            assertEquals(100, f.value("pms_sku_stock", "stock"));
        }
    }

    @Test void 预算在取项后耗尽不前移_下一调用处理原项() {
        try (var f = fixture()) {
            f.sku(1, 5, 100);
            var order = create(f, 1, "budget-before-order");
            var broken = mock(CommerceStore.class);
            when(broken.recordTrustedPayment(any())).thenThrow(new CommerceException(503, "保留未投递原件"));
            rejects(503, () -> worker(f, broken, delivery(f), System::nanoTime).pay(new MockPaymentApi.Pay(order.orderId(), "budget-before-request")));
            var calls = new AtomicInteger();
            var scanning = worker(f, f.store, delivery(f), () -> calls.incrementAndGet() >= 3 ? TimeUnit.SECONDS.toNanos(5) : 0);
            var first = scanning.reconcile();
            assertEquals(0, first.attempted()); assertTrue(first.budgetExhausted());
            assertEquals(0, f.count("commerce_payment_receipt"));
            assertEquals(1, scanning.reconcile().succeeded());
            assertEquals(1, f.count("commerce_payment_receipt"));
        }
    }

    @Test void mock回执不可跳过提供方事实_金额与退款原付款绑定不可伪造() {
        try (var f = fixture()) {
            f.sku(1, 5, 100);
            var order = create(f, 1, "forged-mock-order");
            rejects(503, () -> f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(), "mock", "mock-pay-" + order.orderId(), 100, "CNY")));
            worker(f).pay(new MockPaymentApi.Pay(order.orderId(), "forged-mock-request"));
            delivery(f).receipt(new DeliveryApi.Receipt(order.orderId(), "123", "REFUND_PENDING", "b".repeat(64)));
            rejects(503, () -> delivery(f).refunded(new CommerceApi.Payment(order.orderId(), "mock", "mock-refund-" + order.orderId(), 100, "CNY")));
            rejects(409, () -> delivery(f).refunded(new CommerceApi.Payment(order.orderId(), "test", "foreign-refund", 100, "CNY")));
            assertEquals(0, f.count("commerce_refund_receipt"));
            assertEquals(1, worker(f).reconcile().succeeded());
        }
    }

    @Test void 两独立worker并发退款只能形成一笔原件与原回执() throws Exception {
        try (var f = fixture()) {
            f.sku(1, 5, 100);
            var order = create(f, 1, "parallel-refund-order");
            f.store.cancel("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1"));
            worker(f).pay(new MockPaymentApi.Pay(order.orderId(), "parallel-refund-request"));
            var results = parallel(() -> worker(f).reconcile(), () -> worker(f).reconcile());
            for (var result : results) {
                assertEquals(0, result.failed()); assertEquals(0, result.scanFailures()); assertFalse(result.busy());
            }
            assertTrue(results.stream().mapToInt(MockPaymentApi.Reconcile::succeeded).sum() >= 1);
            assertEquals(2, f.count("commerce_mock_transaction"));
            assertEquals(1, f.count("commerce_refund_receipt"));
            assertEquals("REFUNDED", f.store.get("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")).state());
            assertEquals(5, f.value("pms_sku_stock", "stock"));
        }
    }

    @Test void 退款与原付款幂等重放并发只形成一笔原件与原回执() throws Exception {
        try (var f = fixture()) {
            f.sku(1, 5, 100);
            var order = create(f, 1, "parallel-refund-replay-order");
            f.store.cancel("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1"));
            worker(f).pay(new MockPaymentApi.Pay(order.orderId(), "parallel-refund-replay-request"));
            var results = parallel(() -> worker(f).reconcile(),
                () -> worker(f).pay(new MockPaymentApi.Pay(order.orderId(), "parallel-refund-replay-request")));
            var result = (MockPaymentApi.Reconcile) results.getFirst();
            assertEquals(0, result.failed()); assertEquals(0, result.scanFailures()); assertFalse(result.busy());
            assertTrue(result.succeeded() >= 1);
            assertEquals(2, f.count("commerce_mock_transaction"));
            assertEquals(1, f.count("commerce_refund_receipt"));
            assertEquals("REFUNDED", f.store.get("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")).state());
            assertEquals(5, f.value("pms_sku_stock", "stock"));
        }
    }

    @Test void 扫描存储错误不伪装成功且未知项游标不前移() {
        try (var f = fixture()) {
            f.jdbc.execute("DROP TABLE commerce_mock_transaction");
            var result = worker(f).reconcile();
            assertEquals(0, result.attempted()); assertEquals(0, result.succeeded()); assertEquals(0, result.failed());
            assertEquals(2, result.scanFailures()); assertTrue(result.unavailable());
        }
    }

    @Test void V3升级同一V4结构且保留冻结订单与预占_缺mock表拒启动() throws Exception {
        try (var f = fixture()) {
            f.sku(1, 5, 125);
            var order = f.store.create("1", new CommerceApi.CreateOrder("mock-migration-order", 1, 2, "测试账号", "1"));
            var grant = f.jdbc.queryForMap("SELECT * FROM commerce_order_grant WHERE order_sn=?", order.orderId());
            // 本例尚无退款数据，退回V3形状；不操作任何既有共享库。
            f.jdbc.execute("DROP TABLE commerce_mock_transaction");
            f.jdbc.execute("DROP TABLE commerce_refund_intent");
            f.jdbc.execute("CREATE TABLE commerce_refund_intent(order_sn VARCHAR(36) PRIMARY KEY,status VARCHAR(24) NOT NULL,channel VARCHAR(32) NULL,transaction_id VARCHAR(128) NULL)");
            assertThrows(Exception.class, () -> SchemaVerifier.verify(f.jdbc, f.source));
            f.migrate("db/migration/V4__mock_payment.sql");
            SchemaVerifier.verify(f.jdbc, f.source);
            assertEquals(order, f.store.get("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")));
            assertEquals(grant, f.jdbc.queryForMap("SELECT * FROM commerce_order_grant WHERE order_sn=?", order.orderId()));
            assertEquals(5, f.value("pms_sku_stock", "stock"));
            assertEquals(2, f.value("pms_sku_stock", "lock_stock"));
            assertEquals(0, f.count("commerce_mock_transaction"));
        }
    }

    private static CommerceFixture fixture() {
        var f = new CommerceFixture();
        f.props.setRunMode("test"); f.props.setMockPaymentEnabled(true); f.props.validate();
        return f;
    }
    private static CommerceApi.Order create(CommerceFixture f, int buyer, String request) {
        return f.store.create(Integer.toString(buyer), new CommerceApi.CreateOrder(request, 1, 1, "测试账号", "1"));
    }
    private static DeliveryStore delivery(CommerceFixture f) { return new DeliveryStore(f.jdbc, f.manager, f.props); }
    // 批量契约不依赖机器墙钟快慢；专门的预算用例显式推动同一单调时钟，SQL仍有真实事务/查询超时。
    private static MockPaymentStore worker(CommerceFixture f) { return worker(f, f.store, delivery(f), () -> 0L); }
    private static MockPaymentStore worker(CommerceFixture f, CommerceStore orders, DeliveryStore delivery, java.util.function.LongSupplier nanos) {
        return new MockPaymentStore(f.jdbc, f.manager, CLOCK, f.props, orders, delivery, nanos);
    }
    private static byte[] ascii(String value) { return value.getBytes(StandardCharsets.US_ASCII); }
    private static void rejects(int status, Executable action) { assertEquals(status, assertThrows(CommerceException.class, action).status()); }

    private static <T> List<T> parallel(java.util.concurrent.Callable<T> first, java.util.concurrent.Callable<T> second) throws Exception {
        var executor = Executors.newFixedThreadPool(2);
        var ready = new CountDownLatch(2); var start = new CountDownLatch(1);
        List<Future<T>> futures = new ArrayList<>(); List<T> results = new ArrayList<>(); List<Executable> errors = new ArrayList<>();
        long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(20);
        try {
            for (var action : List.of(first, second)) {
                futures.add(executor.submit(() -> {
                    ready.countDown();
                    if (!start.await(5, TimeUnit.SECONDS)) { throw new AssertionError("mock并发起跑超时"); }
                    return action.call();
                }));
            }
            assertTrue(ready.await(5, TimeUnit.SECONDS)); start.countDown();
            for (var future : futures) {
                try { results.add(future.get(Math.max(1, deadline - System.nanoTime()), TimeUnit.NANOSECONDS)); }
                catch (ExecutionException failure) { errors.add(() -> { throw failure.getCause(); }); }
                catch (java.util.concurrent.TimeoutException timeout) { future.cancel(true); errors.add(() -> { throw timeout; }); }
            }
            assertAll("全部Future错误在主线程收集", errors);
            return results;
        } finally {
            start.countDown();
            for (var future : futures) { if (!future.isDone()) { future.cancel(true); } }
            executor.shutdownNow();
            assertTrue(executor.awaitTermination(5, TimeUnit.SECONDS), "本例executor清理超时");
        }
    }
}
