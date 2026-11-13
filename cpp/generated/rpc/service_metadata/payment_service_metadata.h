#pragma once
#include <cstdint>

#include "proto/payment/payment.pb.h"

constexpr uint32_t ClientPlayerPaymentListPayChannelsMessageId = 396;
constexpr uint32_t ClientPlayerPaymentListPayChannelsIndex = 0;
#define ClientPlayerPaymentListPayChannelsMethod  ::ClientPlayerPayment_Stub::descriptor()->method(0)

constexpr uint32_t ClientPlayerPaymentCreatePayIntentMessageId = 397;
constexpr uint32_t ClientPlayerPaymentCreatePayIntentIndex = 1;
#define ClientPlayerPaymentCreatePayIntentMethod  ::ClientPlayerPayment_Stub::descriptor()->method(1)

constexpr uint32_t ClientPlayerPaymentGetPayIntentMessageId = 398;
constexpr uint32_t ClientPlayerPaymentGetPayIntentIndex = 2;
#define ClientPlayerPaymentGetPayIntentMethod  ::ClientPlayerPayment_Stub::descriptor()->method(2)

constexpr uint32_t ClientPlayerPaymentSubmitPurchaseMessageId = 399;
constexpr uint32_t ClientPlayerPaymentSubmitPurchaseIndex = 3;
#define ClientPlayerPaymentSubmitPurchaseMethod  ::ClientPlayerPayment_Stub::descriptor()->method(3)
