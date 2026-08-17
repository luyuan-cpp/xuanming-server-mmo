#include <memory>
#include <unordered_map>
#include "rpc/player_rpc_response_handler.h"

// battle 节点没有玩家消息的异步应答链路,gPlayerServiceReplied 保持空表。
// 定义与空实现是每个节点二进制的链接义务(参照 gate 的同名文件)。
std::unordered_map<std::string, std::unique_ptr<PlayerServiceReplied>> gPlayerServiceReplied;

void InitPlayerServiceReplied()
{
}
