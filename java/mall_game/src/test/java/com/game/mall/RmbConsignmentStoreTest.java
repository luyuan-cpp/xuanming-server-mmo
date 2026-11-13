package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;

import java.time.Clock;
import java.time.Instant;
import java.time.ZoneId;
import java.time.ZoneOffset;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicLong;
import java.util.function.LongSupplier;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.function.Executable;
import org.springframework.beans.factory.support.StaticListableBeanFactory;

/** 每例独立随机库；commerce.it.tidb显式启用真实TiDB，默认只证明H2事务契约。 */
class RmbConsignmentStoreTest {
    @Test void 买家修订随原购买冻结_同账号ABA不能重绑原请求_历史仍归原账号() {
        try (var f = new Fixture()) {
            f.register(1, 101);
            var original = new RmbConsignmentApi.Buy("1", "201", BUYER, "buyer-revision-original", "18446744073709551615");
            var order = f.orders.createConsignment(original);
            assertEquals(order, f.orders.createConsignment(original));
            rejects(409, () -> f.orders.createConsignment(new RmbConsignmentApi.Buy("1", "201", BUYER, original.requestId(), "9")));
            rejects(409, () -> f.orders.createConsignment(new RmbConsignmentApi.Buy("1", "201", "新买家", original.requestId(), "8")));
            assertEquals(1, f.db.count("oms_order"));
            f.pay(order);
            var intent = f.rmb.deliveries(new DeliveryApi.Poll(20)).entries().getFirst().intent();
            assertEquals(BUYER, intent.buyerAccount());
            assertEquals("18446744073709551615", intent.buyerOwnerRevision());
            assertEquals(order.orderId(), f.orders.getConsignment(new RmbConsignmentApi.OrderQuery(BUYER, order.orderId())).orderId());
            assertEquals(intent, f.rmb.deliveries(new DeliveryApi.Poll(20)).entries().getFirst().intent());
        }
    }

    @Test void 买家修订缺失越界或非规范时不能占用寄售_损坏原件不能交付() {
        try (var f = new Fixture()) {
            f.register(1, 101);
            for (String revision : new String[]{null, "", "0", "01", "+1", "18446744073709551616"}) {
                rejects(400, () -> f.orders.createConsignment(new RmbConsignmentApi.Buy("1", "201", BUYER, "invalid-buyer-revision", revision)));
            }
            assertEquals(0, f.db.count("oms_order"));
            assertEquals("LISTED", f.rmb.get(new RmbConsignmentApi.Get("1")).state());
            var order = f.buy(1, 1); f.pay(order);
            f.db.jdbc.update("UPDATE commerce_consignment_order SET buyer_owner_revision=? WHERE order_sn=?",
                "01".getBytes(java.nio.charset.StandardCharsets.US_ASCII), order.orderId());
            rejects(503, () -> f.rmb.deliveries(new DeliveryApi.Poll(20)));
            assertEquals(0, f.db.count("commerce_seller_income"));
        }
    }

    static final String SELLER = " 原卖家账号 ";
    static final String BUYER = "买家账号";
    static final String PROOF = "a".repeat(64);

    @Test void 同一OMS付款与可信终局产生一次收入_费率向上取整_冷静期后真实mock出款() {
        try (var f = new Fixture()) {
            f.rules.setNoticeMs(1_000); f.rules.setCoolingMs(2_000); f.rules.setFeeBps(500);
            var listing = f.register(1, 101);
            assertEquals("PUBLIC_NOTICE", listing.phase());
            rejects(409, () -> f.buy(1, 1));
            f.clock.advance(1_000);
            var order = f.buy(1, 1);
            assertEquals(order, f.buy(1, 1));
            assertEquals(0, order.skuId()); assertEquals(101, order.amountMinor());
            // 登记后的配置变更不能改原合同费率或冷静期。
            f.rules.setFeeBps(9_999); f.rules.setCoolingMs(0);
            var paid = f.pay(order);
            assertEquals("DELIVERY_PENDING", paid.state());
            assertEquals(1, f.db.count("commerce_mock_transaction"));
            assertEquals(1, f.db.count("commerce_payment_receipt"));
            assertEquals(0, f.db.count("commerce_seller_income"));
            assertTrue(f.delivery.poll(new DeliveryApi.Poll(20)).entries().isEmpty(), "托管资产不能混入官方SKU交付");
            var intent = f.rmb.deliveries(new DeliveryApi.Poll(20)).entries().getFirst().intent();
            assertEquals(order.orderId(), intent.orderId()); assertEquals(BUYER, intent.buyerAccount());
            assertEquals("10001", intent.escrowReference()); assertEquals(PROOF, intent.escrowProof());
            f.deliver(order, "DELIVERED"); f.deliver(order, "DELIVERED");
            assertEquals("DELIVERED", f.orders.getConsignment(new RmbConsignmentApi.OrderQuery(BUYER, order.orderId())).state());
            var income = f.rmb.incomes(new RmbConsignmentApi.IncomePage(SELLER, "", 100)).incomes().getFirst();
            assertEquals(new RmbConsignmentApi.Income(order.orderId(), 101, 6, 95, 0, 0, f.clock.millis() + 2_000), income);
            assertTrue(f.rmb.incomes(new RmbConsignmentApi.IncomePage(SELLER.trim(), "", 100)).incomes().isEmpty());
            rejects(409, () -> f.rmb.withdraw(new RmbConsignmentApi.Withdraw(SELLER, "withdraw-cooling", 95)));
            assertEquals(0, f.db.count("commerce_withdrawal"));
            f.clock.advance(1_999);
            rejects(409, () -> f.rmb.withdraw(new RmbConsignmentApi.Withdraw(SELLER, "withdraw-cooling", 95)));
            f.clock.advance(1);
            var command = new RmbConsignmentApi.Withdraw(SELLER, "withdraw-success", 95);
            var withdrawal = f.rmb.withdraw(command);
            assertEquals(withdrawal, f.rmb.withdraw(command));
            rejects(409, () -> f.rmb.withdraw(new RmbConsignmentApi.Withdraw(SELLER, command.requestId(), 94)));
            rejects(404, () -> f.rmb.getWithdrawal(new RmbConsignmentApi.WithdrawalQuery(SELLER.trim(), withdrawal.withdrawalId())));
            var payment = new MockPayoutStore.Pay(withdrawal.withdrawalId(), "local-payout-success");
            var settled = f.payout.pay(payment);
            assertEquals("PAID", settled.state()); assertEquals("mock", settled.channel());
            assertEquals(settled, f.payout.pay(payment));
            assertEquals(settled, f.newPayout(() -> 0).pay(payment));
            assertEquals(1, f.db.count("commerce_mock_payout"));
            assertEquals(1, f.db.count("commerce_withdrawal_receipt"));
            assertEquals(95, f.db.value("commerce_seller_income", "paid_fen"));
            assertEquals(0, f.db.value("commerce_seller_income", "reserved_fen"));
        }
    }

    @Test void 取消再买后旧付款只退款_原账号保留订单权限_交付失败不产生收入() {
        try (var f = new Fixture()) {
            f.register(1, 333);
            var old = f.buy(1, 1);
            rejects(404, () -> f.orders.cancelConsignment(new RmbConsignmentApi.OrderQuery("新账号", old.orderId())));
            assertEquals("CANCELLED", f.orders.cancelConsignment(new RmbConsignmentApi.OrderQuery(BUYER, old.orderId())).state());
            var current = f.orders.createConsignment(new RmbConsignmentApi.Buy("1", "202", "新账号", "new-account-buy-1", "1"));
            assertNotEquals(old.orderId(), current.orderId());
            assertEquals("REFUND_PENDING", f.pay(old).state());
            assertEquals(current.orderId(), f.rmb.list(new RmbConsignmentApi.Page("", 100)).listings().getFirst().orderId());
            assertEquals(0, f.db.count("commerce_delivery_intent"));
            assertEquals(1, f.payment.reconcile().succeeded());
            assertEquals("REFUNDED", f.orders.getConsignment(new RmbConsignmentApi.OrderQuery(BUYER, old.orderId())).state());
            rejects(404, () -> f.orders.getConsignment(new RmbConsignmentApi.OrderQuery("新账号", old.orderId())));
            rejects(404, () -> f.orders.get(old.buyerId(), old.orderId()));
            rejects(404, () -> f.orders.cancel(old.buyerId(), old.orderId()));
            assertEquals(List.of(old.orderId()), f.orders.listConsignments(new RmbConsignmentApi.OrderPage(BUYER, "", 100)).stream().map(CommerceApi.Order::orderId).toList());
            f.pay(current); f.deliver(current, "REFUND_PENDING");
            assertEquals(0, f.db.count("commerce_seller_income"));
            assertEquals(1, f.payment.reconcile().succeeded());
            assertEquals(2, f.db.count("commerce_refund_receipt"));
            var pendingReturn = f.rmb.returns(new DeliveryApi.Poll(20)).entries().getFirst().intent();
            assertEquals("1", pendingReturn.listingId());
            var returned = new RmbConsignmentApi.ReturnReceipt("1", "10001", PROOF, "b".repeat(64));
            f.rmb.returned(returned); f.rmb.returned(returned);
            rejects(409, () -> f.rmb.returned(new RmbConsignmentApi.ReturnReceipt("1", "10001", PROOF, "c".repeat(64))));
            assertTrue(f.rmb.returns(new DeliveryApi.Poll(20)).entries().isEmpty());
            assertEquals(0, f.db.count("pms_sku_stock"), "唯一托管资产不应创建第二份PMS库存");
        }
    }

    @Test void 双实例并发提现不可双花_失败回执返还预留_未知证明不成功() throws Exception {
        try (var f = new Fixture()) {
            f.register(1, 101); var order = f.buy(1, 1); f.pay(order); f.deliver(order, "DELIVERED");
            var second = new RmbConsignmentStore(f.db.jdbc, f.db.manager, f.clock, f.rules, f.payout);
            var outcomes = parallel(
                () -> attemptWithdrawal(f.rmb, "parallel-withdraw-1"),
                () -> attemptWithdrawal(second, "parallel-withdraw-2"));
            assertEquals(1, outcomes.stream().filter(RmbConsignmentApi.Withdrawal.class::isInstance).count());
            assertEquals(1, outcomes.stream().filter(value -> value instanceof CommerceException error && error.status() == 409).count());
            var original = (RmbConsignmentApi.Withdrawal) outcomes.stream().filter(RmbConsignmentApi.Withdrawal.class::isInstance).findFirst().orElseThrow();
            assertEquals(95, f.db.value("commerce_seller_income", "reserved_fen"));
            var unknown = new RmbConsignmentApi.WithdrawalReceipt(original.withdrawalId(), "mock", "unknown-fact", 95, "CNY", true);
            rejects(409, () -> f.rmb.completeWithdrawal(unknown));
            assertEquals("REQUESTED", f.rmb.getWithdrawal(new RmbConsignmentApi.WithdrawalQuery(SELLER, original.withdrawalId())).state());
            var noVerifier = new RmbConsignmentStore(f.db.jdbc, f.db.manager, f.clock, f.rules, (RmbConsignmentStore.PayoutEvidenceVerifier) null);
            rejects(503, () -> noVerifier.completeWithdrawal(unknown));
            assertEquals(0, f.db.count("commerce_withdrawal_receipt"));
            // 明确的本地提供方失败原件；并非真实渠道失败或调用方自报证明。
            String tx = "mock-payout-" + java.util.UUID.randomUUID();
            f.db.jdbc.update("INSERT INTO commerce_mock_payout(withdrawal_id,request_id,transaction_id,amount_fen,successful,created_ms) VALUES(?,?,?,?,0,?)",
                original.withdrawalId(), bytes("failed-provider-request"), bytes(tx), 95, f.clock.millis());
            var failed = new RmbConsignmentApi.WithdrawalReceipt(original.withdrawalId(), "mock", tx, 95, "CNY", false);
            assertEquals("FAILED", f.rmb.completeWithdrawal(failed).state());
            assertEquals("FAILED", f.rmb.completeWithdrawal(failed).state());
            assertEquals(0, f.db.value("commerce_seller_income", "reserved_fen"));
            assertEquals(0, f.db.value("commerce_seller_income", "paid_fen"));
            assertEquals(95, f.rmb.withdraw(new RmbConsignmentApi.Withdraw(SELLER, "after-failed-withdraw", 95)).amountFen());
        }
    }

    @Test void 固定上界同毫秒分页_前二十未交付不挡尾五_新插入留待绕回() {
        try (var f = new Fixture()) {
            for (int i = 1; i <= 25; i++) { f.register(i, 101); f.pay(f.buy(i, i)); }
            var sorted = f.db.jdbc.queryForList("SELECT order_sn FROM commerce_delivery_intent ORDER BY created_ms,order_sn", String.class);
            var first = f.rmb.deliveries(new DeliveryApi.Poll(20));
            assertEquals(sorted.subList(0, 20), first.entries().stream().map(row -> row.intent().orderId()).toList());
            String cursor = first.entries().getLast().cursor(); assertFalse(cursor.isEmpty());
            f.clock.advance(1); f.register(26, 101); var newer = f.buy(26, 26); f.pay(newer);
            var tail = f.rmb.deliveries(new DeliveryApi.Poll(20, cursor));
            assertEquals(sorted.subList(20, 25), tail.entries().stream().map(row -> row.intent().orderId()).toList());
            assertEquals("", tail.entries().getLast().cursor());
            for (var row : tail.entries()) { f.deliver(f.orders.paymentOrderView(row.intent().orderId()), "DELIVERED"); }
            assertEquals(5, f.db.count("commerce_seller_income"));
            assertEquals(21, f.db.jdbc.queryForObject("SELECT COUNT(*) FROM commerce_delivery_intent WHERE status='PENDING'", Integer.class));
            var restart = new RmbConsignmentStore(f.db.jdbc, f.db.manager, f.clock, f.rules, f.payout);
            assertTrue(restart.deliveries(new DeliveryApi.Poll(20, cursor)).entries().isEmpty(), "旧上界后新增单留待绕回");
            var wrapped = restart.deliveries(new DeliveryApi.Poll(20));
            assertEquals(sorted.subList(0, 20), wrapped.entries().stream().map(row -> row.intent().orderId()).toList());
            assertEquals(newer.orderId(), restart.deliveries(new DeliveryApi.Poll(20, wrapped.entries().getLast().cursor())).entries().getFirst().intent().orderId());
            for (String bad : List.of("v2:0:x:0:y", " v1:0:x:0:y", "v1:00:00000000-0000-0000-0000-000000000000:1:00000000-0000-0000-0000-000000000000")) {
                rejects(400, () -> restart.deliveries(new DeliveryApi.Poll(20, bad)));
            }
        }
    }

    @Test void 托管登记幂等与资产限制_公示到期退回_金额边界不溢出() {
        try (var f = new Fixture()) {
            f.rules.setSaleMs(10);
            var original = f.register(1, 101);
            assertEquals(original, f.register(1, 101));
            rejects(409, () -> f.register(1, 102));
            rejects(409, () -> f.orders.createConsignment(new RmbConsignmentApi.Buy("1", "201", SELLER, "same-account-buy", "1")));
            rejects(400, () -> f.rmb.register(new RmbConsignmentApi.Escrow("2", "101", SELLER, 1, "CURRENCY", 2, "10002", PROOF, 100, "绑定币")));
            assertEquals(1, RmbConsignmentStore.fee(1, 1));
            assertEquals(CommerceStore.MAX_AMOUNT_MINOR, RmbConsignmentStore.fee(CommerceStore.MAX_AMOUNT_MINOR, 10_000));
            assertEquals(0, RmbConsignmentStore.fee(CommerceStore.MAX_AMOUNT_MINOR, 0));
            f.clock.advance(10);
            assertTrue(f.rmb.list(new RmbConsignmentApi.Page("", 100)).listings().isEmpty());
            rejects(409, () -> f.buy(1, 1));
            assertEquals("1", f.rmb.returns(new DeliveryApi.Poll(20)).entries().getFirst().intent().listingId());
        }
    }

    /**
     * 同账号自买的入口只有这一处：下单事务里的 reserve 在建订单、占用寄售之前按账号拒绝，不看角色号。
     * trade 的交付（go/trade/internal/commerce/rmb.go 的 deliver）对「买家账号等于卖家账号」的已付款意图只拒绝、没有退款出口，
     * 靠的就是这种订单建不出来；要放开这条判断，先给 trade 的交付补上退款分支。
     */
    @Test void 同账号换一个角色也不能买自己的寄售_不建订单不占寄售_别的账号照常可买() {
        try (var f = new Fixture()) {
            f.register(1, 101);
            // 卖家角色是 101；同一个账号用另一个角色 202 来买。
            var selfBuy = new RmbConsignmentApi.Buy("1", "202", SELLER, "self-buy-other-role", "1");
            rejects(409, () -> f.orders.createConsignment(selfBuy));
            // 同一个请求号重放也一样被拒：第一次没有留下可重放的订单。
            rejects(409, () -> f.orders.createConsignment(selfBuy));
            assertEquals(0, f.db.count("oms_order"));
            assertEquals(0, f.db.count("commerce_consignment_order"));
            assertEquals(0, f.db.count("commerce_delivery_intent"));
            var listing = f.rmb.get(new RmbConsignmentApi.Get("1"));
            assertEquals("LISTED", listing.state());
            assertNull(listing.orderId());
            // 拒绝没有留下占用：别的账号照常可买。
            var order = f.buy(1, 1);
            assertEquals("AWAITING_PAYMENT", order.state());
            assertEquals("LOCKED", f.rmb.get(new RmbConsignmentApi.Get("1")).state());
            assertEquals(order.orderId(), f.rmb.get(new RmbConsignmentApi.Get("1")).orderId());
        }
    }

    @Test void 交付收据落库失败时收入与OMS一同回滚_恢复后只结一次() {
        try (var f = new Fixture()) {
            f.register(1, 101); var order = f.buy(1, 1); f.pay(order);
            var reached = new java.util.concurrent.atomic.AtomicBoolean();
            // 在真实同事务income INSERT之后、receipt INSERT之前注入存储失败；不在纯预检处提前失败。
            var failing = new org.springframework.jdbc.core.JdbcTemplate(f.db.source) {
                @Override public int update(String sql, Object... args) {
                    if (sql.startsWith("INSERT INTO commerce_delivery_receipt")) {
                        assertEquals(1, f.db.count("commerce_seller_income")); reached.set(true);
                        throw new org.springframework.dao.DataAccessResourceFailureException("本例receipt写入失败");
                    }
                    return super.update(sql, args);
                }
            };
            var broken = new DeliveryStore(failing, f.db.manager, f.db.props, f.rmb);
            assertThrows(org.springframework.dao.DataAccessException.class, () -> broken.receipt(new DeliveryApi.Receipt(order.orderId(), "9001", "DELIVERED", PROOF)));
            assertTrue(reached.get());
            assertEquals(0, f.db.count("commerce_seller_income"));
            assertEquals("DELIVERY_PENDING", f.orders.getConsignment(new RmbConsignmentApi.OrderQuery(BUYER, order.orderId())).state());
            assertEquals("PENDING", f.db.jdbc.queryForObject("SELECT status FROM commerce_delivery_intent WHERE order_sn=?", String.class, order.orderId()));
            f.deliver(order, "DELIVERED"); f.deliver(order, "DELIVERED");
            assertEquals(1, f.db.count("commerce_seller_income")); assertEquals(1, f.db.count("commerce_delivery_receipt"));
            assertEquals(95, f.db.value("commerce_seller_income", "net_fen"));
        }
    }

    @Test void 退回分页固定上界_尾项已终结返回空页_绕回才能见新增单() {
        try (var f = new Fixture()) {
            for (int i = 1; i <= 25; i++) { f.register(i, 101); f.rmb.cancel(new RmbConsignmentApi.Cancel(Integer.toString(i), SELLER)); }
            var first = f.rmb.returns(new DeliveryApi.Poll(20));
            assertEquals(20, first.entries().size());
            String cursor = first.entries().getLast().cursor(); assertFalse(cursor.isEmpty());
            f.register(999, 101); f.rmb.cancel(new RmbConsignmentApi.Cancel("999", SELLER));
            var tail = f.rmb.returns(new DeliveryApi.Poll(20, cursor));
            assertEquals(5, tail.entries().size()); assertEquals("", tail.entries().getLast().cursor());
            assertTrue(tail.entries().stream().noneMatch(row -> row.intent().listingId().equals("999")));
            for (var entry : tail.entries()) {
                var value = entry.intent();
                f.rmb.returned(new RmbConsignmentApi.ReturnReceipt(value.listingId(), value.escrowReference(), value.escrowProof(), "c".repeat(64)));
            }
            assertTrue(f.rmb.returns(new DeliveryApi.Poll(20, cursor)).entries().isEmpty());
            var wrapped = f.rmb.returns(new DeliveryApi.Poll(20));
            assertEquals(20, wrapped.entries().size());
            assertEquals("999", f.rmb.returns(new DeliveryApi.Poll(20, wrapped.entries().getLast().cursor())).entries().getFirst().intent().listingId());
            assertEquals(5, f.db.count("commerce_consignment_return"));
        }
    }

    @Test void 跨两笔收入分配只预留原额_原终局核清分配后保留剩余可用收入() {
        try (var f = new Fixture()) {
            f.register(1, 101); var first = f.buy(1, 1); f.pay(first); f.deliver(first, "DELIVERED");
            f.register(2, 201); var second = f.buy(2, 2); f.pay(second); f.deliver(second, "DELIVERED");
            assertEquals(285, f.db.value("commerce_seller_income", "net_fen"));
            var withdrawal = f.rmb.withdraw(new RmbConsignmentApi.Withdraw(SELLER, "two-income-allocation", 200));
            assertEquals(2, f.db.count("commerce_withdrawal_allocation"));
            assertEquals(200, f.db.value("commerce_withdrawal_allocation", "amount_fen"));
            assertEquals(200, f.db.value("commerce_seller_income", "reserved_fen"));
            assertEquals(0, f.db.jdbc.queryForObject("SELECT COUNT(*) FROM commerce_withdrawal_allocation a JOIN commerce_seller_income i ON i.order_sn=a.order_sn WHERE a.amount_fen>i.net_fen", Integer.class));
            f.payout.pay(new MockPayoutStore.Pay(withdrawal.withdrawalId(), "two-income-payout"));
            assertEquals(200, f.db.value("commerce_seller_income", "paid_fen"));
            assertEquals(0, f.db.value("commerce_seller_income", "reserved_fen"));
            assertEquals(85, f.rmb.withdraw(new RmbConsignmentApi.Withdraw(SELLER, "remaining-allocation", 85)).amountFen());
            rejects(409, () -> f.rmb.withdraw(new RmbConsignmentApi.Withdraw(SELLER, "overdraw-after-rest", 1)));
        }
    }

    private static Object attemptWithdrawal(RmbConsignmentStore store, String request) {
        try { return store.withdraw(new RmbConsignmentApi.Withdraw(SELLER, request, 95)); }
        catch (CommerceException rejected) { return rejected; }
    }
    static void rejects(int status, Executable action) { assertEquals(status, assertThrows(CommerceException.class, action).status()); }
    static byte[] bytes(String value) { return value.getBytes(java.nio.charset.StandardCharsets.UTF_8); }
    static <T> List<T> parallel(java.util.concurrent.Callable<T> first, java.util.concurrent.Callable<T> second) throws Exception {
        var executor = Executors.newFixedThreadPool(2);
        var ready = new CountDownLatch(2); var start = new CountDownLatch(1);
        List<Future<T>> futures = new ArrayList<>(); List<T> results = new ArrayList<>(); List<Executable> errors = new ArrayList<>();
        long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(20);
        try {
            for (var action : List.of(first, second)) {
                futures.add(executor.submit(() -> {
                    ready.countDown();
                    if (!start.await(5, TimeUnit.SECONDS)) { throw new AssertionError("本例并发起跑超时"); }
                    return action.call();
                }));
            }
            assertTrue(ready.await(5, TimeUnit.SECONDS)); start.countDown();
            for (var future : futures) {
                try { results.add(future.get(Math.max(1, deadline - System.nanoTime()), TimeUnit.NANOSECONDS)); }
                catch (java.util.concurrent.ExecutionException failure) { errors.add(() -> { throw failure.getCause(); }); }
                catch (java.util.concurrent.TimeoutException timeout) { future.cancel(true); errors.add(() -> { throw timeout; }); }
            }
            assertAll("两个worker错误均在主线程收集", errors);
            return results;
        } finally {
            start.countDown();
            for (var future : futures) { if (!future.isDone()) { future.cancel(true); } }
            executor.shutdownNow(); assertTrue(executor.awaitTermination(5, TimeUnit.SECONDS), "本例并发执行器未停止");
        }
    }
    static final class MutableClock extends Clock {
        private final AtomicLong now = new AtomicLong(1_760_000_000_000L);
        void advance(long millis) { now.addAndGet(millis); }
        @Override public ZoneId getZone() { return ZoneOffset.UTC; }
        @Override public Clock withZone(ZoneId zone) { if (!ZoneOffset.UTC.equals(zone)) { throw new IllegalArgumentException("测试仅支持UTC"); } return this; }
        @Override public long millis() { return now.get(); }
        @Override public Instant instant() { return Instant.ofEpochMilli(millis()); }
    }
    static final class Fixture implements AutoCloseable {
        final CommerceFixture db = new CommerceFixture();
        final MutableClock clock = new MutableClock();
        final RmbConsignmentProperties rules = new RmbConsignmentProperties();
        final StaticListableBeanFactory beans = new StaticListableBeanFactory();
        final MockPayoutStore payout;
        final RmbConsignmentStore rmb;
        final CommerceStore orders;
        final DeliveryStore delivery;
        final MockPaymentStore payment;
        Fixture() {
            db.props.setRunMode("test"); db.props.setMockPaymentEnabled(true); db.props.validate();
            rules.setNoticeMs(0); rules.setCoolingMs(0); rules.validate();
            payout = newPayout(() -> 0);
            rmb = new RmbConsignmentStore(db.jdbc, db.manager, clock, rules, payout);
            beans.addBean("settlements", rmb);
            orders = new CommerceStore(db.jdbc, db.manager, clock, db.props, rmb);
            delivery = new DeliveryStore(db.jdbc, db.manager, db.props, rmb);
            payment = new MockPaymentStore(db.jdbc, db.manager, clock, db.props, orders, delivery, () -> 0);
        }
        MockPayoutStore newPayout(LongSupplier nanos) { return new MockPayoutStore(db.jdbc, db.manager, db.props, beans.getBeanProvider(RmbConsignmentStore.class), clock, nanos); }
        RmbConsignmentApi.Listing register(int id, long price) {
            return rmb.register(new RmbConsignmentApi.Escrow(Integer.toString(id), "101", SELLER, 1, "ITEMS", null,
                Integer.toString(10_000 + id), PROOF, price, "唯一托管资产"));
        }
        CommerceApi.Order buy(int listing, int buyer) { return orders.createConsignment(new RmbConsignmentApi.Buy(Integer.toString(listing), Integer.toString(200 + buyer), BUYER, "consignment-buy-" + listing, "1")); }
        CommerceApi.Order pay(CommerceApi.Order order) { return payment.pay(new MockPaymentApi.Pay(order.orderId(), "mock-pay-" + order.orderId())); }
        void deliver(CommerceApi.Order order, String state) { delivery.receipt(new DeliveryApi.Receipt(order.orderId(), "9001", state, PROOF)); }
        RmbConsignmentApi.Withdrawal earned(int id) { register(id, 101); var order = buy(id, id); pay(order); deliver(order, "DELIVERED"); return rmb.withdraw(new RmbConsignmentApi.Withdraw(SELLER, "earned-withdraw-" + id, 95)); }
        @Override public void close() { db.close(); }
    }
}
