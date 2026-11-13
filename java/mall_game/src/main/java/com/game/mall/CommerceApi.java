package com.game.mall;

import jakarta.validation.constraints.*;

public final class CommerceApi {
    private CommerceApi() {}
    public record CreateOrder(@NotBlank @Pattern(regexp="[A-Za-z0-9_-]{8,128}") String requestId,
                              @Positive long skuId, @Min(1) @Max(10000) int quantity,
                              @NotNull @Size(min=1,max=191) String buyerAccount, @NotBlank String buyerOwnerRevision) {}
    // 可信trade根据原合同补原修订；不能把过户后当前角色修订冒充原订单身份。
    public record OwnedOrder(String orderId, String buyerAccount, String buyerOwnerRevision) {}
    // 此消息只能来自已核实渠道事实的可信适配器；本地mock还须匹配独立原件，不接受客户端宣称成功。
    // amountMinor / currency 是渠道实收的金额币种，不是订单价：与订单冻结值不符时商城照样记下这笔钱并排进退款(见 PaymentOutcome)。
    public record Payment(@NotBlank @Pattern(regexp="[0-9a-f-]{36}") String orderId,
                          @NotBlank @Pattern(regexp="[a-z][a-z0-9_-]{1,31}") String channel,
                          @NotBlank @Pattern(regexp="[A-Za-z0-9_-]{1,128}") String transactionId,
                          @Positive long amountMinor,
                          @NotBlank @Pattern(regexp="[A-Z][A-Z0-9_]{2,15}") String currency) {}

    /**
     * POST /internal/payments 的应答(2026-11-10 起;之前只回订单视图,适配器看不出这笔钱去了哪)。
     * <p>HTTP 200 表示这笔付款已按 (channel, transactionId) 落库,适配器据此停止重试;同一笔重放也回 200,不重复记账。
     * 409 表示报来的这笔与已记下的冲突(交易号已属别的订单,或改了金额币种),是适配器的错,可以当终局;
     * 503 表示商城存储不变量被破坏,这笔钱没有记下,适配器必须保留它、退避重试并告警。
     * order 是这笔付款所报订单的当前视图。这笔钱的去向只看 refundReason(取这笔付款当前的退款待办):
     * <ul>
     *   <li>null:这笔是订单本身的付款,订单照常推进(order.state 为 DELIVERY_PENDING 等)。</li>
     *   <li>非 null:商城已为这笔钱建好退款待办(/internal/refunds/poll 会列出它),值是退款原因。适配器不把它计为已付、不发货,
     *       等退款完成。重复付款(DUPLICATE_PAYMENT)与金额 / 币种不符(AMOUNT_MISMATCH / CURRENCY_MISMATCH)应答形状相同、
     *       都回 200,只靠这个值区分:前者是玩家对已付订单又付了一次;后者是渠道实收与订单冻结的金额币种对不上
     *       (渠道侧改价,或适配器取错了金额、币种),应单独告警排查。三者都不绑定订单,order.state 不因它们改变。</li>
     * </ul>
     * 取消后迟到的那笔给 CANCELLED_LATE(仍绑定订单,order.state 为 REFUND_PENDING)。重放的结论与第一次相同,只有一个例外:
     * 订单本身那笔在交付被拒之后重放,refundReason 由 null 变为 DELIVERY_REJECTED(这笔已转退款)。
     */
    public record PaymentOutcome(Order order, RefundReason refundReason) {}

    /**
     * 退款原因:commerce_refund_intent.source 与 commerce_abnormal_payment.reason 的取值,也出现在付款应答和退款待办里。
     * 前两种退的是订单绑定的那笔付款;后三种是异常付款,这笔钱没有绑定订单。
     */
    public enum RefundReason {
        /** 订单取消后才到账:这笔绑定原单,订单转 REFUND_PENDING。 */
        CANCELLED_LATE(true),
        /** trade 拒收交付:退订单绑定的那笔。只由交付回执产生,付款当时不会给出。 */
        DELIVERY_REJECTED(true),
        /** 订单已由另一笔付清,这是多付的一笔。 */
        DUPLICATE_PAYMENT(false),
        /** 币种相符、金额与订单冻结的应付金额不同(多付少付都算)。 */
        AMOUNT_MISMATCH(false),
        /** 币种与订单冻结的币种不同;不同币种的金额不可比,不再比金额。 */
        CURRENCY_MISMATCH(false);

        private final boolean ordersOwnPayment;
        RefundReason(boolean ordersOwnPayment) { this.ordersOwnPayment = ordersOwnPayment; }

        /**
         * true:退的是订单绑定的那笔付款,退款回执把订单 REFUND_PENDING 推进到 REFUNDED。
         * false:异常付款(付款回执 order_sn 为 NULL,另有 commerce_abnormal_payment 一行),订单、库存、交付都不因它变化,
         * 退款回执只终结它自己的待办。
         */
        public boolean refundsOrdersOwnPayment() { return ordersOwnPayment; }

        /** 读库里存的取值;认不出说明存储已损坏,拒绝而不是猜。 */
        static RefundReason stored(String value) {
            for (RefundReason reason : values()) {
                if (reason.name().equals(value)) { return reason; }
            }
            throw new CommerceException(503, "退款原因无法识别");
        }
    }

    /**
     * 待退款的一笔付款(/internal/refunds/poll 的一项)。金额币种取这笔付款回执记下的实收值:异常付款退的是玩家实际付的钱,不是订单价。
     * <p>适配器契约:向渠道发起退款时,用被退那笔付款的交易号 transactionId 作渠道退款的幂等键(渠道限制长度或字符集时,
     * 由它确定性派生,例如取摘要);发起前先按这个键查渠道退款状态,已退款或退款中就沿用那条结果,不再发起。
     * 不能拿 orderId 当键:一张订单可能同时有本单那笔和几笔异常付款要退,按订单去重会让后几笔永远退不出去。
     * 退完按 Refund 上报,paymentTransactionId 填这里的 transactionId。
     */
    public record PendingRefund(String orderId, String channel, String transactionId, long amountMinor, String currency,
                                RefundReason reason) {}

    // 渠道退款回执,同样只来自可信适配器。一张订单可能有多笔要退(订单绑定的那笔与异常付款),所以必须指明退的是哪一笔:
    // paymentTransactionId 是退款待办列出的那笔付款的交易号,transactionId 是渠道给这次退款的流水号;渠道两者共用。
    // 金额币种必须等于那笔付款回执记的实收值。一笔付款只认一条退款流水,一条退款流水也只能了结一笔付款。
    public record Refund(String orderId, String channel, String paymentTransactionId, String transactionId,
                         long amountMinor, String currency) {}
    public record Order(String orderId, String buyerId, long skuId, int quantity, long amountMinor,
                        String currency, String state, long paymentDeadlineMs) {}
    // 仅供已认证的 payment 角色查原订单；金额与归属均由服务器原记录返回。
    public record OrderQuery(@NotBlank @Size(max=36) String orderId) {}

    /**
     * POST /internal/payments/revision 的请求(payment 角色,payment-recharge.md §22.6):渠道翻转了一笔已记下的付款。
     * revision 是这笔钱的修订号,奇数 = 已被渠道撤销(fact=REVOKED),大于 0 的偶数 = 撤销又被撤回(fact=PAID)。
     * orderId 是这笔付款报给商城时所报的订单(绑定的那张,或异常付款冲的那张),商城按它先锁订单行,与付款、退款同一锁序。
     */
    public record Revision(String orderId, String channel, String transactionId, int revision, String fact) {}

    /** 修订的应答:商城当前记下的修订号与事实(请求的修订号不大于已记的也回 200,原样回当前值),以及订单当前状态。 */
    public record RevisionOutcome(int revision, String fact, String orderState) {}

    /**
     * POST /internal/refunds/close 的请求(payment 角色):这笔付款的渠道退款做不了(渠道没有开发者退款接口,或渠道明确失败),
     * 退款待办转人工,不再列在待退款里。订单本身那笔(取消后迟到、交付被拒)转人工时订单一并转 MANUAL。
     */
    public record RefundClosure(String orderId, String channel, String paymentTransactionId, String reason) {}
}
