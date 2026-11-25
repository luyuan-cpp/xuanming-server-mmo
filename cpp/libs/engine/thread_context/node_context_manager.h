#pragma once

#include "core/utils/registry/game_registry.h"
#include "node/system/node/node_util.h"
#include "engine/core/type_define/type_define.h"
#include <cassert>

constexpr uint32_t kNodeTypeCount = eNodeType_ARRAYSIZE;

using NodeRegistries = std::array<entt::registry, kNodeTypeCount>;
// 按节点类型分别持有 registry，只在所属线程使用。
class NodeContextManager {
public:
    NodeContextManager() = default;
    NodeContextManager(const NodeContextManager&) = delete;
    NodeContextManager& operator=(const NodeContextManager&) = delete;

    [[nodiscard]] static constexpr bool IsValidNodeType(uint32_t nodeType) {
        return nodeType < kNodeTypeCount;
    }

    // 只给可信的内部调用方:nodeType 是编译期常量,或本进程已经校验过的值。
    // 越界是编程错误,任何构建下都 LOG_FATAL 终止(fail-fast,与 RpcController::From 同口径)。
    // 以前只有 assert,Release 下 NDEBUG 把它抹掉,越界直接读写 registries_ 之外的内存
    // (ecs-entt-misuse-audit-20261015 F093/C007)。
    // 不返回替身 registry 让调用方照常往下跑:那样写路径(建 RpcClient、发起连接)的副作用照样发生,
    // 只是落进一个谁也不查的地方,还会让不同的非法类型互相查到对方写进去的实体,比当场终止更难发现。
    // 来自对端请求、etcd、Kafka 等外部输入的 nodeType 不要走这里,先用 TryGetRegistry 显式拒绝。
    entt::registry& GetRegistry(uint32_t nodeType) {
        RequireValidNodeType("GetRegistry", nodeType);
        return registries[nodeType];
    }

    // 给拿外部输入选 registry 的入口用:越界返回 nullptr,由调用方 LOG_ERROR 并拒绝本次请求。
    [[nodiscard]] entt::registry* TryGetRegistry(uint32_t nodeType) {
        return IsValidNodeType(nodeType) ? &registries[nodeType] : nullptr;
    }

    NodeRegistries& GetAllRegistries() {
        return registries;
    }

    void Clear() {
        for (auto& registry : registries) {
            registry.clear();
        }
    }

private:
    static void RequireValidNodeType(const char* caller, uint32_t nodeType) {
        if (!IsValidNodeType(nodeType)) {
            FailOutOfRange(caller, nodeType);
        }
    }

    // 终止逻辑放在 .cpp 里:本头文件被大量工程包含,不在这里引入 muduo 日志。
    [[noreturn]] static void FailOutOfRange(const char* caller, uint32_t nodeType);

    NodeRegistries registries;
};

extern thread_local NodeContextManager tlsNodeContextManager;
