package com.game.mall;

import java.util.List;

/** 服务间寄售契约；账号和托管引用由认证trade提供，普通客户端无此写权限。 */
public final class RmbConsignmentApi {
    private RmbConsignmentApi() {}
    public record Escrow(String listingId, String sellerPlayerId, String sellerAccount, long marketZone,
                         String assetKind, Integer assetCurrencyType, String escrowReference, String escrowProof,
                         long priceFen, String title) {}
    public record Page(String afterListingId, int limit) {}
    public record Get(String listingId) {}
    public record Listing(String listingId, String sellerPlayerId, long marketZone, String assetKind, String title,
                          long priceFen, int feeBps, long coolingMs, long noticeEndMs, long saleEndMs,
                          String state, String phase, String orderId) {}
    public record Listings(List<Listing> listings) {}
    public record Buy(String listingId, String buyerPlayerId, String buyerAccount, String requestId,
                      String buyerOwnerRevision) {}
    public record Cancel(String listingId, String sellerAccount) {}
    public record OrderQuery(String account, String orderId) {}
    public record OrderPage(String account, String afterOrderId, int limit) {}
    public record IncomePage(String account, String afterOrderId, int limit) {}
    public record Income(String orderId, long grossFen, long feeFen, long netFen, long reservedFen,
                         long paidFen, long availableMs, long forfeitedFen) {
        public Income(String orderId, long grossFen, long feeFen, long netFen, long reservedFen,
                      long paidFen, long availableMs) {
            this(orderId, grossFen, feeFen, netFen, reservedFen, paidFen, availableMs, 0L);
        }
    }
    public record Incomes(List<Income> incomes) {}
    public record Withdraw(String account, String requestId, long amountFen) {}
    public record Withdrawal(String withdrawalId, long amountFen, String state, String channel, String transactionId) {}
    public record WithdrawalQuery(String account, String withdrawalId) {}
    public record WithdrawalReceipt(String withdrawalId, String channel, String transactionId,
                                    long amountFen, String currency, boolean successful) {}
    public record Delivery(String orderId, String buyerPlayerId, String buyerAccount, String buyerOwnerRevision, String listingId,
                           String escrowReference, String escrowProof, long amountFen) {}
    public record DeliveryEntry(Delivery intent, String cursor) {}
    public record Deliveries(List<DeliveryEntry> entries) {}
    public record Return(String listingId, String sellerPlayerId, String sellerAccount, String escrowReference,
                         String escrowProof) {}
    public record ReturnEntry(Return intent, String cursor) {}
    public record Returns(List<ReturnEntry> entries) {}
    public record ReturnReceipt(String listingId, String escrowReference, String escrowProof, String returnProof) {}
}
