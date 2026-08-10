#pragma once

#include "core/utils/registry/game_registry.h"
#include "node/system/node/node_util.h"
#include "engine/core/type_define/type_define.h"
#include <cassert>

constexpr uint32_t kNodeTypeCount = eNodeType_ARRAYSIZE;

using NodeRegistries = std::array<entt::registry, kNodeTypeCount>;
using NodeGlobalEntity = std::array<entt::entity, kNodeTypeCount>;

// Thread-local manager for per-node-type ECS registries and global entities.
class NodeContextManager {
public:
    // globalEntities_ 必须显式填成 entt::null:std::array<entt::entity, N>
    // 默认构造不初始化元素,thread_local 的零初始化残留是 entity{0} ——
    // 而 64 位 entt 下 entt::null 是全 1,不是 0。此前
    // GetOrCreateGlobalEntity 的懒创建判断(== entt::null)因此永远不成立,
    // GetGlobalEntity 返回的是一个从未 create() 过的幽灵句柄,etcd 全部
    // gRPC 客户端状态(CQ/stub/watch 流)都 emplace 在它上面,仅靠
    // EnTT 池语义宽容才"碰巧能跑"。同目录 EcsContext 对各 globalEntity_
    // 逐一 { entt::null } 初始化,这里此前漏了。
    NodeContextManager() { globalEntities_.fill(entt::null); }
    NodeContextManager(const NodeContextManager&) = delete;
    NodeContextManager& operator=(const NodeContextManager&) = delete;

    entt::registry& GetRegistry(uint32_t nodeType) {
        assert(nodeType < kNodeTypeCount && "nodeType out of range");
        return registries_[nodeType];
    }

    NodeRegistries& GetAllRegistries() {
        return registries_;
    }

    void SetGlobalEntity(uint32_t nodeType, entt::entity entity) {
        assert(nodeType < kNodeTypeCount && "nodeType out of range");
        globalEntities_[nodeType] = entity;
    }

    entt::entity GetGlobalEntity(uint32_t nodeType) const {
        assert(nodeType < kNodeTypeCount && "nodeType out of range");
        return globalEntities_[nodeType];
    }

    // Returns existing or creates new global entity for this node type.
    entt::entity GetOrCreateGlobalEntity(uint32_t nodeType) {
        assert(nodeType < kNodeTypeCount && "nodeType out of range");
        if (globalEntities_[nodeType] == entt::null) {
            globalEntities_[nodeType] = GetRegistry(nodeType).create();
        }
        return globalEntities_[nodeType];
    }

    void Clear() {
        for (auto& registry : registries_) {
            registry.clear();
        }
        globalEntities_.fill(entt::null);
    }

private:
    NodeRegistries registries_;
    NodeGlobalEntity globalEntities_;
};

extern thread_local NodeContextManager tlsNodeContextManager;
