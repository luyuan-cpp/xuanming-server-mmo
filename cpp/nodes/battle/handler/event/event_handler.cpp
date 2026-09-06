// battle 节点不消费任何 Kafka 命令 topic,也没有进程内 proto 事件订阅。
// RegisterNodeEvents 是 node_entry.h 要求每个节点二进制提供的链接符号,
// 这里给出空实现。
//
// 本文件不含任何 gRPC 代码。battle 的 gRPC 面在 handler/grpc/ 下,两个服务的
// 定位完全不同,别混为一谈(选型定谳见 docs/design/battle-transport-decision.md):
//   * battle_node.{h,cpp} —— match(Go)→ battle 的控制面,**长期保留**;
//   * battle_client_player_service.{h,cpp} —— gate 中继客户端消息,**过渡路径**,
//     等客户端第二条连接全量铺开后按 §18.7 收缩阶段删除。
void RegisterNodeEvents() {}
