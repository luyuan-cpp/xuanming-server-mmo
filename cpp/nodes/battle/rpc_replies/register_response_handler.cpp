// battle 节点没有 muduo TCP RPC 出站调用,也就没有任何异步应答 handler 需要注册。
// InitReply 是 Node::RegisterHandlers 要求每个节点二进制提供的链接符号。
void InitReply()
{
}
