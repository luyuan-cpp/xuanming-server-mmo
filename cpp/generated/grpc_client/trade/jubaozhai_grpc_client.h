#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/jubaozhai_service_metadata.h"

namespace trade {
#pragma region ClientPlayerJubaozhaiBrowseListings
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiBrowseListingsGrpcPath = "/trade.ClientPlayerJubaozhai/BrowseListings";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiBrowseListings(entt::registry& registry, entt::entity nodeEntity, const ::trade::BrowseListingsRequest& request, GrpcDone<::trade::BrowseListingsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiBrowseListingsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetListingDetail
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiGetListingDetailGrpcPath = "/trade.ClientPlayerJubaozhai/GetListingDetail";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiGetListingDetail(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetListingDetailRequest& request, GrpcDone<::trade::GetListingDetailResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiGetListingDetailErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiSetFavorite
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiSetFavoriteGrpcPath = "/trade.ClientPlayerJubaozhai/SetFavorite";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiSetFavorite(entt::registry& registry, entt::entity nodeEntity, const ::trade::SetFavoriteRequest& request, GrpcDone<::trade::SetFavoriteResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiSetFavoriteErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetMyShelf
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiGetMyShelfGrpcPath = "/trade.ClientPlayerJubaozhai/GetMyShelf";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiGetMyShelf(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetMyShelfRequest& request, GrpcDone<::trade::GetMyShelfResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiGetMyShelfErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiListOfficialSkus
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiListOfficialSkusGrpcPath = "/trade.ClientPlayerJubaozhai/ListOfficialSkus";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiListOfficialSkus(entt::registry& registry, entt::entity nodeEntity, const ::trade::ListOfficialSkusRequest& request, GrpcDone<::trade::ListOfficialSkusResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiListOfficialSkusErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiCreateOfficialOrder
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiCreateOfficialOrderGrpcPath = "/trade.ClientPlayerJubaozhai/CreateOfficialOrder";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiCreateOfficialOrder(entt::registry& registry, entt::entity nodeEntity, const ::trade::CreateOfficialOrderRequest& request, GrpcDone<::trade::CommerceOrderResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiCreateOfficialOrderErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetCommerceOrder
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiGetCommerceOrderGrpcPath = "/trade.ClientPlayerJubaozhai/GetCommerceOrder";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiGetCommerceOrder(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetCommerceOrderRequest& request, GrpcDone<::trade::CommerceOrderResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiGetCommerceOrderErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiCancelCommerceOrder
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiCancelCommerceOrderGrpcPath = "/trade.ClientPlayerJubaozhai/CancelCommerceOrder";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiCancelCommerceOrder(entt::registry& registry, entt::entity nodeEntity, const ::trade::CancelCommerceOrderRequest& request, GrpcDone<::trade::CommerceOrderResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiCancelCommerceOrderErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiListCommerceOrders
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiListCommerceOrdersGrpcPath = "/trade.ClientPlayerJubaozhai/ListCommerceOrders";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiListCommerceOrders(entt::registry& registry, entt::entity nodeEntity, const ::trade::ListCommerceOrdersRequest& request, GrpcDone<::trade::ListCommerceOrdersResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiListCommerceOrdersErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiCreatePlayerTrade
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiCreatePlayerTradeGrpcPath = "/trade.ClientPlayerJubaozhai/CreatePlayerTrade";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiCreatePlayerTrade(entt::registry& registry, entt::entity nodeEntity, const ::trade::CreatePlayerTradeRequest& request, GrpcDone<::trade::CommerceOrderResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiCreatePlayerTradeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiConfirmPlayerTrade
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiConfirmPlayerTradeGrpcPath = "/trade.ClientPlayerJubaozhai/ConfirmPlayerTrade";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiConfirmPlayerTrade(entt::registry& registry, entt::entity nodeEntity, const ::trade::ConfirmPlayerTradeRequest& request, GrpcDone<::trade::CommerceOrderResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiConfirmPlayerTradeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiCreateConsignment
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiCreateConsignmentGrpcPath = "/trade.ClientPlayerJubaozhai/CreateConsignment";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiCreateConsignment(entt::registry& registry, entt::entity nodeEntity, const ::trade::CreateConsignmentRequest& request, GrpcDone<::trade::CommerceOrderResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiCreateConsignmentErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiBuyConsignment
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiBuyConsignmentGrpcPath = "/trade.ClientPlayerJubaozhai/BuyConsignment";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiBuyConsignment(entt::registry& registry, entt::entity nodeEntity, const ::trade::BuyConsignmentRequest& request, GrpcDone<::trade::CommerceOrderResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiBuyConsignmentErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiCancelConsignment
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiCancelConsignmentGrpcPath = "/trade.ClientPlayerJubaozhai/CancelConsignment";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiCancelConsignment(entt::registry& registry, entt::entity nodeEntity, const ::trade::CancelConsignmentRequest& request, GrpcDone<::trade::CommerceOrderResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiCancelConsignmentErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiPlaceMarketOrder
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiPlaceMarketOrderGrpcPath = "/trade.ClientPlayerJubaozhai/PlaceMarketOrder";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiPlaceMarketOrder(entt::registry& registry, entt::entity nodeEntity, const ::trade::PlaceMarketOrderRequest& request, GrpcDone<::trade::MarketOrderResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiPlaceMarketOrderErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiCancelMarketOrder
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiCancelMarketOrderGrpcPath = "/trade.ClientPlayerJubaozhai/CancelMarketOrder";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiCancelMarketOrder(entt::registry& registry, entt::entity nodeEntity, const ::trade::CancelMarketOrderRequest& request, GrpcDone<::trade::MarketOrderResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiCancelMarketOrderErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiListMarketOrders
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiListMarketOrdersGrpcPath = "/trade.ClientPlayerJubaozhai/ListMarketOrders";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiListMarketOrders(entt::registry& registry, entt::entity nodeEntity, const ::trade::ListMarketOrdersRequest& request, GrpcDone<::trade::ListMarketOrdersResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiListMarketOrdersErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetMarketDepth
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiGetMarketDepthGrpcPath = "/trade.ClientPlayerJubaozhai/GetMarketDepth";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiGetMarketDepth(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetMarketDepthRequest& request, GrpcDone<::trade::GetMarketDepthResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiGetMarketDepthErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiCreateAuctionLot
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiCreateAuctionLotGrpcPath = "/trade.ClientPlayerJubaozhai/CreateAuctionLot";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiCreateAuctionLot(entt::registry& registry, entt::entity nodeEntity, const ::trade::CreateAuctionLotRequest& request, GrpcDone<::trade::AuctionLotResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiCreateAuctionLotErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiPlaceAuctionBid
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiPlaceAuctionBidGrpcPath = "/trade.ClientPlayerJubaozhai/PlaceAuctionBid";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiPlaceAuctionBid(entt::registry& registry, entt::entity nodeEntity, const ::trade::PlaceAuctionBidRequest& request, GrpcDone<::trade::AuctionLotResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiPlaceAuctionBidErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetAuctionLot
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiGetAuctionLotGrpcPath = "/trade.ClientPlayerJubaozhai/GetAuctionLot";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiGetAuctionLot(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetAuctionLotRequest& request, GrpcDone<::trade::AuctionLotResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiGetAuctionLotErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiCancelAuctionLot
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiCancelAuctionLotGrpcPath = "/trade.ClientPlayerJubaozhai/CancelAuctionLot";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiCancelAuctionLot(entt::registry& registry, entt::entity nodeEntity, const ::trade::CancelAuctionLotRequest& request, GrpcDone<::trade::AuctionLotResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiCancelAuctionLotErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiBrowseCommerceOffers
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiBrowseCommerceOffersGrpcPath = "/trade.ClientPlayerJubaozhai/BrowseCommerceOffers";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiBrowseCommerceOffers(entt::registry& registry, entt::entity nodeEntity, const ::trade::BrowseCommerceOffersRequest& request, GrpcDone<::trade::BrowseCommerceOffersResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiBrowseCommerceOffersErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiListTradeMarkets
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiListTradeMarketsGrpcPath = "/trade.ClientPlayerJubaozhai/ListTradeMarkets";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiListTradeMarkets(entt::registry& registry, entt::entity nodeEntity, const ::trade::ListTradeMarketsRequest& request, GrpcDone<::trade::ListTradeMarketsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiListTradeMarketsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiCreateRmbConsignment
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiCreateRmbConsignmentGrpcPath = "/trade.ClientPlayerJubaozhai/CreateRmbConsignment";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiCreateRmbConsignment(entt::registry& registry, entt::entity nodeEntity, const ::trade::CreateRmbConsignmentRequest& request, GrpcDone<::trade::RmbConsignmentResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiCreateRmbConsignmentErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetRmbConsignment
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiGetRmbConsignmentGrpcPath = "/trade.ClientPlayerJubaozhai/GetRmbConsignment";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiGetRmbConsignment(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetRmbConsignmentRequest& request, GrpcDone<::trade::RmbConsignmentResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiGetRmbConsignmentErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiListRmbConsignments
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiListRmbConsignmentsGrpcPath = "/trade.ClientPlayerJubaozhai/ListRmbConsignments";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiListRmbConsignments(entt::registry& registry, entt::entity nodeEntity, const ::trade::ListRmbConsignmentsRequest& request, GrpcDone<::trade::ListRmbConsignmentsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiListRmbConsignmentsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiBuyRmbConsignment
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiBuyRmbConsignmentGrpcPath = "/trade.ClientPlayerJubaozhai/BuyRmbConsignment";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiBuyRmbConsignment(entt::registry& registry, entt::entity nodeEntity, const ::trade::BuyRmbConsignmentRequest& request, GrpcDone<::trade::CommerceOrderResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiBuyRmbConsignmentErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiCancelRmbConsignment
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiCancelRmbConsignmentGrpcPath = "/trade.ClientPlayerJubaozhai/CancelRmbConsignment";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiCancelRmbConsignment(entt::registry& registry, entt::entity nodeEntity, const ::trade::CancelRmbConsignmentRequest& request, GrpcDone<::trade::RmbConsignmentResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiCancelRmbConsignmentErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetRmbOrder
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiGetRmbOrderGrpcPath = "/trade.ClientPlayerJubaozhai/GetRmbOrder";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiGetRmbOrder(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetCommerceOrderRequest& request, GrpcDone<::trade::CommerceOrderResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiGetRmbOrderErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiListRmbOrders
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiListRmbOrdersGrpcPath = "/trade.ClientPlayerJubaozhai/ListRmbOrders";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiListRmbOrders(entt::registry& registry, entt::entity nodeEntity, const ::trade::ListCommerceOrdersRequest& request, GrpcDone<::trade::ListCommerceOrdersResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiListRmbOrdersErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiCancelRmbOrder
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiCancelRmbOrderGrpcPath = "/trade.ClientPlayerJubaozhai/CancelRmbOrder";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiCancelRmbOrder(entt::registry& registry, entt::entity nodeEntity, const ::trade::CancelCommerceOrderRequest& request, GrpcDone<::trade::CommerceOrderResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiCancelRmbOrderErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiListSellerIncomes
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiListSellerIncomesGrpcPath = "/trade.ClientPlayerJubaozhai/ListSellerIncomes";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiListSellerIncomes(entt::registry& registry, entt::entity nodeEntity, const ::trade::ListSellerIncomesRequest& request, GrpcDone<::trade::ListSellerIncomesResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiListSellerIncomesErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiCreateWithdrawal
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiCreateWithdrawalGrpcPath = "/trade.ClientPlayerJubaozhai/CreateWithdrawal";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiCreateWithdrawal(entt::registry& registry, entt::entity nodeEntity, const ::trade::CreateWithdrawalRequest& request, GrpcDone<::trade::WithdrawalResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiCreateWithdrawalErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetWithdrawal
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiGetWithdrawalGrpcPath = "/trade.ClientPlayerJubaozhai/GetWithdrawal";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiGetWithdrawal(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetWithdrawalRequest& request, GrpcDone<::trade::WithdrawalResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiGetWithdrawalErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetSellerCredit
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiGetSellerCreditGrpcPath = "/trade.ClientPlayerJubaozhai/GetSellerCredit";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiGetSellerCredit(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetSellerCreditRequest& request, GrpcDone<::trade::SellerCreditResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiGetSellerCreditErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiSubmitSellerReview
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiSubmitSellerReviewGrpcPath = "/trade.ClientPlayerJubaozhai/SubmitSellerReview";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiSubmitSellerReview(entt::registry& registry, entt::entity nodeEntity, const ::trade::SubmitSellerReviewRequest& request, GrpcDone<::trade::SubmitSellerReviewResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiSubmitSellerReviewErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetMarketReferencePrice
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiGetMarketReferencePriceGrpcPath = "/trade.ClientPlayerJubaozhai/GetMarketReferencePrice";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiGetMarketReferencePrice(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetMarketReferencePriceRequest& request, GrpcDone<::trade::GetMarketReferencePriceResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiGetMarketReferencePriceErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetPlayerTradeQuota
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiGetPlayerTradeQuotaGrpcPath = "/trade.ClientPlayerJubaozhai/GetPlayerTradeQuota";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiGetPlayerTradeQuota(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetPlayerTradeQuotaRequest& request, GrpcDone<::trade::GetPlayerTradeQuotaResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiGetPlayerTradeQuotaErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiCreateWantedSkillBookOrder
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiCreateWantedSkillBookOrderGrpcPath = "/trade.ClientPlayerJubaozhai/CreateWantedSkillBookOrder";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiCreateWantedSkillBookOrder(entt::registry& registry, entt::entity nodeEntity, const ::trade::CreateWantedSkillBookOrderRequest& request, GrpcDone<::trade::WantedSkillBookOrderResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiCreateWantedSkillBookOrderErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiFulfillWantedSkillBookOrder
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiFulfillWantedSkillBookOrderGrpcPath = "/trade.ClientPlayerJubaozhai/FulfillWantedSkillBookOrder";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiFulfillWantedSkillBookOrder(entt::registry& registry, entt::entity nodeEntity, const ::trade::FulfillWantedSkillBookOrderRequest& request, GrpcDone<::trade::WantedSkillBookOrderResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiFulfillWantedSkillBookOrderErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerJubaozhaiListWantedSkillBookOrders
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerJubaozhaiListWantedSkillBookOrdersGrpcPath = "/trade.ClientPlayerJubaozhai/ListWantedSkillBookOrders";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerJubaozhaiListWantedSkillBookOrders(entt::registry& registry, entt::entity nodeEntity, const ::trade::ListWantedSkillBookOrdersRequest& request, GrpcDone<::trade::ListWantedSkillBookOrdersResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerJubaozhaiListWantedSkillBookOrdersErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetJubaozhaiCallDeadline(std::chrono::milliseconds deadline);

}// namespace trade
