#include "id_segment_bootstrap.h"

#include <array>
#include <chrono>
#include <cstddef>
#include <memory>
#include <optional>
#include <string>
#include <vector>

#include <grpcpp/grpcpp.h>
#include "muduo/base/Logging.h"

#include "grpc_client/data_service/data_service_grpc_client.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "node/system/node/node.h"
#include "node_config_manager.h"
#include "proto/common/base/node.pb.h"
#include "proto/data_service/data_service.pb.h"
#include "thread_context/node_context_manager.h"
#include "utils/encode/base64.h"
#include "utils/random/random.h"

namespace
{
    // 每个请求带一个客户端编号(gRPC metadata)。AllocateIdSegmentResponse 本身不回显 biz_tag /
    // 请求编号,所以今天的归属靠下面的"共享路径单飞 + FIFO";Go 侧若把这个 key 回显到
    // initial metadata(或响应里加回显字段),这里会自动改走精确归属,不用再改 C++。
    constexpr char kSeqMetadataKey[] = "x-idseg-seq";

    // 一次请求被它的持有者按 fetchTimeout 判失败之后,响应可能还在路上。这段窗口里共享路径
    // 不发新请求,于是窗口内到达的任何响应都能无歧义地还给那一次(号没浪费,归属也不会串)。
    // 窗口过了还没回来就放弃归属:一个 CAS 更新超过 3s + 10s 没回来,数据库已经病得够重,
    // 此时被 FIFO 误归属的概率已经很小;而且各种类的范围校验(lo ≥ 上一段 hi)还挡一层。
    constexpr double kLateResponseWindowSec = 10.0;

    // "没有 READY 的 DataService 节点"日志节流:退避封顶 5s 时每次都记就是 12 行/分钟。
    constexpr uint32_t kNoNodeLogEveryN = 12;

    double MonotonicNowSec()
    {
        return std::chrono::duration<double>(std::chrono::steady_clock::now().time_since_epoch()).count();
    }

    struct OutstandingRequest
    {
        uint64_t seq{0};
        GuidKind kind{GuidKind::kItem};
        uint32_t step{0};
        double sentAtSec{0};
    };

    // 所有种类共用的 DataService 传输层。
    //
    // 为什么单飞:生成的 handler 只拿到 (ClientContext, Response),响应里没有 biz_tag,
    // 发送侧也拿不到 ClientContext 的地址,所以同一时刻若有两种在途就分不清谁是谁。
    // 共享路径上同一时刻只放一个在途请求,响应必然属于它 —— 代价是三种同时冷启动时串行领三次
    // (毫秒级),运行期预取本来就稀疏,互不打扰。
    class IdSegmentTransport
    {
    public:
        bool Send(GuidKind kind, uint32_t step);
        void OnResponse(const grpc::ClientContext &ctx, const ::data_service::AllocateIdSegmentResponse &resp);

    private:
        static entt::entity PickReadyDataServiceNode();
        static bool SeqMatches(const std::optional<OutstandingRequest> &request, const std::string &echoed);

        std::optional<OutstandingRequest> inflight_;  // 共享路径上唯一的在途
        std::optional<OutstandingRequest> abandoned_; // 超时后仍可能回来的那一次
        double abandonedUntilSec_{0};
        uint64_t nextSeq_{0};
        uint64_t orphanResponses_{0};
        uint32_t noNodeFailures_{0};
    };

    thread_local IdSegmentTransport tlsIdSegmentTransport;

    // 只在候选里挑**已经建好 gRPC stub 且通道 READY** 的节点:ConnectToGrpcNode 先 emplace channel /
    // stub 再 emplace NodeInfo,三者同在才是可用连接。DataService 是全局池(node_util.h
    // IsZoneScopedNodeType 注释:所有 zone 共享一个逻辑池),所以不比对 zone_id;多实例随机分摊领段压力。
    //
    // 为什么要看通道状态:生成的客户端在 RPC 失败时只打日志、不回调,失败只能靠 fetchTimeout
    // 发现;data_service 整个不在时每次都白等 3s。通道不 READY 就直接返回"发不出去",客户端按
    // 500ms→5s 退避,冷启动时 data_service 一连上就能领到段。try_to_connect=true 顺手把
    // 空闲超时后 IDLE 的通道踢回 CONNECTING。
    entt::entity IdSegmentTransport::PickReadyDataServiceNode()
    {
        auto &registry = tlsNodeContextManager.GetRegistry(DataServiceNodeService);
        std::vector<entt::entity> ready;
        for (auto entity : registry.view<NodeInfo, data_service::DataServiceStubPtr, std::shared_ptr<grpc::Channel>>())
        {
            const auto &channel = registry.get<std::shared_ptr<grpc::Channel>>(entity);
            if (channel && channel->GetState(/*try_to_connect=*/true) == GRPC_CHANNEL_READY)
            {
                ready.push_back(entity);
            }
        }
        if (ready.empty())
        {
            return entt::null;
        }
        return ready[tlsRandom.Rand<std::size_t>(0, ready.size() - 1)];
    }

    bool IdSegmentTransport::Send(GuidKind kind, uint32_t step)
    {
        const double now = MonotonicNowSec();

        if (inflight_)
        {
            const double timeout = tlsGuidSegmentRegistry.Get(inflight_->kind).options().fetchTimeoutSec;
            if (now - inflight_->sentAtSec < timeout)
            {
                // 共享路径忙:调用方按退避重试(它自己的 sendUnavailable 计数会体现出来)。
                return false;
            }
            // 在途的那一次已经(或马上会)被它的持有者按 fetchTimeout 判失败;响应也许还在路上。
            abandoned_ = inflight_;
            abandonedUntilSec_ = now + kLateResponseWindowSec;
            inflight_.reset();
            LOG_WARN << "[idsegment] request seq=" << abandoned_->seq << " kind=" << GuidKindName(abandoned_->kind)
                     << " unanswered for " << timeout << "s; holding the shared DataService path for "
                     << kLateResponseWindowSec << "s so a late reply is still attributable";
        }
        if (abandoned_)
        {
            if (now < abandonedUntilSec_)
            {
                return false;
            }
            LOG_WARN << "[idsegment] giving up on a late reply for seq=" << abandoned_->seq
                     << " kind=" << GuidKindName(abandoned_->kind) << " (window elapsed); resuming sends";
            abandoned_.reset();
        }

        const entt::entity target = PickReadyDataServiceNode();
        if (target == entt::null)
        {
            ++noNodeFailures_;
            if (noNodeFailures_ == 1 || noNodeFailures_ % kNoNodeLogEveryN == 0)
            {
                // 客户端自己按退避重试并节流记 ERROR;这里只补一句"缺什么",方便对着启动日志排障。
                LOG_WARN << "[idsegment] no READY DataServiceNodeService channel yet (attempt " << noNodeFailures_
                         << "): is DataServiceNodeService.rpc in service_discovery_prefixes, does data_service "
                         << "register a NodeInfo under it, and is its gRPC port reachable?";
            }
            return false;
        }
        noNodeFailures_ = 0;

        ::data_service::AllocateIdSegmentRequest request;
        request.set_biz_tag(tlsGuidSegmentRegistry.Get(kind).options().bizTag);
        request.set_step(step);
        const uint64_t seq = ++nextSeq_;
        data_service::SendDataServiceAllocateIdSegment(tlsNodeContextManager.GetRegistry(DataServiceNodeService), target,
                                                       request, std::vector<std::string>{kSeqMetadataKey},
                                                       std::vector<std::string>{std::to_string(seq)});
        inflight_ = OutstandingRequest{seq, kind, step, now};
        return true;
    }

    // 生成的客户端把 metadata 值 Base64 后再发,服务端回显的可能是原文也可能是编码后的形态,两种都认。
    bool IdSegmentTransport::SeqMatches(const std::optional<OutstandingRequest> &request, const std::string &echoed)
    {
        if (!request)
        {
            return false;
        }
        const std::string raw = std::to_string(request->seq);
        return echoed == raw || echoed == Base64Encode(raw);
    }

    void IdSegmentTransport::OnResponse(const grpc::ClientContext &ctx,
                                        const ::data_service::AllocateIdSegmentResponse &resp)
    {
        std::optional<GuidKind> owner;

        // ① 精确归属:服务端若回显了请求编号,按它路由(今天的 Go 侧还没有,这是留给它的接口)。
        const auto &meta = ctx.GetServerInitialMetadata();
        if (const auto it = meta.find(kSeqMetadataKey); it != meta.end())
        {
            const std::string echoed(it->second.data(), it->second.size());
            if (SeqMatches(inflight_, echoed))
            {
                owner = inflight_->kind;
                inflight_.reset();
            }
            else if (SeqMatches(abandoned_, echoed))
            {
                owner = abandoned_->kind;
                abandoned_.reset();
            }
            else
            {
                ++orphanResponses_;
                LOG_WARN << "[idsegment] response with unknown seq echo '" << echoed << "' dropped (range [" << resp.lo()
                         << ", " << resp.hi() << ") wasted, harmless) orphans=" << orphanResponses_;
                return;
            }
        }
        // ② FIFO:共享路径单飞,响应属于唯一的在途;没有在途就属于超时后仍在等的那一次。
        else if (inflight_)
        {
            owner = inflight_->kind;
            inflight_.reset();
        }
        else if (abandoned_)
        {
            owner = abandoned_->kind;
            abandoned_.reset();
        }
        else
        {
            ++orphanResponses_;
            LOG_WARN << "[idsegment] response with no outstanding request dropped (range [" << resp.lo() << ", "
                     << resp.hi() << ") wasted, harmless) orphans=" << orphanResponses_;
            return;
        }

        tlsGuidSegmentRegistry.Get(*owner).OnResponse(resp.error_code(), resp.lo(), resp.hi());
    }

    // DataService 节点连上(NodeInfo 是 ConnectToGrpcNode 最后 emplace 的组件)就催一次首段。
    // 此刻通道多半还是 CONNECTING,Send 会返回 false,客户端 500ms 后重试即可命中 READY。
    void OnDataServiceNodeConnected(entt::registry & /*registry*/, entt::entity /*entity*/)
    {
        tlsGuidSegmentRegistry.WarmAll();
    }
} // namespace

void InitDataServiceReply()
{
    data_service::AsyncDataServiceAllocateIdSegmentHandler =
        [](const grpc::ClientContext &ctx, const ::data_service::AllocateIdSegmentResponse &resp)
    { tlsIdSegmentTransport.OnResponse(ctx, resp); };
}

std::size_t ConfigureGuidSegmentClients(Node & /*node*/)
{
    // 响应 handler 在这里装,不放进 rpc_replies/register_response_handler.cpp:那个文件是
    // proto 生成器产出的(每次 proto-gen-run 都会重写),手写进去的调用会被下一次生成抹掉。
    // 幂等:重复赋同一个 lambda 无副作用。
    InitDataServiceReply();

    const IdSegmentConfig &config = gNodeConfigManager.GetBaseDeployConfig().id_segment();
    std::array<bool, kGuidKindCount> configured{};
    std::size_t enabled = 0;

    for (const auto &kindConfig : config.kinds())
    {
        GuidKind kind{};
        if (!ParseGuidKind(kindConfig.kind(), kind))
        {
            // 不 FATAL:滚动升级期间 yaml 可能已经写了新版本才认识的种类,旧二进制忽略即可。
            LOG_ERROR << "[idsegment] IdSegments entry with unknown Kind '" << kindConfig.kind()
                      << "' ignored (known: item / txlog / snapshot; add new kinds to GUID_SEGMENT_KIND_LIST)";
            continue;
        }
        const auto index = static_cast<std::size_t>(kind);
        if (configured[index])
        {
            LOG_FATAL << "[idsegment] IdSegments configures Kind '" << kindConfig.kind()
                      << "' twice; fix bin/etc/base_deploy_config.yaml";
        }
        configured[index] = true;

        if (!kindConfig.enabled())
        {
            LOG_WARN << "[idsegment] kind=" << GuidKindName(kind)
                     << " Enabled=false: this guid kind has NO id source on this node (no snowflake fallback by "
                        "design, §7.5); every mint of it fails closed";
            continue;
        }

        GuidSegmentClient::Options options;
        options.kindName = GuidKindName(kind);
        options.bizTag = GuidKindName(kind);
        options.initialStep = kindConfig.initial_step();
        options.minStep = kindConfig.min_step();
        options.maxStep = kindConfig.max_step();
        // 传输 = 共享的 DataService 路径(按种类闭包);定时器 / 时钟留空 = 内置 TimerTaskComp / steady_clock。
        auto send = [kind](const std::string & /*bizTag*/, uint32_t step) { return tlsIdSegmentTransport.Send(kind, step); };
        if (!tlsGuidSegmentRegistry.Get(kind).Enable(std::move(options), std::move(send)))
        {
            // Enable 已经记了具体原因(典型:Enabled=true 但 InitialStep=0 / min>initial>max)。
            // 发号源配错不能带病启动:玩家进来后每次拾取都 fail-closed,比启动失败更难排查。
            LOG_FATAL << "[idsegment] IdSegments config for kind=" << GuidKindName(kind)
                      << " is invalid (see the error above); fix bin/etc/base_deploy_config.yaml";
        }
        ++enabled;
    }

    for (std::size_t i = 0; i < kGuidKindCount; ++i)
    {
        if (!configured[i])
        {
            LOG_WARN << "[idsegment] no IdSegments block for kind=" << GuidKindName(static_cast<GuidKind>(i))
                     << ": treated as disabled, every mint of it fails closed";
        }
    }

    // DataService 节点一连上就 Warm(WarmAll 幂等;SetAfterStart 里还会再催一次)。
    tlsNodeContextManager.GetRegistry(DataServiceNodeService)
        .on_construct<NodeInfo>()
        .connect<&OnDataServiceNodeConnected>();

    LOG_INFO << "[idsegment] " << enabled << " guid kind(s) enabled out of " << kGuidKindCount;
    return enabled;
}
