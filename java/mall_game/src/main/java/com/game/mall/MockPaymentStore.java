package com.game.mall;

import java.nio.charset.StandardCharsets;
import java.time.Clock;
import java.util.Objects;
import java.util.UUID;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.function.LongSupplier;
import java.util.function.Supplier;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.context.annotation.DependsOn;
import org.springframework.dao.DuplicateKeyException;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Component;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.TransactionDefinition;
import org.springframework.transaction.support.TransactionTemplate;

/** 本地提供方事实先独立提交，再投递既有OMS；重放只投递同一原件，不生成第二笔资金事实。 */
@Component
@DependsOn("commerceSchemaReady")
public final class MockPaymentStore {
    private static final Logger LOG = LoggerFactory.getLogger(MockPaymentStore.class);
    private static final int MAX_ATTEMPTS = 40;
    private static final long MIN_START_NANOS = TimeUnit.MILLISECONDS.toNanos(250);
    private static final String PAYMENT_SCAN = "FROM commerce_mock_transaction m WHERE m.kind='PAYMENT' AND m.applied=0";
    // 非mock渠道和不匹配原件的回执不属于本adapter。未标applied的退款也要扫描，覆盖OMS已提交后崩溃。
    private static final String REFUND_SCAN = "FROM commerce_payment_receipt p JOIN commerce_mock_transaction m "
        + "ON m.order_sn=p.order_sn AND m.kind='PAYMENT' AND m.transaction_id=p.transaction_id "
        + "AND m.amount_minor=p.amount_minor AND m.currency=p.currency WHERE p.channel=? AND "
        + "(EXISTS(SELECT 1 FROM commerce_refund_intent r WHERE r.channel=p.channel AND r.transaction_id=p.transaction_id AND r.status='PENDING') "
        + "OR EXISTS(SELECT 1 FROM commerce_mock_transaction f WHERE f.kind='REFUND' AND f.order_sn=m.order_sn AND f.applied=0))";
    private final JdbcTemplate jdbc;
    private final PlatformTransactionManager manager;
    private final CommerceProperties props;
    private final CommerceStore orders;
    private final DeliveryStore deliveries;
    private final PaymentPolicy policy;
    private final Clock clock;
    private final LongSupplier nanoTime;
    // 仅一个扫描者拥有这两个游标；多个实例仍由原件/OMS唯一键完成幂等，不破坏性领取。
    private final AtomicBoolean scanning = new AtomicBoolean();
    private final Cursor[] cursors = {new Cursor(), new Cursor()};
    private int nextPhase;

    @Autowired
    public MockPaymentStore(JdbcTemplate jdbc, PlatformTransactionManager manager, Clock clock,
            CommerceProperties props, CommerceStore orders, DeliveryStore deliveries) {
        this(jdbc, manager, clock, props, orders, deliveries, System::nanoTime);
    }
    MockPaymentStore(JdbcTemplate jdbc, PlatformTransactionManager manager, Clock clock,
            CommerceProperties props, CommerceStore orders, DeliveryStore deliveries, LongSupplier nanoTime) {
        this.jdbc = new JdbcTemplate(Objects.requireNonNull(jdbc.getDataSource()));
        this.jdbc.setQueryTimeout(5);
        this.manager = manager; this.props = props; this.clock = clock;
        this.orders = orders; this.deliveries = deliveries; this.nanoTime = nanoTime;
        policy = new PaymentPolicy(this.jdbc, props);
    }

    public CommerceApi.Order pay(MockPaymentApi.Pay input) {
        props.requireMockPayment();
        if (input == null || input.orderId() == null || !input.orderId().matches(PaymentPolicy.ORDER)
                || input.requestId() == null || !input.requestId().matches("[A-Za-z0-9_-]{8,128}")) {
            throw new CommerceException(400, "mock付款参数非法");
        }
        var budget = new Budget(15);
        try {
            var fact = transaction(budget, () -> paymentFact(input));
            return applyPayment(fact, budget);
        }
        catch (DuplicateKeyException conflict) {
            throw new CommerceException(409, "mock请求号或原订单已绑定其他交易");
        } catch (CommerceException rejected) { throw rejected; }
        catch (RuntimeException unavailable) {
            LOG.warn("本地mock付款失败 type={}", unavailable.getClass().getSimpleName());
            throw new CommerceException(503, "mock付款暂不可用，已提交原件保留待恢复");
        }
    }

    private PaymentPolicy.Fact paymentFact(MockPaymentApi.Pay input) {
        var frozen = frozen(input.orderId());
        var prior = policy.find("PAYMENT", input.orderId(), true);
        if (prior != null) {
            if (!prior.requestId().equals(input.requestId())) { throw new CommerceException(409, "原订单已绑定其他mock请求号"); }
            requireAmount(prior, frozen);
            return prior;
        }
        if (frozen.state() != 0 && frozen.state() != 4
                || jdbc.queryForObject("SELECT COUNT(*) FROM commerce_payment_receipt WHERE order_sn=?", Integer.class, input.orderId()) != 0) {
            throw new CommerceException(409, "原订单已有付款或不可付款");
        }
        String tx = "mock-pay-" + UUID.randomUUID();
        insert(new PaymentPolicy.Fact("PAYMENT", input.orderId(), input.requestId(), tx, tx,
            frozen.amount(), frozen.currency(), clock.millis(), 0));
        return policy.required("PAYMENT", input.orderId());
    }

    private CommerceApi.Order applyPayment(PaymentPolicy.Fact fact, Budget budget) {
        props.requireMockPayment();
        // 三个阶段各自REQUIRES_NEW；任何外层事务也不能把提供方原件与OMS提交合并，崩溃后可恢复。
        var order = transaction(budget, () -> orders.recordTrustedPayment(fact.receipt()));
        markApplied(fact, budget);
        return order;
    }

    private void refund(String orderId, Budget budget) {
        var fact = transaction(budget, () -> {
            var frozen = frozen(orderId);
            var payment = policy.ownedPayment(orderId);
            requireAmount(payment, frozen);
            var prior = policy.find("REFUND", orderId, true);
            if (prior != null) {
                requireAmount(prior, frozen);
                if (!prior.paymentTransactionId().equals(payment.transactionId())) { throw new CommerceException(503, "mock退款原付款绑定损坏"); }
                return prior;
            }
            // 退款待办按被退付款的交易键识别;mock 只退订单绑定的那笔 mock 付款。
            if (frozen.state() != 5 || jdbc.queryForObject(
                    "SELECT COUNT(*) FROM commerce_refund_intent WHERE channel=? AND transaction_id=? AND status='PENDING'", Integer.class,
                    ascii("mock"), ascii(payment.transactionId())) != 1) {
                throw new CommerceException(409, "原订单不在待退款状态");
            }
            insert(new PaymentPolicy.Fact("REFUND", orderId, "refund_" + orderId.replace("-", ""),
                "mock-refund-" + UUID.randomUUID(), payment.transactionId(), payment.amountMinor(), payment.currency(), clock.millis(), 0));
            return policy.required("REFUND", orderId);
        });
        transaction(budget, () -> { deliveries.refunded(fact.refundReceipt()); return Boolean.TRUE; });
        markApplied(fact, budget);
    }

    public MockPaymentApi.Reconcile reconcile() {
        props.requireMockPayment();
        if (!scanning.compareAndSet(false, true)) { return new MockPaymentApi.Reconcile(0, 0, 0, 0, true, false); }
        int attempted = 0, succeeded = 0, failed = 0, scanFailures = 0;
        var budget = new Budget(5);
        boolean[] drained = {false, false};
        try {
            while (attempted < MAX_ATTEMPTS && !(drained[0] && drained[1]) && budget.canStart()) {
                int phase = nextPhase;
                nextPhase = 1 - phase; // 跨调用也交替，慢付款不能永久挤掉退款阶段。
                if (drained[phase]) { continue; }
                String orderId;
                try { orderId = next(phase); }
                catch (RuntimeException unavailable) {
                    scanFailures++; drained[phase] = true;
                    LOG.warn("本地mock扫描失败 phase={} type={}", phase, unavailable.getClass().getSimpleName());
                    continue;
                }
                if (orderId == null) { drained[phase] = true; continue; }
                if (!budget.canStart()) { break; } // 已读取但尚未开始的项不前移，下次仍能取到。
                attempted++;
                try {
                    if (!orderId.matches(PaymentPolicy.ORDER)) { throw new CommerceException(503, "mock扫描订单标识损坏"); }
                    if (phase == 0) { applyPayment(policy.required("PAYMENT", orderId), budget); }
                    else { refund(orderId, budget); }
                    succeeded++;
                } catch (RuntimeException unavailable) {
                    failed++;
                    LOG.warn("本地mock投递失败 phase={} type={}", phase, unavailable.getClass().getSimpleName());
                } finally {
                    cursors[phase].after = orderId; // 失败保留原件，先让本轮后项获得机会；到上界后再绕回。
                }
            }
            return new MockPaymentApi.Reconcile(attempted, succeeded, failed, scanFailures, false, !budget.canStart());
        } finally { scanning.set(false); }
    }

    private String next(int phase) {
        var cursor = cursors[phase];
        String from = phase == 0 ? PAYMENT_SCAN : REFUND_SCAN;
        Object[] channel = phase == 0 ? new Object[0] : new Object[]{ascii("mock")};
        if (cursor.upper == null) {
            var upper = jdbc.query("SELECT m.order_sn " + from + " ORDER BY m.order_sn DESC LIMIT 1", (row, n) -> row.getString(1), channel);
            if (upper.isEmpty()) { cursor.reset(); return null; }
            cursor.upper = upper.getFirst();
        }
        Object[] args = phase == 0 ? new Object[]{cursor.after, cursor.upper} : new Object[]{ascii("mock"), cursor.after, cursor.upper};
        var rows = jdbc.query("SELECT m.order_sn " + from + " AND m.order_sn>? AND m.order_sn<=? ORDER BY m.order_sn LIMIT 1",
            (row, n) -> row.getString(1), args);
        if (rows.isEmpty()) { cursor.reset(); return null; }
        return rows.getFirst();
    }

    private record Frozen(int state, long amount, String currency) {}
    private Frozen frozen(String orderId) {
        var rows = jdbc.query("SELECT status,pay_amount,currency FROM oms_order WHERE order_sn=? FOR UPDATE",
            (row, n) -> new Frozen(row.getInt(1), row.getLong(2), row.getString(3)), orderId);
        if (rows.size() != 1) { throw new CommerceException(404, "原订单不存在"); }
        var frozen = rows.getFirst();
        if (frozen.amount() < 1 || frozen.amount() > CommerceStore.MAX_AMOUNT_MINOR
                || frozen.currency() == null || !frozen.currency().matches("[A-Z][A-Z0-9_]{2,15}")) {
            throw new CommerceException(503, "冻结订单金额或币种损坏");
        }
        return frozen;
    }
    private static void requireAmount(PaymentPolicy.Fact fact, Frozen frozen) {
        if (fact.amountMinor() != frozen.amount() || !fact.currency().equals(frozen.currency())) {
            throw new CommerceException(503, "mock原件与冻结订单金额不符");
        }
    }
    private void insert(PaymentPolicy.Fact fact) {
        jdbc.update("INSERT INTO commerce_mock_transaction(kind,order_sn,request_id,transaction_id,payment_transaction_id,amount_minor,currency,created_ms,applied) VALUES(?,?,?,?,?,?,?,?,0)",
            fact.kind(), fact.orderId(), ascii(fact.requestId()), ascii(fact.transactionId()), ascii(fact.paymentTransactionId()),
            fact.amountMinor(), fact.currency(), fact.createdMs());
    }
    private void markApplied(PaymentPolicy.Fact fact, Budget budget) {
        transaction(budget, () -> {
            jdbc.update("UPDATE commerce_mock_transaction SET applied=1 WHERE kind=? AND order_sn=? AND transaction_id=? AND applied=0",
                fact.kind(), fact.orderId(), ascii(fact.transactionId()));
            var current = policy.required(fact.kind(), fact.orderId());
            if (current.applied() != 1 || !current.transactionId().equals(fact.transactionId())) {
                throw new CommerceException(503, "mock原件投递标记冲突");
            }
            return Boolean.TRUE;
        });
    }
    private <T> T transaction(Budget budget, Supplier<T> action) {
        if (!budget.canStart()) { throw new CommerceException(503, "mock处理预算已用尽，原件保留待恢复"); }
        var tx = new TransactionTemplate(manager);
        tx.setPropagationBehavior(TransactionDefinition.PROPAGATION_REQUIRES_NEW);
        tx.setIsolationLevel(TransactionDefinition.ISOLATION_READ_COMMITTED);
        tx.setTimeout(budget.timeoutSeconds());
        return Objects.requireNonNull(tx.execute(status -> action.get()));
    }
    private final class Budget {
        private final long start = nanoTime.getAsLong();
        private final long duration;
        Budget(int seconds) { duration = TimeUnit.SECONDS.toNanos(seconds); }
        long remaining() { return duration - (nanoTime.getAsLong() - start); }
        boolean canStart() { return remaining() >= MIN_START_NANOS; }
        int timeoutSeconds() { return (int)Math.max(1, Math.min(5, (remaining() + 999_999_999L) / 1_000_000_000L)); }
    }
    private static final class Cursor {
        private String after = "";
        private String upper;
        void reset() { after = ""; upper = null; }
    }
    private static byte[] ascii(String value) { return value.getBytes(StandardCharsets.US_ASCII); }
}
