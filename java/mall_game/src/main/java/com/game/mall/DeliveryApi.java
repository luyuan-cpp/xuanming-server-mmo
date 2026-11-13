package com.game.mall;
import java.util.List;
/** 内部游戏交付契约；付款事实仍只来自渠道认证面，资产证明仅由trade提交。 */
public final class DeliveryApi {
    private DeliveryApi() {}
    public record Poll(int limit, String cursor) {
        public Poll(int limit) { this(limit, ""); }
    }
    // 扫描位置只放包装里；Intent 是已有订单的幂等摘要输入，不能因翻页增加字段。
    // paymentRevision 同理放包装里:订单绑定付款的最新修订号(有扣回待办时,见 ReversalEntry),没有为 0;奇数 = 付款已被撤销,
    // trade 不再发放,只记作废标记并回执 VOIDED。
    public record PollEntry(Intent intent, String cursor, int paymentRevision) {
        public PollEntry(Intent intent, String cursor) { this(intent, cursor, 0); }
    }
    public record PollPage(List<PollEntry> entries) {}
    // 发放物二选一:物品(itemConfigId × itemCount)或货币(currencyType × currencyAmount,已乘过购买数量);另一种全为 0。
    // 货币种类 0 是银两,有没有货币发放看 currencyAmount 是否大于 0。
    public record Intent(String orderId, String buyerId, String buyerAccount, String buyerOwnerRevision,
        long skuId, int quantity, int itemConfigId, int itemCount,
        String title, long amountMinor, String currency, int currencyType, long currencyAmount) {}
    public record OrderPage(String afterOrderId, int limit, String buyerAccount, String buyerOwnerRevision) {}
    public record SkuPage(long afterSkuId, int limit) {}
    public record Receipt(String orderId, String tradeOrderId, String state, String proof) {}
    public record Sku(long skuId, String title, long priceMinor, String currency, long available) {}
    public record Skus(List<Sku> skus) {}

    /**
     * 扣回待办(payment-recharge.md §22.6):渠道撤销或撤回一笔已绑定订单的付款后,商城按新修订号写一条。
     * intent 与交付待办同一份订单条款(trade 据此找到或建立同一张合同,摘要一致);entitled 为真表示这个修订号下买家该持有这份发放。
     */
    public record ReversalEntry(Intent intent, int revision, boolean entitled, String cursor) {}
    public record ReversalPage(List<ReversalEntry> entries) {}
    /** 扣回回执:trade 把这张订单的发放收敛到 entitled 之后回报。state 取 REVERSED(已不持有)、DELIVERED(持有)、MANUAL(扣不回,人工)。 */
    public record ReversalReceipt(String orderId, int revision, String tradeOrderId, String state, String proof) {}
}
