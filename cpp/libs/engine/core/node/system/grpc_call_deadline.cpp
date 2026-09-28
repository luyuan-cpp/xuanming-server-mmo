#include "node/system/grpc_call_deadline.h"

#include <atomic>
#include <iterator>
#include <sstream>

#include "muduo/base/Logging.h"
#include "grpc_client/grpc_call_tag.h"
#include "grpc_client/grpc_init_client.h"
#include "proto/common/base/node.pb.h"

namespace grpc_call_deadline
{
    namespace
    {
        // 与生成的客户端里的 deadline 一样是进程级全局(一个进程一份部署配置)。0 = 尚未 Apply,按默认。
        std::atomic<uint32_t> gEffectiveMs[common::base::eNodeType_ARRAYSIZE];
    } // namespace

    Resolution Resolve(const GrpcClientConfig &config)
    {
        Resolution resolution;
        resolution.deadlineMsByNodeType.assign(common::base::eNodeType_ARRAYSIZE, kDefaultGrpcCallDeadlineMs);
        for (const auto &entry : config.call_deadline_ms())
        {
            const std::string &name = entry.first;
            const uint32_t ms = entry.second;
            common::base::eNodeType nodeType{};
            if (!common::base::eNodeType_Parse(name, &nodeType))
            {
                // 不 FATAL:滚动升级时 yaml 可能先于二进制认识新的节点类型(与 IdSegments 的未知 Kind 同一口径)。
                resolution.rejected.push_back("unknown node type '" + name + "'");
                continue;
            }
            if (ms == 0)
            {
                resolution.rejected.push_back(name + " = 0 (no deadline violates the timeout budget)");
                continue;
            }
            resolution.deadlineMsByNodeType[static_cast<std::size_t>(nodeType)] = ms;
        }
        return resolution;
    }

    void Apply(const GrpcClientConfig &config)
    {
        const Resolution resolution = Resolve(config);
        for (const auto &why : resolution.rejected)
        {
            LOG_ERROR << "[GrpcClient] CallDeadlineMs entry ignored: " << why << "; that node type keeps the default "
                      << kDefaultGrpcCallDeadlineMs << "ms (fix bin/etc/base_deploy_config.yaml)";
        }

        std::ostringstream overrides;
        for (std::size_t index = 0; index < resolution.deadlineMsByNodeType.size(); ++index)
        {
            if (!common::base::eNodeType_IsValid(static_cast<int>(index)))
            {
                continue;
            }
            const uint32_t ms = resolution.deadlineMsByNodeType[index];
            gEffectiveMs[index].store(ms, std::memory_order_relaxed);
            SetGrpcCallDeadline(static_cast<uint32_t>(index), std::chrono::milliseconds(ms));
            if (ms != kDefaultGrpcCallDeadlineMs)
            {
                overrides << ' '
                          << std::string(common::base::eNodeType_Name(static_cast<common::base::eNodeType>(index)))
                          << '=' << ms;
            }
        }
        LOG_INFO << "[GrpcClient] unary call deadlines (ms): default=" << kDefaultGrpcCallDeadlineMs
                 << overrides.str();
    }

    std::chrono::milliseconds Get(uint32_t nodeType)
    {
        if (nodeType < std::size(gEffectiveMs))
        {
            if (const uint32_t ms = gEffectiveMs[nodeType].load(std::memory_order_relaxed); ms != 0)
            {
                return std::chrono::milliseconds(ms);
            }
        }
        return std::chrono::milliseconds(kDefaultGrpcCallDeadlineMs);
    }
} // namespace grpc_call_deadline
