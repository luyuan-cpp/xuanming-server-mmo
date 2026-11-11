#pragma once

#include <array>
#include <cstdint>
#include <string>

#include "proto/common/base/node.pb.h"
#include "proto/common/base/common.pb.h"

// 服务发现的本地快照,由 ServiceDiscoveryManager 独占持有,仅在所属 loop 线程读写。
// 节点以 node_uuid 区分;非法类型按空桶处理,查询不创建或改变任何状态。
class ServiceNodeSnapshot
{
public:
    using NodeList = google::protobuf::RepeatedPtrField<NodeInfo>;
    enum class UpsertResult : uint8_t { kAdded, kUpdated, kRejected };

    const NodeList &NodesOfType(uint32_t nodeType) const
    {
        static const NodeList empty;
        return nodeType < byType_.size() ? byType_[nodeType] : empty;
    }

    bool HasNodesOfType(uint32_t nodeType) const
    {
        return !NodesOfType(nodeType).empty();
    }

    // 这一类的成员每变一次(加入、更新、摘除)加一,从 0 起。按快照算派生结果的调用方(如 gate 的路由服子集)
    // 拿它判断缓存是否过期;非法类型恒为 0。
    uint64_t Generation(uint32_t nodeType) const
    {
        return nodeType < generations_.size() ? generations_[nodeType] : 0;
    }

    UpsertResult Upsert(uint32_t nodeType, const NodeInfo &node)
    {
        if (nodeType >= byType_.size())
        {
            return UpsertResult::kRejected;
        }
        ++generations_[nodeType];
        auto &nodes = byType_[nodeType];
        for (auto &existing : nodes)
        {
            if (existing.node_uuid() == node.node_uuid())
            {
                existing.CopyFrom(node);
                return UpsertResult::kUpdated;
            }
        }
        nodes.Add()->CopyFrom(node);
        return UpsertResult::kAdded;
    }

    // 删除全部匹配项,未命中或非法类型返回 0;不影响其他类型或 uuid。
    int Remove(uint32_t nodeType, const std::string &nodeUuid)
    {
        if (nodeType >= byType_.size())
        {
            return 0;
        }
        auto &nodes = byType_[nodeType];
        int removed = 0;
        for (int i = nodes.size() - 1; i >= 0; --i)
        {
            if (nodes.Get(i).node_uuid() == nodeUuid)
            {
                nodes.DeleteSubrange(i, 1);
                ++removed;
            }
        }
        if (removed > 0)
        {
            ++generations_[nodeType];
        }
        return removed;
    }

private:
    std::array<NodeList, common::base::eNodeType_ARRAYSIZE> byType_;
    std::array<uint64_t, common::base::eNodeType_ARRAYSIZE> generations_{};
};
