// battle 节点不消费任何 Kafka 命令 topic,也没有进程内 proto 事件订阅。
// RegisterNodeEvents 是 node_entry.h 要求每个节点二进制提供的链接符号,
// 这里给出空实现。
//
// 本文件不含任何 gRPC 代码。battle 的 gRPC 面在 handler/grpc/ 下,只剩
// battle_node.{h,cpp} —— match(Go)→ battle 的控制面,**长期保留**
// (选型定谳见 docs/design/battle-transport-decision.md)。
// 原先的 battle_client_player_service.{h,cpp}(gate 中继客户端消息的过渡路径)已随
// 直连收缩删除(turn-based §22 D66/D67):客户端战斗消息只从直连面(client/battle_client_edge)进来。
void RegisterNodeEvents() {}
