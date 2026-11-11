#pragma once

#include <cstdint>
#include <functional>
#include <optional>
#include <sstream>
#include <string>
#include <utility>

#include "core/async/coroutine_callback.h"
#include "core/async/loop_task_scope.h"
#include "core/network/grpc_client_call.h"
#include "id_segment_request_table.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "muduo/base/Logging.h"
#include "proto/data_service/data_service.pb.h"
#include <boost/cobalt/task.hpp>

namespace id_segment_detail
{
// 所有种类共用的 DataService 领段传输(docs/design/node-id-overhaul-plan-20260908.md §6 / §7.5)。
//
// 单飞:同一时刻只有一个在途请求。完成回调按闭包捕获的请求编号精确认领,不依赖响应字段或 metadata 回显。
// 持有者按 fetchTimeout 判失败之后,共享路径再守 RequestTable::kLateWindowSeconds 的迟到窗口,迟到的结果仍交给
// 发出它的那一种;窗口过期之后才到的结果计为孤儿丢弃,不会落到下一种身上。代价是各类冷启动串行领段,
// 运行期预取本来就稀疏,互不打扰。
// Close 只封新请求,已发出的结果照常交付(退出存盘期间仍可能用到已领的号);Stop 结束本地等待并清表,之后到的结果丢弃。
//
// 只在所属 loop 线程使用。选路与发送由装配层注入(PickFetcher):生产挑一个 READY 的 DataService 节点走 gRPC
// (id_segment_bootstrap.cpp),单测注入假实现,测的就是这份生产代码。
class Transport final
{
public:
    using Request = ::data_service::AllocateIdSegmentRequest;
    struct FetchResult
    {
        GrpcCallOutcome outcome;
        std::optional<::data_service::AllocateIdSegmentResponse> reply;
    };
    // 发出一次请求,之后恰好回调 done 一次(生成的 gRPC 客户端连本地拒绝也延后交付)。
    using Fetcher = std::function<void(const Request& request, std::function<void(FetchResult)> done)>;
    // 此刻可用的发送路径。返回空表示没有可用节点:Send 返回 false,客户端按退避重试,不占请求编号。
    using PickFetcher = std::function<Fetcher()>;
    // 单调时钟,秒。
    using Clock = std::function<double()>;

    Transport() = default;
    // 挂起的领段任务持有 this:析构前先结束本地等待,晚到的结果只能碰到已失效的弱状态。
    ~Transport() { Stop(); }
    Transport(const Transport&) = delete;
    Transport& operator=(const Transport&) = delete;

    void Init(muduo::net::EventLoop& loop, PickFetcher pick, Clock now)
    {
        loop_ = &loop;
        pick_ = std::move(pick);
        now_ = std::move(now);
        stopped_ = false;
        tasks_.emplace(loop);
    }

    void Close()
    {
        if (tasks_) tasks_->Close();
    }

    void Stop()
    {
        if (!tasks_) return;
        stopped_ = true;
        tasks_->Stop();
        tasks_.reset();
        requests_.Clear();
    }

    // GuidSegmentClient 的 SendFn:true = 已发出,结果经 OnResponse / OnTransportFailure 回到发出它的那一种;
    // false = 此刻发不出去(已关闭、共享路径忙或在迟到窗口内、没有可用节点),客户端按退避重试。
    bool Send(GuidKind kind, uint32_t step)
    {
        if (!tasks_ || !tasks_->IsAccepting()) return false;
        const double now = now_();
        using Admission = RequestTable::Admission;
        const auto activeKind = requests_.InFlightKind();
        const auto timeout = activeKind ? tlsGuidSegmentRegistry.Get(*activeKind).options().fetchTimeoutSec : 0.0;
        const auto gate = requests_.BeforeSend(now, timeout);
        if (gate.admission == Admission::kBusy) return false;
        if (gate.admission == Admission::kOpenedLateWindow)
        {
            LOG_WARN << "[idsegment] request seq=" << gate.affected->sequence << " kind=" << GuidKindName(gate.affected->kind)
                     << " unanswered for " << timeout << "s; holding the shared DataService path for "
                     << RequestTable::kLateWindowSeconds << "s so a late reply is still attributable";
            return false;
        }
        if (gate.admission == Admission::kExpiredLateWindow)
        {
            LOG_WARN << "[idsegment] giving up on a late reply for seq=" << gate.affected->sequence
                     << " kind=" << GuidKindName(gate.affected->kind) << " (window elapsed); resuming sends";
        }

        Fetcher fetcher = pick_();
        if (!fetcher) return false;

        Request request;
        request.set_biz_tag(tlsGuidSegmentRegistry.Get(kind).options().bizTag);
        request.set_step(step);
        const uint64_t requestSequence = requests_.Reserve(kind, now);
        if (tasks_->Start(FetchAsync(std::move(fetcher), std::move(request), requestSequence),
            "[idsegment] AllocateIdSegment")) return true;
        requests_.Take(requestSequence);
        return false;
    }

    // 完成时已认领不到请求(迟到窗口过期之后才到)而丢弃的结果数,只用于日志与单测。
    [[nodiscard]] uint64_t OrphanResponses() const { return orphanResponses_; }

private:
    boost::cobalt::task<void> FetchAsync(Fetcher fetcher, Request request, uint64_t requestSequence)
    {
        auto result = co_await loop_async::AwaitCallback<FetchResult>(*loop_, [&fetcher, &request](auto done) {
            fetcher(request, std::move(done));
            return true; // 发送路径承诺恰好回调一次,本地拒绝也延后交付。
        }, FetchResult{GrpcCallOutcome{kGrpcStatusUnavailable, "idsegment fetch rejected"}, std::nullopt});
        // 结果可能已由适配器认领，之后同一栈才停止传输并清表。
        if (result && !stopped_) OnCompleted(requestSequence, std::move(*result));
    }

    void OnCompleted(uint64_t requestSequence, FetchResult result)
    {
        const auto completed = requests_.Take(requestSequence);
        if (!completed)
        {
            ++orphanResponses_;
            LOG_WARN << "[idsegment] completion for unknown request seq=" << requestSequence
                     << " dropped; orphans=" << orphanResponses_;
            return;
        }
        const auto kind = completed->kind;

        if (result.outcome.ok() && result.reply)
        {
            tlsGuidSegmentRegistry.Get(kind).OnResponse(result.reply->error_code(), result.reply->lo(), result.reply->hi());
            return;
        }

        std::ostringstream why;
        why << "gRPC DataService.AllocateIdSegment code=" << result.outcome.code << " msg=" << result.outcome.message;
        if (completed->late)
        {
            // 持有者早已按 fetchTimeout 判过失败;这次调用到此确定不会再有响应,共享路径不必再为它守迟到窗口。
            LOG_WARN << "[idsegment] abandoned request seq=" << requestSequence << " kind=" << GuidKindName(kind)
                     << " finally failed (" << why.str() << "); releasing the shared DataService path";
            return;
        }
        // 客户端自己按连续失败次数节流记 ERROR,这里不再另记。
        tlsGuidSegmentRegistry.Get(kind).OnTransportFailure(why.str());
    }

    muduo::net::EventLoop* loop_ = nullptr;
    PickFetcher pick_;
    Clock now_;
    std::optional<LoopTaskScope> tasks_;
    RequestTable requests_;
    bool stopped_ = false;
    uint64_t orphanResponses_{0};
};
}
