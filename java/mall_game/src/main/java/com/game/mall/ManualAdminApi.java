package com.game.mall;

import java.util.List;

/** 官方商城与人民币寄售的人工单(MANUAL)查询与受控处置契约。 */
public final class ManualAdminApi {
    private ManualAdminApi() {}

    /** 查询人工待办分页参数；各游标均可省略或为空串，limit 缺省 50、上限 100。 */
    public record ListQuery(String afterOrderId, String afterListingId, String afterRefundCursor, int limit) {}

    /** 状态为 MANUAL(7) 的订单诊断视图(含官方交付/扣回与寄售/冻结收入上下文)。 */
    public record ManualOrder(
        String orderId,
        String buyerId,
        long skuId,
        int quantity,
        long amountMinor,
        String currency,
        int paymentRevision,
        String deliveryStatus,
        Integer reversalRevision,
        String reversalStatus,
        String consignmentListingId,
        String consignmentState,
        Long incomeNetFen,
        Long incomeReservedFen,
        Long incomePaidFen,
        Long incomeForfeitedFen,
        Long incomeAvailableMs) {}

    /** 状态为 MANUAL 的退款待办诊断视图。 */
    public record ManualRefund(
        String orderId,
        String channel,
        String transactionId,
        long amountMinor,
        String currency,
        CommerceApi.RefundReason reason,
        String closeReason,
        int paymentRevision,
        String paymentFact) {}

    /** 状态为 MANUAL 的寄售单与对应卖家收入视图。 */
    public record ManualConsignment(
        String listingId,
        String sellerPlayerId,
        long marketZone,
        String assetKind,
        String title,
        long priceFen,
        String currentOrderId,
        String orderState,
        Long incomeNetFen,
        Long incomeReservedFen,
        Long incomePaidFen,
        Long incomeForfeitedFen,
        Long incomeAvailableMs) {}

    /** 三类人工待办的分页汇总。 */
    public record Summary(
        List<ManualOrder> orders,
        List<ManualRefund> refunds,
        String nextRefundCursor,
        List<ManualConsignment> consignments) {}

    /**
     * 处置官方订单(skuId > 0, status = 7 MANUAL)。
     * action 取值:
     * - RETRY_REVERSAL: 扣回待办 MANUAL -> PENDING、清除非终局 MANUAL 回执并置订单 REVERSING(8)，由 trade 重新拉取收敛；
     * - MARK_REVERSED: 仅限奇数修订号扣回 MANUAL，收敛为 REVERSED(9)；
     * - MARK_DELIVERED: 偶数修订号扣回 MANUAL 或无扣回的交付 MANUAL，收敛为 DELIVERED(2)；
     * - CREATE_REFUND: 无扣回且绑定付款仍为 PAID 的交付 MANUAL，建 DELIVERY_REJECTED 退款待办并置订单 REFUND_PENDING(5)。
     */
    public record ResolveOrder(String requestId, String orderId, String action, String reason) {}

    /**
     * 处置人工退款待办(commerce_refund_intent.status = MANUAL)。
     * action 取值:
     * - RETRY_REFUND: 仅限原付款仍为 PAID，待办 MANUAL -> PENDING(清空 close_reason)，本单退款订单 7 -> 5；
     * - MARK_REFUNDED: 凭渠道线下退款流水 externalRefundId 记入 commerce_refund_receipt 并置 REFUNDED，本单退款订单 7 -> 6；
     * - MARK_REVOKED: 确认渠道侧已退还/撤销，置 REVOKED(不写退款回执)，本单退款订单 7 -> 6。
     */
    public record ResolveRefund(
        String requestId,
        String orderId,
        String channel,
        String paymentTransactionId,
        String action,
        String externalRefundId,
        String reason) {}

    /**
     * 处置人工寄售单(commerce_consignment.state = MANUAL)及冻结卖家收入。
     * action 取值:
     * - RETURN_TO_SELLER: 仅限尚未生成卖家收入(未交付买家)，作废未完成交付待办，寄售 MANUAL -> RETURNING(由 trade 退还托管资产)，关联订单 7 -> 9；
     * - UNFREEZE_INCOME: 仅限已生成卖家收入且处于冻结(available_ms = Long.MAX_VALUE)且仍有未结余款，解冻为当前时刻可提现，寄售 MANUAL -> SOLD，关联订单 7 -> 2；
     * - FORFEIT_INCOME: 仅限已生成卖家收入且无在途提现预留(reserved_fen = 0)，将剩余未付净额计入 forfeited_fen 并永久冻结，寄售 MANUAL -> FORFEITED，关联订单 7 -> 9。
     */
    public record ResolveConsignment(String requestId, String listingId, String action, String reason) {}

    /**
     * 逐单争议冻结 / 释放卖家收益(与 RmbConsignmentStore.withdraw 共用账号锁 commerce_seller_guard)。
     * action 取值:
     * - FREEZE_DISPUTE: 按 amountFen 冻结尚未预留/未出款/未没收的可用净额并登记 caseId，阻止该部分提现；
     * - RELEASE_DISPUTE: 释放已冻结的 amountFen（全部释放后清空 dispute_case_id），恢复按原冷静期可提现。
     */
    public record ResolveIncomeDispute(
        String requestId,
        String orderId,
        String action,
        long amountFen,
        String caseId,
        String reason) {}

    /** 人工单处置结果，按 (actor, requestId) 幂等持久化在 commerce_admin_command。 */
    public record Resolution(
        String targetType,
        String targetId,
        String action,
        String resultingState,
        String reason,
        long resolvedAtMs) {}
}
