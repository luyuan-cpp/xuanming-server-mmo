#include <memory>
#include <string>
#include <unordered_map>
#include "rpc/player_service_interface.h"

// battle 节点没有玩家实体,也没有 TCP 玩家消息入口,gPlayerService 保持空表。
// 定义与空实现是每个节点二进制的链接义务(参照 gate 的同名文件)。
std::unordered_map<std::string, std::unique_ptr<PlayerService>> gPlayerService;

void InitPlayerService()
{
}
