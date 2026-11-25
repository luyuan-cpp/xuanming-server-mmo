#pragma once

// etcd 双向流重建的退避(docs/design/grpc-muduo-transport.md §10.2 第 1 条、§13.5),头文件内联以便单测。
//
// 0.2s 起、×2、封顶 5s;流活过 10s 再断视为「稳定过」,从 0.2s 重新开始;不设次数上限。
// 抖动(±20%)由调用方在返回的基准值上加,本类不持随机数源,结果确定、可重复测试。

#include <algorithm>
#include <chrono>

namespace etcd_transport
{
class StreamRebuildBackoff
{
public:
    static constexpr std::chrono::milliseconds kInitial{200};
    static constexpr std::chrono::milliseconds kMax{5000};
    static constexpr int kMultiplier = 2;
    static constexpr std::chrono::seconds kStableAfter{10};
    static constexpr double kJitter = 0.2;

    // 一条流结束时调用。lived = 它从开流到结束活了多久。返回这次重建前要等的基准时长,并推进下一次。
    std::chrono::milliseconds OnStreamEnded(std::chrono::steady_clock::duration lived)
    {
        if (lived >= kStableAfter)
        {
            next = kInitial;
        }
        const std::chrono::milliseconds delay = next;
        next = std::min(next * kMultiplier, kMax);
        return delay;
    }

private:
    std::chrono::milliseconds next{kInitial};
};
} // namespace etcd_transport
