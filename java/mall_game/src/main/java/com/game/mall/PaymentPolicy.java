package com.game.mall;

import java.nio.charset.StandardCharsets;
import java.util.List;
import org.springframework.jdbc.core.JdbcTemplate;

/** 原回执提交点的渠道与mock事实守卫；允许集合本身不能授权伪造mock回执。 */
final class PaymentPolicy {
    static final String ORDER = "[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}";
    private final JdbcTemplate jdbc;
    private final CommerceProperties props;
    PaymentPolicy(JdbcTemplate jdbc, CommerceProperties props) { this.jdbc = jdbc; this.props = props; }

    record Fact(String kind, String orderId, String requestId, String transactionId, String paymentTransactionId,
        long amountMinor, String currency, long createdMs, int applied) {
        CommerceApi.Payment receipt() { return new CommerceApi.Payment(orderId, "mock", transactionId, amountMinor, currency); }
        /** REFUND 原件对应的退款回执:被退的那笔是 paymentTransactionId,退款流水是 transactionId。 */
        CommerceApi.Refund refundReceipt() {
            return new CommerceApi.Refund(orderId, "mock", paymentTransactionId, transactionId, amountMinor, currency);
        }
    }

    Fact find(String kind, String orderId, boolean lock) {
        var rows = jdbc.query("SELECT kind,order_sn,request_id,transaction_id,payment_transaction_id,amount_minor,currency,created_ms,applied "
                + "FROM commerce_mock_transaction WHERE kind=? AND order_sn=?" + (lock ? " FOR UPDATE" : ""),
            (row, index) -> new Fact(row.getString(1), row.getString(2), row.getString(3), row.getString(4),
                row.getString(5), row.getLong(6), row.getString(7), row.getLong(8), row.getInt(9)), kind, orderId);
        if (rows.isEmpty()) { return null; }
        if (rows.size() != 1) { throw new CommerceException(503, "mock交易原件不唯一"); }
        var fact = rows.getFirst();
        String prefix = "PAYMENT".equals(kind) ? "mock-pay-" : "mock-refund-";
        if (!List.of("PAYMENT", "REFUND").contains(kind) || !kind.equals(fact.kind())
            || fact.orderId() == null || !fact.orderId().matches(ORDER) || !orderId.equals(fact.orderId())
            || fact.requestId() == null || !fact.requestId().matches("[A-Za-z0-9_-]{8,128}")
            || fact.transactionId() == null || !fact.transactionId().matches(prefix + ORDER)
            || fact.paymentTransactionId() == null || !fact.paymentTransactionId().matches("mock-pay-" + ORDER)
            || ("PAYMENT".equals(kind) && !fact.transactionId().equals(fact.paymentTransactionId()))
            || fact.amountMinor() < 1 || fact.amountMinor() > CommerceStore.MAX_AMOUNT_MINOR
            || fact.currency() == null || !fact.currency().matches("[A-Z][A-Z0-9_]{2,15}")
            || fact.createdMs() < 0 || fact.applied() < 0 || fact.applied() > 1) {
            throw new CommerceException(503, "mock交易原件损坏");
        }
        return fact;
    }

    Fact required(String kind, String orderId) {
        var fact = find(kind, orderId, false);
        if (fact == null) { throw new CommerceException(503, "缺少本地mock交易原件"); }
        return fact;
    }

    Fact ownedPayment(String orderId) {
        var payment = required("PAYMENT", orderId);
        var original = jdbc.query("SELECT channel,transaction_id,amount_minor,currency FROM commerce_payment_receipt WHERE order_sn=?",
            (row, index) -> new CommerceApi.Payment(orderId, row.getString(1), row.getString(2), row.getLong(3), row.getString(4)), orderId);
        if (original.size() != 1 || !payment.receipt().equals(original.getFirst())) {
            throw new CommerceException(503, "原付款回执与mock事实不符");
        }
        return payment;
    }

    void payment(CommerceApi.Payment input) {
        props.requirePaymentChannel(input.channel());
        if ("mock".equals(input.channel()) && !required("PAYMENT", input.orderId()).receipt().equals(input)) {
            throw new CommerceException(409, "mock付款回执与原交易不符");
        }
    }

    void refund(CommerceApi.Refund input) {
        props.requirePaymentChannel(input.channel());
        // 被退的那笔按交易键找;退款渠道是键的一部分,拿别的渠道报这笔付款的退款在这里就找不到原付款。
        var original = jdbc.query("SELECT amount_minor,currency FROM commerce_payment_receipt WHERE channel=? AND transaction_id=?",
            (row, index) -> new CommerceApi.Payment(input.orderId(), input.channel(), input.paymentTransactionId(), row.getLong(1), row.getString(2)),
            ascii(input.channel()), ascii(input.paymentTransactionId()));
        if (original.size() != 1) {
            throw new CommerceException(409, "退款渠道与原付款不符");
        }
        if ("mock".equals(input.channel())) {
            // mock 只能付一笔(原件按订单唯一),所以被退的必须正是订单的 mock 付款原件。
            var payment = required("PAYMENT", input.orderId());
            var refund = required("REFUND", input.orderId());
            if (!payment.receipt().equals(original.getFirst()) || !refund.refundReceipt().equals(input)
                || !refund.paymentTransactionId().equals(payment.transactionId())
                || refund.amountMinor() != payment.amountMinor() || !refund.currency().equals(payment.currency())) {
                throw new CommerceException(409, "mock退款与原付款事实不符");
            }
        }
    }

    private static byte[] ascii(String value) { return value.getBytes(StandardCharsets.US_ASCII); }
}
