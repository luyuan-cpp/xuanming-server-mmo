package com.game.mall;

import java.nio.charset.StandardCharsets;
import java.time.Clock;
import java.util.Map;
import java.util.Objects;
import java.util.Set;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.context.annotation.DependsOn;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Repository;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.TransactionDefinition;
import org.springframework.transaction.support.TransactionSynchronization;
import org.springframework.transaction.support.TransactionSynchronizationManager;
import org.springframework.transaction.support.TransactionTemplate;

/**
 * 付款修订(payment 角色,POST /internal/payments/revision;docs/design/payment-recharge.md §22.6)。
 * <p>渠道把一笔已记下的付款撤销了(用户在 App Store / Google Play 退款、渠道作废、商户后台手工退款),或撤销又被撤回
 * (App Store 的 REFUND_REVERSED)。支付服务按交易给每次翻转一个递增的修订号:0 是原始付款(/internal/payments 记下的),
 * 奇数 = 已撤销(fact=REVOKED),大于 0 的偶数 = 撤销又被撤回(fact=PAID)。商城只接受更大的修订号,过时的盖不住新的,
 * 重放与乱序都回当前值(200)。
 * <p>修订对订单的影响只看这笔付款现在的处境,按下面的顺序判一次:
 * <ol>
 *   <li>这笔付款有退款待办(订单取消后迟到、交付被拒、重复付款、金额币种不符):撤销 = 渠道已把钱退给用户,待办
 *       PENDING / MANUAL → REVOKED,不用再退;订单本身那笔的订单 5 / 7 → 6。撤回撤销 = 钱又回到商户,已 REVOKED 的待办
 *       转 MANUAL(原因 REVOCATION_REVERSED),由人决定再退一次;订单本身那笔的订单 6 → 7。已退成(REFUNDED)的只留档。</li>
 *   <li>寄售订单(sku 0)绑定的付款:卖家的收入已入账、物品已转给买家,扣不回来;只留档并记错误日志,由人工处理。</li>
 *   <li>官方订单绑定的付款(已建交付待办):写扣回待办(一张订单一行,最新修订号,PENDING),订单转 8 扣回中;
 *       trade 把发放收敛到最新修订号之后回执(DeliveryStore.reversalReceipt)。没有扣回待办且新事实仍是付款(中间的
 *       撤销与撤回商城没看见)时发放不用变,只记修订号。</li>
 * </ol>
 * 锁序:订单行 → 付款回执 → 退款待办 → 扣回待办,与付款、退款回执、交付回执、扣回回执相同(都先锁订单行)。
 */
@Repository
@DependsOn("commerceSchemaReady")
public class PaymentRevisionStore {
    private static final Logger LOG = LoggerFactory.getLogger(PaymentRevisionStore.class);
    static final String PAID = "PAID";
    static final String REVOKED = "REVOKED";
    /** 官方订单绑定的付款可能所在的订单状态:待交付、已交付、人工、扣回中、已扣回。 */
    private static final Set<Integer> BOUND_OFFICIAL_STATES = Set.of(1, 2, 7, 8, 9);
    private final JdbcTemplate jdbc;
    private final TransactionTemplate tx;
    private final Clock clock;
    private final CommerceProperties props;

    public PaymentRevisionStore(JdbcTemplate jdbc, PlatformTransactionManager manager, Clock clock, CommerceProperties props) {
        this.jdbc = jdbc;
        this.clock = clock;
        this.props = props;
        tx = new TransactionTemplate(manager);
        tx.setIsolationLevel(TransactionDefinition.ISOLATION_READ_COMMITTED);
        tx.setTimeout(5);
    }

    public CommerceApi.RevisionOutcome record(CommerceApi.Revision in) {
        if (in == null || in.orderId() == null || !in.orderId().matches(PaymentPolicy.ORDER)
            || in.channel() == null || !in.channel().matches("[a-z][a-z0-9_-]{1,31}")
            || in.transactionId() == null || !in.transactionId().matches("[A-Za-z0-9_-]{1,128}")
            || in.revision() < 1 || !(PAID.equals(in.fact()) || REVOKED.equals(in.fact()))
            || (in.revision() % 2 == 1) != REVOKED.equals(in.fact()))
            throw new CommerceException(400, "付款修订参数非法");
        props.requirePaymentChannel(in.channel());
        return Objects.requireNonNull(tx.execute(status -> {
            var orders = jdbc.queryForList("SELECT status,sku_id FROM oms_order WHERE order_sn=? FOR UPDATE", in.orderId());
            if (orders.size() != 1)
                throw new CommerceException(404, "订单不存在");
            int orderStatus = ((Number) orders.getFirst().get("status")).intValue();
            long skuId = ((Number) orders.getFirst().get("sku_id")).longValue();
            var receipts = jdbc.queryForList("SELECT order_sn,revision,fact FROM commerce_payment_receipt "
                + "WHERE channel=? AND transaction_id=? FOR UPDATE", ascii(in.channel()), ascii(in.transactionId()));
            if (receipts.size() != 1)
                throw new CommerceException(404, "付款回执不存在");
            var receipt = receipts.getFirst();
            String boundOrder = (String) receipt.get("order_sn");
            String owner = boundOrder;
            if (owner == null) {
                var abnormal = jdbc.queryForList("SELECT order_sn FROM commerce_abnormal_payment WHERE channel=? AND transaction_id=?",
                    String.class, ascii(in.channel()), ascii(in.transactionId()));
                if (abnormal.size() != 1)
                    throw new CommerceException(503, "不绑定订单的付款缺异常付款记录");
                owner = abnormal.getFirst();
            }
            if (!in.orderId().equals(owner))
                throw new CommerceException(409, "这笔付款不属于这张订单");
            int stored = ((Number) receipt.get("revision")).intValue();
            String storedFact = (String) receipt.get("fact");
            if (in.revision() <= stored)
                return new CommerceApi.RevisionOutcome(stored, storedFact, CommerceStore.stateName(orderStatus));
            if (jdbc.update("UPDATE commerce_payment_receipt SET revision=?,fact=? WHERE channel=? AND transaction_id=? AND revision=?",
                    in.revision(), in.fact(), ascii(in.channel()), ascii(in.transactionId()), stored) != 1)
                throw new CommerceException(503, "付款回执修订冲突");
            boolean revoked = REVOKED.equals(in.fact());
            var refunds = jdbc.queryForList("SELECT source,status FROM commerce_refund_intent WHERE channel=? AND transaction_id=? FOR UPDATE",
                ascii(in.channel()), ascii(in.transactionId()));
            int next;
            if (!refunds.isEmpty()) {
                next = refundEffect(in, refunds.getFirst(), revoked, orderStatus);
            } else if (boundOrder == null) {
                throw new CommerceException(503, "异常付款缺退款待办");
            } else if (skuId == 0) {
                next = orderStatus;
                afterCommit(() -> LOG.error("consignment payment changed by the channel, manual handling needed: order={} channel={} revision={} fact={}",
                    in.orderId(), in.channel(), in.revision(), in.fact()));
            } else {
                next = reversalEffect(in, revoked, orderStatus);
            }
            return new CommerceApi.RevisionOutcome(in.revision(), in.fact(), CommerceStore.stateName(next));
        }));
    }

    /** 这笔付款有退款待办时的影响,返回订单的新状态(不动订单时原样返回)。 */
    private int refundEffect(CommerceApi.Revision in, Map<String, Object> refund, boolean revoked, int orderStatus) {
        boolean ordersOwnPayment = CommerceApi.RefundReason.stored((String) refund.get("source")).refundsOrdersOwnPayment();
        String current = (String) refund.get("status");
        String nextStatus;
        String reason;
        int expectedOrder;
        int nextOrder;
        if (revoked && ("PENDING".equals(current) || "MANUAL".equals(current))) {
            nextStatus = "REVOKED";
            reason = "CHANNEL_REVOKED";
            expectedOrder = "PENDING".equals(current) ? 5 : 7;
            nextOrder = 6;
        } else if (!revoked && "REVOKED".equals(current)) {
            nextStatus = "MANUAL";
            reason = "REVOCATION_REVERSED";
            expectedOrder = 6;
            nextOrder = 7;
        } else {
            // 修订没有改变要不要退:已退成的付款又被渠道翻转(多半是我们自己那笔退款的回声),或撤回落在仍待退的待办上。只留档。
            if ("REFUNDED".equals(current)) {
                afterCommit(() -> LOG.warn("payment changed by the channel after the mall refunded it: order={} channel={} revision={} fact={}",
                    in.orderId(), in.channel(), in.revision(), in.fact()));
            }
            return orderStatus;
        }
        if (jdbc.update("UPDATE commerce_refund_intent SET status=?,close_reason=? WHERE channel=? AND transaction_id=? AND status=?",
                nextStatus, reason, ascii(in.channel()), ascii(in.transactionId()), current) != 1)
            throw new CommerceException(503, "退款待办状态冲突");
        if (!ordersOwnPayment)
            return orderStatus;
        if (jdbc.update("UPDATE oms_order SET status=? WHERE order_sn=? AND status=?", nextOrder, in.orderId(), expectedOrder) != 1)
            throw new CommerceException(503, "订单本身的退款待办与订单状态不一致");
        return nextOrder;
    }

    /** 官方订单绑定的付款:写扣回待办,订单转 8 扣回中,返回订单的新状态。 */
    private int reversalEffect(CommerceApi.Revision in, boolean revoked, int orderStatus) {
        if (!BOUND_OFFICIAL_STATES.contains(orderStatus))
            throw new CommerceException(503, "绑定付款的官方订单状态无法处理");
        // 交付待办与绑定付款同一事务写下(CommerceStore.firstPayment),之后只改状态不删行;这里只核对存在,不加锁。
        if (jdbc.queryForList("SELECT status FROM commerce_delivery_intent WHERE order_sn=?", in.orderId()).size() != 1)
            throw new CommerceException(503, "官方订单的绑定付款缺交付待办");
        var existing = jdbc.queryForList("SELECT revision FROM commerce_reversal_intent WHERE order_sn=? FOR UPDATE", in.orderId());
        if (existing.isEmpty() && !revoked)
            return orderStatus; // 付款 → 付款:中间的撤销与撤回商城没看见,发放不用变
        long now = clock.millis();
        if (existing.isEmpty()) {
            jdbc.update("INSERT INTO commerce_reversal_intent(order_sn,revision,status,updated_ms) VALUES(?,?,'PENDING',?)",
                in.orderId(), in.revision(), now);
        } else if (jdbc.update("UPDATE commerce_reversal_intent SET revision=?,status='PENDING',updated_ms=? WHERE order_sn=? AND revision<?",
                in.revision(), now, in.orderId(), in.revision()) != 1) {
            throw new CommerceException(503, "扣回待办的修订号超过付款回执");
        }
        if (orderStatus != 8 && jdbc.update("UPDATE oms_order SET status=8 WHERE order_sn=? AND status=?", in.orderId(), orderStatus) != 1)
            throw new CommerceException(503, "订单状态冲突");
        afterCommit(() -> LOG.warn("payment changed by the channel, game grant reconciliation queued: order={} channel={} revision={} fact={}",
            in.orderId(), in.channel(), in.revision(), in.fact()));
        return 8;
    }

    /** 提交成功后才记日志:事务回滚时不能留下「已处理」的假象。 */
    private static void afterCommit(Runnable log) {
        TransactionSynchronizationManager.registerSynchronization(new TransactionSynchronization() {
            @Override public void afterCommit() { log.run(); }
        });
    }

    private static byte[] ascii(String value) { return value.getBytes(StandardCharsets.US_ASCII); }
}
