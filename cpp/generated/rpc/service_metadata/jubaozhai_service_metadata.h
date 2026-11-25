#pragma once
#include <cstdint>

#include "proto/trade/jubaozhai.pb.h"

constexpr uint32_t ClientPlayerJubaozhaiBrowseListingsMessageId = 196;
constexpr uint32_t ClientPlayerJubaozhaiBrowseListingsIndex = 0;
#define ClientPlayerJubaozhaiBrowseListingsMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(0)

constexpr uint32_t ClientPlayerJubaozhaiGetListingDetailMessageId = 197;
constexpr uint32_t ClientPlayerJubaozhaiGetListingDetailIndex = 1;
#define ClientPlayerJubaozhaiGetListingDetailMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(1)

constexpr uint32_t ClientPlayerJubaozhaiSetFavoriteMessageId = 198;
constexpr uint32_t ClientPlayerJubaozhaiSetFavoriteIndex = 2;
#define ClientPlayerJubaozhaiSetFavoriteMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(2)

constexpr uint32_t ClientPlayerJubaozhaiGetMyShelfMessageId = 200;
constexpr uint32_t ClientPlayerJubaozhaiGetMyShelfIndex = 3;
#define ClientPlayerJubaozhaiGetMyShelfMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(3)

constexpr uint32_t ClientPlayerJubaozhaiListOfficialSkusMessageId = 277;
constexpr uint32_t ClientPlayerJubaozhaiListOfficialSkusIndex = 4;
#define ClientPlayerJubaozhaiListOfficialSkusMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(4)

constexpr uint32_t ClientPlayerJubaozhaiCreateOfficialOrderMessageId = 295;
constexpr uint32_t ClientPlayerJubaozhaiCreateOfficialOrderIndex = 5;
#define ClientPlayerJubaozhaiCreateOfficialOrderMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(5)

constexpr uint32_t ClientPlayerJubaozhaiGetCommerceOrderMessageId = 279;
constexpr uint32_t ClientPlayerJubaozhaiGetCommerceOrderIndex = 6;
#define ClientPlayerJubaozhaiGetCommerceOrderMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(6)

constexpr uint32_t ClientPlayerJubaozhaiCancelCommerceOrderMessageId = 300;
constexpr uint32_t ClientPlayerJubaozhaiCancelCommerceOrderIndex = 7;
#define ClientPlayerJubaozhaiCancelCommerceOrderMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(7)

constexpr uint32_t ClientPlayerJubaozhaiListCommerceOrdersMessageId = 287;
constexpr uint32_t ClientPlayerJubaozhaiListCommerceOrdersIndex = 8;
#define ClientPlayerJubaozhaiListCommerceOrdersMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(8)

constexpr uint32_t ClientPlayerJubaozhaiCreatePlayerTradeMessageId = 269;
constexpr uint32_t ClientPlayerJubaozhaiCreatePlayerTradeIndex = 9;
#define ClientPlayerJubaozhaiCreatePlayerTradeMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(9)

constexpr uint32_t ClientPlayerJubaozhaiConfirmPlayerTradeMessageId = 291;
constexpr uint32_t ClientPlayerJubaozhaiConfirmPlayerTradeIndex = 10;
#define ClientPlayerJubaozhaiConfirmPlayerTradeMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(10)

constexpr uint32_t ClientPlayerJubaozhaiCreateConsignmentMessageId = 281;
constexpr uint32_t ClientPlayerJubaozhaiCreateConsignmentIndex = 11;
#define ClientPlayerJubaozhaiCreateConsignmentMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(11)

constexpr uint32_t ClientPlayerJubaozhaiBuyConsignmentMessageId = 293;
constexpr uint32_t ClientPlayerJubaozhaiBuyConsignmentIndex = 12;
#define ClientPlayerJubaozhaiBuyConsignmentMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(12)

constexpr uint32_t ClientPlayerJubaozhaiCancelConsignmentMessageId = 298;
constexpr uint32_t ClientPlayerJubaozhaiCancelConsignmentIndex = 13;
#define ClientPlayerJubaozhaiCancelConsignmentMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(13)

constexpr uint32_t ClientPlayerJubaozhaiPlaceMarketOrderMessageId = 282;
constexpr uint32_t ClientPlayerJubaozhaiPlaceMarketOrderIndex = 14;
#define ClientPlayerJubaozhaiPlaceMarketOrderMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(14)

constexpr uint32_t ClientPlayerJubaozhaiCancelMarketOrderMessageId = 276;
constexpr uint32_t ClientPlayerJubaozhaiCancelMarketOrderIndex = 15;
#define ClientPlayerJubaozhaiCancelMarketOrderMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(15)

constexpr uint32_t ClientPlayerJubaozhaiListMarketOrdersMessageId = 266;
constexpr uint32_t ClientPlayerJubaozhaiListMarketOrdersIndex = 16;
#define ClientPlayerJubaozhaiListMarketOrdersMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(16)

constexpr uint32_t ClientPlayerJubaozhaiGetMarketDepthMessageId = 284;
constexpr uint32_t ClientPlayerJubaozhaiGetMarketDepthIndex = 17;
#define ClientPlayerJubaozhaiGetMarketDepthMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(17)

constexpr uint32_t ClientPlayerJubaozhaiCreateAuctionLotMessageId = 271;
constexpr uint32_t ClientPlayerJubaozhaiCreateAuctionLotIndex = 18;
#define ClientPlayerJubaozhaiCreateAuctionLotMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(18)

constexpr uint32_t ClientPlayerJubaozhaiPlaceAuctionBidMessageId = 288;
constexpr uint32_t ClientPlayerJubaozhaiPlaceAuctionBidIndex = 19;
#define ClientPlayerJubaozhaiPlaceAuctionBidMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(19)

constexpr uint32_t ClientPlayerJubaozhaiGetAuctionLotMessageId = 299;
constexpr uint32_t ClientPlayerJubaozhaiGetAuctionLotIndex = 20;
#define ClientPlayerJubaozhaiGetAuctionLotMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(20)

constexpr uint32_t ClientPlayerJubaozhaiCancelAuctionLotMessageId = 297;
constexpr uint32_t ClientPlayerJubaozhaiCancelAuctionLotIndex = 21;
#define ClientPlayerJubaozhaiCancelAuctionLotMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(21)

constexpr uint32_t ClientPlayerJubaozhaiBrowseCommerceOffersMessageId = 304;
constexpr uint32_t ClientPlayerJubaozhaiBrowseCommerceOffersIndex = 22;
#define ClientPlayerJubaozhaiBrowseCommerceOffersMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(22)

constexpr uint32_t ClientPlayerJubaozhaiListTradeMarketsMessageId = 305;
constexpr uint32_t ClientPlayerJubaozhaiListTradeMarketsIndex = 23;
#define ClientPlayerJubaozhaiListTradeMarketsMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(23)

constexpr uint32_t ClientPlayerJubaozhaiCreateRmbConsignmentMessageId = 333;
constexpr uint32_t ClientPlayerJubaozhaiCreateRmbConsignmentIndex = 24;
#define ClientPlayerJubaozhaiCreateRmbConsignmentMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(24)

constexpr uint32_t ClientPlayerJubaozhaiGetRmbConsignmentMessageId = 334;
constexpr uint32_t ClientPlayerJubaozhaiGetRmbConsignmentIndex = 25;
#define ClientPlayerJubaozhaiGetRmbConsignmentMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(25)

constexpr uint32_t ClientPlayerJubaozhaiListRmbConsignmentsMessageId = 335;
constexpr uint32_t ClientPlayerJubaozhaiListRmbConsignmentsIndex = 26;
#define ClientPlayerJubaozhaiListRmbConsignmentsMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(26)

constexpr uint32_t ClientPlayerJubaozhaiBuyRmbConsignmentMessageId = 336;
constexpr uint32_t ClientPlayerJubaozhaiBuyRmbConsignmentIndex = 27;
#define ClientPlayerJubaozhaiBuyRmbConsignmentMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(27)

constexpr uint32_t ClientPlayerJubaozhaiCancelRmbConsignmentMessageId = 337;
constexpr uint32_t ClientPlayerJubaozhaiCancelRmbConsignmentIndex = 28;
#define ClientPlayerJubaozhaiCancelRmbConsignmentMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(28)

constexpr uint32_t ClientPlayerJubaozhaiGetRmbOrderMessageId = 338;
constexpr uint32_t ClientPlayerJubaozhaiGetRmbOrderIndex = 29;
#define ClientPlayerJubaozhaiGetRmbOrderMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(29)

constexpr uint32_t ClientPlayerJubaozhaiListRmbOrdersMessageId = 339;
constexpr uint32_t ClientPlayerJubaozhaiListRmbOrdersIndex = 30;
#define ClientPlayerJubaozhaiListRmbOrdersMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(30)

constexpr uint32_t ClientPlayerJubaozhaiCancelRmbOrderMessageId = 340;
constexpr uint32_t ClientPlayerJubaozhaiCancelRmbOrderIndex = 31;
#define ClientPlayerJubaozhaiCancelRmbOrderMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(31)

constexpr uint32_t ClientPlayerJubaozhaiListSellerIncomesMessageId = 341;
constexpr uint32_t ClientPlayerJubaozhaiListSellerIncomesIndex = 32;
#define ClientPlayerJubaozhaiListSellerIncomesMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(32)

constexpr uint32_t ClientPlayerJubaozhaiCreateWithdrawalMessageId = 342;
constexpr uint32_t ClientPlayerJubaozhaiCreateWithdrawalIndex = 33;
#define ClientPlayerJubaozhaiCreateWithdrawalMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(33)

constexpr uint32_t ClientPlayerJubaozhaiGetWithdrawalMessageId = 343;
constexpr uint32_t ClientPlayerJubaozhaiGetWithdrawalIndex = 34;
#define ClientPlayerJubaozhaiGetWithdrawalMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(34)

constexpr uint32_t ClientPlayerJubaozhaiGetSellerCreditMessageId = 481;
constexpr uint32_t ClientPlayerJubaozhaiGetSellerCreditIndex = 35;
#define ClientPlayerJubaozhaiGetSellerCreditMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(35)

constexpr uint32_t ClientPlayerJubaozhaiSubmitSellerReviewMessageId = 482;
constexpr uint32_t ClientPlayerJubaozhaiSubmitSellerReviewIndex = 36;
#define ClientPlayerJubaozhaiSubmitSellerReviewMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(36)

constexpr uint32_t ClientPlayerJubaozhaiGetMarketReferencePriceMessageId = 483;
constexpr uint32_t ClientPlayerJubaozhaiGetMarketReferencePriceIndex = 37;
#define ClientPlayerJubaozhaiGetMarketReferencePriceMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(37)

constexpr uint32_t ClientPlayerJubaozhaiGetPlayerTradeQuotaMessageId = 484;
constexpr uint32_t ClientPlayerJubaozhaiGetPlayerTradeQuotaIndex = 38;
#define ClientPlayerJubaozhaiGetPlayerTradeQuotaMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(38)
