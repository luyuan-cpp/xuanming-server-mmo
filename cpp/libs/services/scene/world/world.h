#pragma once

#include <cstdint>

class World
{
public:
    static void InitializeSystemBeforeConnect();

    // 路由 node_id 分配成功后(Node::StartRpcServer 之后,SetAfterStart 里)调用:
    // buff / skill 这类临时 id 的 node 段用的是路由 node_id。以前这一步在
    // InitializeSystemBeforeConnect 里做,那时 etcd CAS 还没跑,node_id 恒为 0,
    // 于是每个 scene 节点的 buff/skill id 都种在同一个 node 段上。
    static void OnRoutingNodeIdAllocated(uint32_t nodeId);

    static void Update();
};