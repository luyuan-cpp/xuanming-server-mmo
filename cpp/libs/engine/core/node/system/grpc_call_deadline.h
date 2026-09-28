#pragma once

#include <chrono>
#include <cstdint>
#include <string>
#include <vector>

#include "proto/common/base/config.pb.h"

// C++ 节点发出的 unary gRPC 调用的 deadline(docs/design/grpc-client-deadline-failure-callback.md §4)。
//
// 真源是 BaseDeployConfig.grpc_client(bin/etc/base_deploy_config.yaml 的 GrpcClient 块),按**目标**节点类型配;
// 没配到的类型用生成代码的 kDefaultGrpcCallDeadlineMs(grpc_client/grpc_call_tag.h)。
// Apply 把解析结果写进生成的客户端(SetGrpcCallDeadline),并留一份供业务查询:依赖某次调用结果的
// 看门狗 / 在途记录必须比 deadline 宽(上游比下游宽),一律从 Get 派生,不另写常数。
//
// 线程:Apply 在 Node::Initialize(事件循环线程)里、发出任何 gRPC 请求之前调一次;Get 任意线程可读。
namespace grpc_call_deadline
{
    struct Resolution
    {
        // 下标 = eNodeType 数值,值 = 毫秒;非法枚举值的槽位也填默认,调用方按 eNodeType_IsValid 过滤。
        std::vector<uint32_t> deadlineMsByNodeType;
        // 被忽略的配置项(不认识的键 / 0 = 不限时),每条一句说明;Apply 逐条记 ERROR。
        std::vector<std::string> rejected;
    };

    // 纯函数:按配置解析每种目标节点类型的 deadline,不碰任何全局状态。
    Resolution Resolve(const GrpcClientConfig &config);

    // 解析 → 写进生成的客户端 → 记下供 Get 查询;启动日志打一行生效值。重复调用以最后一次为准。
    void Apply(const GrpcClientConfig &config);

    // 发往 nodeType 类节点的 unary 调用当前生效的 deadline。Apply 之前、或 nodeType 越界时返回内置默认。
    std::chrono::milliseconds Get(uint32_t nodeType);
} // namespace grpc_call_deadline
