// battle 节点不消费任何 Kafka 命令 topic,也没有进程内 proto 事件订阅
// (上行全走 gRPC,出站全走 Kafka producer,设计文档 D6)。
// RegisterNodeEvents 是 node_entry.h 要求每个节点二进制提供的链接符号,
// 这里给出空实现。
void RegisterNodeEvents() {}
