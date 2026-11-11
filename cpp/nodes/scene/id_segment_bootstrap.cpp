#include "id_segment_bootstrap.h"
#include "id_segment_transport.h"

#include <array>
#include <chrono>
#include <cstddef>
#include <functional>
#include <optional>
#include <string>
#include <vector>

#include "muduo/base/Logging.h"
#include "muduo_grpc/channel.h"

#include "grpc_client/data_service/data_service_grpc_client.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "node/system/grpc_call_deadline.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "node/system/node/node.h"
#include "node_config_manager.h"
#include "proto/common/base/node.pb.h"
#include "proto/data_service/data_service.pb.h"
#include "thread_context/node_context_manager.h"
#include "utils/random/random.h"

namespace
{
    // "没有 READY 的 DataService 节点"日志节流:退避封顶 5s 时每次都记就是 12 行/分钟。
    constexpr uint32_t kNoNodeLogEveryN = 12;

    double MonotonicNowSec()
    {
        return std::chrono::duration<double>(std::chrono::steady_clock::now().time_since_epoch()).count();
    }

    // 所有种类共用的领段传输。单飞、迟到窗口、Close / Stop 的语义在 id_segment_transport.h,
    // 这里只装配生产的选路(READY 的 DataService 节点 + 生成的 gRPC 客户端)与单调时钟。
    thread_local id_segment_detail::Transport tlsIdSegmentTransport;
    // 连续挑不到 READY 节点的次数,只用于日志节流。
    thread_local uint32_t tlsNoNodeFailures = 0;

    // 只在候选里挑**挂着 muduo_grpc 通道且通道 READY** 的节点:ConnectToGrpcNode 先 emplace MuduoGrpcChannelPtr
    // 再 emplace NodeInfo,两者同在才是可用连接。DataService 是全局池(node_util.h
    // IsZoneScopedNodeType 注释:所有 zone 共享一个逻辑池),所以不比对 zone_id;多实例随机分摊领段压力。
    //
    // 为什么要看通道状态:通道不 READY 时发出去,TRANSIENT_FAILURE 立即换回 UNAVAILABLE;IDLE / CONNECTING 则排队等连接,
    // 连不上就占着共享路径直到 deadline 才失败。直接返回"发不出去",客户端按 500ms→5s 退避,冷启动时 data_service
    // 一连上就能领到段。
    // GetState(true) 顺手把 READY 连接断开后回到 IDLE 的通道踢回 CONNECTING(docs/design/grpc-muduo-transport.md §5)。
    entt::entity PickReadyDataServiceNode()
    {
        auto &registry = tlsNodeContextManager.GetRegistry(DataServiceNodeService);
        std::vector<entt::entity> ready;
        for (auto entity : registry.view<NodeInfo, MuduoGrpcChannelPtr>())
        {
            const auto &channel = registry.get<MuduoGrpcChannelPtr>(entity);
            if (channel && channel->GetState(/*tryToConnect=*/true) == muduo_grpc::ConnectivityState::kReady)
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

    // 生产的发送路径:挑一个 READY 的 DataService 节点,走生成的 gRPC 客户端(它连本地拒绝也延后交付完成)。
    // 挑不到返回空,传输当场回 false,客户端按退避重试。闭包只捕获节点实体值,不捕获组件指针。
    id_segment_detail::Transport::Fetcher PickDataServiceFetcher()
    {
        const entt::entity target = PickReadyDataServiceNode();
        if (target == entt::null)
        {
            ++tlsNoNodeFailures;
            if (tlsNoNodeFailures == 1 || tlsNoNodeFailures % kNoNodeLogEveryN == 0)
            {
                // 客户端自己按退避重试并节流记 ERROR;这里只补一句"缺什么",方便对着启动日志排障。
                LOG_WARN << "[idsegment] no READY DataServiceNodeService channel yet (attempt " << tlsNoNodeFailures
                         << "): is DataServiceNodeService.rpc in service_discovery_prefixes, does data_service "
                         << "register a NodeInfo under it, and is its gRPC port reachable?";
            }
            return {};
        }
        tlsNoNodeFailures = 0;
        return [target](const id_segment_detail::Transport::Request& request,
            std::function<void(id_segment_detail::Transport::FetchResult)> done) {
            data_service::SendDataServiceAllocateIdSegment(tlsNodeContextManager.GetRegistry(DataServiceNodeService),
                target, request, [done = std::move(done)](const GrpcCallOutcome& outcome, const auto* reply) {
                    done({outcome, reply ? std::optional{*reply} : std::nullopt});
                });
        };
    }

    // DataService 节点连上(NodeInfo 是 ConnectToGrpcNode 最后 emplace 的组件)就催一次首段。
    // 此刻通道多半还是 CONNECTING,Send 会返回 false,客户端 500ms 后重试即可命中 READY。
    void OnDataServiceNodeConnected(entt::registry & /*registry*/, entt::entity /*entity*/)
    {
        tlsGuidSegmentRegistry.WarmAll();
    }
} // namespace

void CloseGuidSegmentTransport() { tlsIdSegmentTransport.Close(); }

void StopGuidSegmentTransport() { tlsIdSegmentTransport.Stop(); }

std::size_t ConfigureGuidSegmentClients(Node &node)
{
    tlsIdSegmentTransport.Init(*node.GetLoop(), &PickDataServiceFetcher, &MonotonicNowSec);
    const IdSegmentConfig &config = gNodeConfigManager.GetBaseDeployConfig().id_segment();
    // 单次领段的兜底期限从 DataService 的 gRPC deadline 派生、比它宽 1s(上游比下游宽,
    // docs/design/grpc-muduo-transport.md §16.3):失败通知必须先于它到,
    // 否则持有者先放弃、共享路径进迟到窗口,随后到的失败 / 响应只能靠 abandoned_ 归属。
    // Node 构造时已 Apply,这里读到的就是生效值。
    const double fetchTimeoutSec =
        std::chrono::duration<double>(grpc_call_deadline::Get(DataServiceNodeService)).count() + 1.0;
    std::array<bool, kGuidKindCount> configured{};
    std::size_t enabled = 0;

    for (const auto &kindConfig : config.kinds())
    {
        GuidKind kind{};
        if (!ParseGuidKind(kindConfig.kind(), kind))
        {
            // 不 FATAL:滚动升级期间 yaml 可能已经写了新版本才认识的种类,旧二进制忽略即可。
            LOG_ERROR << "[idsegment] IdSegments entry with unknown Kind '" << kindConfig.kind()
                      << "' ignored (known: item / txlog / snapshot; add new kinds to GuidKind + kGuidKindNames)";
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
        options.fetchTimeoutSec = fetchTimeoutSec;
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
