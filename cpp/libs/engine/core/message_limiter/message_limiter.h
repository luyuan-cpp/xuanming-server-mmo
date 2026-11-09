#pragma once

#include <unordered_map>
#include <boost/circular_buffer.hpp>

class MessageLimiter {
public:
    MessageLimiter() {}

    MessageLimiter(uint8_t defaultMaxRequests, uint32_t defaultTimeWindow);

    // 成功时记录本次请求并消耗窗口额度；返回成功码或拒绝的 tip 错误码。
    uint32_t TryConsumeRequestQuota(uint32_t messageId);
    
private:
    uint8_t defaultMaxRequests{3};  // max requests per window
    uint32_t defaultTimeWindow{1};   // window duration (seconds)
    std::unordered_map<uint32_t, boost::circular_buffer<uint64_t>> requestRecords;
};

