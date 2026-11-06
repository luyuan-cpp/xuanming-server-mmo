#pragma once

#include <cstddef>

// 永久 guid 号段在 scene 节点上的接线(docs/design/node-id-overhaul-plan-20260908.md §6 / §7.5)。
//
// 客户端本体(modules/id_segment/guid_segment_client.h)不认识 gRPC 与节点发现;注册表
// (guid_segment_registry.h)只是按种类放实例。这里把它们接到 DataService.AllocateIdSegment:
//   * 发送 = 所有种类共用一条到 data_service 的 gRPC 路径(共享通道单飞,回调捕获编号),
//     从已连接且通道 READY 的 DataServiceNodeService 节点里随机挑一个发异步请求;
//   * 响应 = 每次调用回调带回本次请求编号,分发给发出它的那一种实例。

class Node;

// 按 BaseDeployConfig.id_segment 配置 tlsGuidSegmentRegistry 里的每一种 GUID(传输 + 内置定时器),
// 并挂上"DataService 节点连上即 WarmAll"的钩子。返回启用的种类数。
// 配置自相矛盾(Enabled=true 但 step 三元组非法、同一种类配了两块)直接 LOG_FATAL:
// 发号源配错了不该带病启动。没配 / Enabled=false 的种类 WARN 后按 fail-closed 跑。
std::size_t ConfigureGuidSegmentClients(Node &node);

// 停机时先关闭新领段，已提交的结果在退出存盘期间仍可交付；拆节点时停止本地等待。
void CloseGuidSegmentTransport();
void StopGuidSegmentTransport();
